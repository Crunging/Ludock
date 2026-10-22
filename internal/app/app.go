// Package app wires Ludock's services together and runs the background
// tick.
package app

import (
	"context"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/attention"
	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/compose"
	"github.com/Crunging/Ludock/internal/config"
	"github.com/Crunging/Ludock/internal/console"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/events"
	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/history"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/jobs"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/monitor"
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/schedules"
	"github.com/Crunging/Ludock/internal/servers"
)

var log = logging.New("server")

// App holds every service.
type App struct {
	Config    config.Config
	DB        *db.DB
	Docker    *docker.Client
	Locks     *locks.Table
	Identity  *identity.Store
	Authz     *authz.Service
	Auth      *auth.Service
	ClientIP  *auth.ClientIP
	Observer  *servers.Observer
	Servers   *servers.Service
	Events    *events.Hub
	Images    *launch.Images
	Files     *files.Service
	Backups   *backups.Service
	Runner    *ops.Runner
	Jobs      *jobs.Jobs
	Schedules *schedules.Service
	Monitor   *monitor.Service
	Notify    *notify.Service
	History   *history.Service
	Attention *attention.Service
	Console   *console.Runtime
	Sessions  *console.Sessions
	Compose   *compose.Updates

	tick       sync.Mutex
	background sync.WaitGroup
	storage    sync.Mutex
	delivering sync.Mutex
	stop       chan struct{}
}

// New builds the services on an open database.
func New(cfg config.Config, database *db.DB, setup *auth.SetupWindow) (*App, error) {
	clientIP, err := auth.NewClientIP(cfg.TrustedProxies)
	if err != nil {
		return nil, err
	}
	client := docker.New(cfg.DockerSocket)
	table := locks.Default
	store := &identity.Store{DB: database}
	authorization := &authz.Service{DB: database, Servers: store}
	observer := &servers.Observer{Docker: client}
	serverService := &servers.Service{DB: database, Identity: store, Authz: authorization, Observer: observer}
	notifications := &notify.Service{DB: database}
	images := launch.NewImages(client, cfg.HelperImage, cfg.SelfContainer)
	storage := backups.NewStorage(client, cfg.BackupRoots, cfg.SelfContainer)
	backupService := &backups.Service{DB: database, Storage: storage, Docker: client, Observer: observer, Locks: table,
		Helpers: &backups.Helpers{Docker: client, Images: images, Storage: storage}}
	runner := &ops.Runner{DB: database, Locks: table, Notify: notifications.Notify}
	monitoring := &monitor.Service{DB: database, Locks: table, Notify: notifications.Notify, Store: store}
	runtime := &console.Runtime{Docker: client, Images: images}
	composeRunner := compose.NewRunner(cfg.DockerSocket, cfg.DockerConfig, cfg.ComposeRoots)
	files.SetSensitivePaths(cfg.SensitivePaths)
	updates := &compose.Updates{Snapshots: &compose.Snapshots{Runner: composeRunner, DB: database}, Docker: client, Locks: table}
	jobService := &jobs.Jobs{DB: database, Authz: authorization, Identity: store, Servers: serverService, Docker: client, Locks: table,
		Monitor: monitoring, Notify: notifications.Notify, Console: runtime, Backups: backupService, Updates: updates}
	jobService.Register(runner)
	scheduleService := &schedules.Service{DB: database, Authz: authorization, Identity: store, Runner: runner, Servers: serverService,
		Locks: table, Notify: notifications.Notify, Console: console.Broadcaster{Runtime: runtime}}
	a := &App{Config: cfg, DB: database, Docker: client, Locks: table, Identity: store, Authz: authorization,
		Auth: auth.NewService(database, authorization, setup), ClientIP: clientIP, Observer: observer, Servers: serverService,
		Images: images, Files: &files.Service{Docker: client, Images: images}, Backups: backupService, Runner: runner, Jobs: jobService,
		Schedules: scheduleService, Monitor: monitoring, Notify: notifications, Console: runtime, Compose: updates,
		History:  &history.Service{DB: database, Authz: authorization, Identity: store},
		Sessions: &console.Sessions{DB: database, Authz: authorization, Identity: store, Servers: serverService, Locks: table, Runtime: runtime},
		stop:     make(chan struct{})}
	a.Events = &events.Hub{Docker: client, Identity: store, Authz: authorization, Refresh: func(ctx context.Context) error {
		_, err := serverService.RefreshNow(ctx)
		return err
	}}
	a.Attention = &attention.Service{DB: database, Authz: authorization, Identity: store, Monitor: monitoring, Schedules: scheduleService,
		Refresh: func(ctx context.Context) error {
			_, err := serverService.Refresh(ctx)
			return err
		}}
	return a, nil
}

