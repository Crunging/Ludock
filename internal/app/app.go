// Package app wires Ludock's services together and runs the background
// tick.
package app

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Crunging/Ludock/internal/attention"
	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
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
	"github.com/Crunging/Ludock/internal/players"
	"github.com/Crunging/Ludock/internal/resources"
	"github.com/Crunging/Ludock/internal/schedules"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/update"
)

var log = logging.New("server")

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
	Resources *resources.Service
	Console   *console.Runtime
	Sessions  *console.Sessions
	Updates   *update.Service
	Players   *players.Service

	tick       sync.Mutex
	sampling   sync.Mutex
	background sync.WaitGroup
	storage    sync.Mutex
	delivering sync.Mutex
	polling    sync.Mutex
	// deliverAgain asks the running delivery pass for another.
	deliverAgain atomic.Bool
	stop         chan struct{}
}

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
	files.SetSensitivePaths(cfg.SensitivePaths)
	updates := &update.Service{Docker: client, DockerConfig: cfg.DockerConfig}
	jobService := &jobs.Jobs{DB: database, Authz: authorization, Identity: store, Servers: serverService, Docker: client, Locks: table,
		Monitor: monitoring, Notify: notifications.Notify, Console: runtime, Backups: backupService, Updates: updates}
	jobService.Register(runner)
	scheduleService := &schedules.Service{DB: database, Authz: authorization, Identity: store, Runner: runner, Servers: serverService,
		Locks: table, Notify: notifications.Notify, Console: console.Broadcaster{Runtime: runtime}}
	a := &App{Config: cfg, DB: database, Docker: client, Locks: table, Identity: store, Authz: authorization,
		Auth: auth.NewService(database, authorization, setup), ClientIP: clientIP, Observer: observer, Servers: serverService,
		Images: images, Files: &files.Service{Docker: client, Images: images}, Backups: backupService, Runner: runner, Jobs: jobService,
		Schedules: scheduleService, Monitor: monitoring, Notify: notifications, Console: runtime, Updates: updates,
		History:   &history.Service{DB: database, Authz: authorization, Identity: store},
		Sessions:  &console.Sessions{DB: database, Authz: authorization, Identity: store, Servers: serverService, Locks: table, Runtime: runtime},
		Resources: &resources.Service{DB: database, Identity: store, Authz: authorization, Sample: serverService.Sample},
		stop:      make(chan struct{})}
	a.Events = &events.Hub{Docker: client, Identity: store, Authz: authorization, Refresh: func(ctx context.Context) error {
		_, err := serverService.RefreshNow(ctx)
		return err
	}}
	a.Players = &players.Service{Servers: serverService, Identity: store, Observer: observer, Runtime: runtime, Locks: table,
		Changed: func(serverID string) { go a.Events.Changed(context.Background(), serverID) }}
	a.Sessions.Commanded = func(serverID string) { a.Players.Check(context.Background(), serverID) }
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
		wait:
			for {
				select {
				case <-a.stop:
					return
				case <-ticker.C:
					break wait
				case <-a.Notify.Wake():
					// A notification was just queued; send it now.
					a.deliver(ctx)
				}
			}
		}
	}()
}

// Tick runs discovery, schedules, and monitoring once. Resource samples,
// backup storage setup, notification delivery, and player readings can each
// wait on Docker, the network, or a game console, so they run beside it.
func (a *App) Tick(ctx context.Context) {
	if a.tick.TryLock() {
		a.background.Add(1)
		go func() {
			defer a.background.Done()
			defer a.tick.Unlock()
			// Monitoring and schedules judge the fleet as it is now, not as
			// a page read saw it a moment ago.
			current, err := a.Servers.RefreshNow(ctx)
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
			a.record(ctx, current, now)
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
	if a.polling.TryLock() {
		a.background.Add(1)
		go func() {
			defer a.background.Done()
			defer a.polling.Unlock()
			a.Players.Poll(ctx)
		}()
	}
	a.deliver(ctx)
}

// deliver sends due notifications beside other work. A call while a pass
// runs makes that pass run once more, so nothing queued meanwhile waits for
// the next tick.
func (a *App) deliver(ctx context.Context) {
	a.deliverAgain.Store(true)
	if !a.delivering.TryLock() {
		return
	}
	a.background.Add(1)
	go func() {
		defer a.background.Done()
		for {
			for a.deliverAgain.Swap(false) {
				if err := a.Notify.Deliver(ctx); err != nil {
					log.Warn("Notification delivery is temporarily unavailable")
				}
			}
			a.delivering.Unlock()
			// A call between the last pass and the unlock found the lock held.
			if !a.deliverAgain.Load() || !a.delivering.TryLock() {
				return
			}
		}
	}()
}

// record samples resources beside the tick, at most once per minute.
func (a *App) record(ctx context.Context, current map[string]servers.Container, now int64) {
	if !a.sampling.TryLock() {
		return
	}
	a.background.Add(1)
	go func() {
		defer a.background.Done()
		defer a.sampling.Unlock()
		if err := a.Resources.Record(ctx, current, now); err != nil {
			log.Warn("Could not record resource history; it will be recorded again next minute")
		}
	}()
}

// Stop ends background work. Locks and cleanup of console commands and file
// streams settle before the database closes.
func (a *App) Stop() {
	close(a.stop)
	a.Events.Stop()
	a.Runner.Stop()
	a.background.Wait()
	a.Schedules.Wait()
	a.Players.Stop()
	select {
	case <-a.Locks.Drained():
	case <-time.After(5 * time.Minute):
		log.Error("Failed to shut down cleanly")
	}
	a.Files.Close()
	// Last: operations finishing above may still queue notifications.
	a.Notify.Stop()
}