// Start removes leftover file helpers and reconciles interrupted operations,
// then runs the tick every 15 s. Recovery is retried with backoff until it
// succeeds, since no work may take locks while an earlier writer remains.
func (a *App) Start(ctx context.Context) {
	// Requests wait for recovery to take its operations' locks; Start runs
	// before the server listens, so none can take them first.
	lift := a.Locks.Fence()
	recoverStartup := func() error {
		// A file command interrupted by the restart may still be writing.
		if err := a.Files.RemoveLeftovers(ctx); err != nil {
			return err
		}
		return a.Runner.Start(ctx, lift)
	}
	a.background.Add(1)
	go func() {
		defer a.background.Done()
		for delay := time.Second; recoverStartup() != nil; delay = min(2*delay, time.Minute) {
			log.Error("Could not recover interrupted operations; retrying")
			select {
			case <-a.stop:
				return
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			a.Tick(ctx)
			select {
			case <-a.stop:
				return
			case <-ticker.C:
			}
		}
	}()
}

// Tick runs discovery, schedules, and monitoring once. Backup storage setup
// can wait on Docker and notification delivery can outlast a minute, so
// both run beside it.
func (a *App) Tick(ctx context.Context) {
	if a.tick.TryLock() {
		a.background.Add(1)
		go func() {
			defer a.background.Done()
			defer a.tick.Unlock()
			current, err := a.Servers.Refresh(ctx)
			now := time.Now().UnixMilli()
			if err != nil {
				log.Warn("Discovery and schedules are temporarily unavailable")
			} else {
				a.Schedules.Run(ctx, now)
				a.Schedules.Announce(ctx, now)
			}
			if err := a.Monitor.Check(ctx, current, now); err != nil {
				log.Warn("Availability checks are temporarily unavailable")
			}
		}()
	}
	if a.storage.TryLock() {
		a.background.Add(1)
		go func() {
			defer a.background.Done()
			defer a.storage.Unlock()
			if err := a.Backups.Initialize(ctx); err != nil {
				log.Warn("Automatic backup storage setup is temporarily unavailable")
			}
			if err := a.Backups.FinishDeletions(ctx); err != nil {
				log.Warn("Could not remove deleted backup archives; they will be retried")
			}
		}()
	}
	if a.delivering.TryLock() {
		a.background.Add(1)
		go func() {
			defer a.background.Done()
			defer a.delivering.Unlock()
			if err := a.Notify.Deliver(ctx); err != nil {
				log.Warn("Notification delivery is temporarily unavailable")
			}
		}()
	}
}

// Stop ends background work. Console commands and streamed file requests
// can outlive their connections; their locks and cleanup settle before the
// database closes.
func (a *App) Stop() {
	close(a.stop)
	a.Events.Stop()
	a.Runner.Stop()
	a.background.Wait()
	a.Schedules.Wait()
	select {
	case <-a.Locks.Drained():
	case <-time.After(5 * time.Minute):
		log.Error("Failed to shut down cleanly")
	}
	a.Files.Close()
	// Last: operations finishing above may still queue notifications.
	a.Notify.Stop()
}
