// Package app wires Ludock's services together and runs the background
// tick.
package app

import (
	"context"
	"database/sql"
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
	Checks    *update.Checks

	tick       sync.Mutex
	sampling   sync.Mutex
	background sync.WaitGroup
	storage    sync.Mutex
	copying    sync.Mutex
	delivering sync.Mutex
	polling    sync.Mutex
	checking   sync.Mutex
	measuring  sync.Mutex
	// deliverAgain asks the running delivery pass for another.
	deliverAgain atomic.Bool
	stop         chan struct{}
}

func New(cfg config.Config, database *db.DB, setup *auth.Setup) (*App, error) {
	clientIP, err := auth.NewClientIP(cfg.TrustedProxies)
	if err != nil {
		return nil, err
	}
	client := docker.New(cfg.DockerSocket)
	table := locks.Default
	store := &identity.Store{DB: database}
	authorization := &authz.Service{DB: database, Servers: store}
	observer := &servers.Observer{Docker: client, Choices: store}
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
	checks := &update.Checks{Updates: updates, DB: database, Identity: store, Notify: notifications.Notify}
	backupService.CopyFailed = func(ctx context.Context, tx *sql.Tx, backupID, serverID, problem string) error {
		name := "a server"
		var display string
		if tx.QueryRowContext(ctx, "SELECT display_name FROM logical_servers WHERE id=?", serverID).Scan(&display) == nil {
			name = display
		}
		_, err := notifications.QueueIn(ctx, tx, notify.FailedWork, "backup-copy-failed:"+backupID,
			"The second copy of a backup of "+name+" couldn’t be made: "+problem+" Ludock keeps trying; its Backups tab shows which backup.")
		return err
	}
	backupService.Alerted = notifications.Poke
	jobService.Pulled = checks.Forget
	jobService.Register(runner)
	scheduleService := &schedules.Service{DB: database, Authz: authorization, Identity: store, Runner: runner, Servers: serverService,
		Locks: table, Notify: notifications.Notify, Console: console.Broadcaster{Runtime: runtime}}
	a := &App{Config: cfg, DB: database, Docker: client, Locks: table, Identity: store, Authz: authorization,
		Auth: auth.NewService(database, authorization, setup), ClientIP: clientIP, Observer: observer, Servers: serverService,
		Images: images, Files: &files.Service{DB: database, Docker: client, Images: images, Managed: observer.Managed}, Backups: backupService, Runner: runner, Jobs: jobService,
		Schedules: scheduleService, Monitor: monitoring, Notify: notifications, Console: runtime, Updates: updates, Checks: checks,
		History:   &history.Service{DB: database, Authz: authorization, Identity: store},
		Sessions:  &console.Sessions{DB: database, Authz: authorization, Identity: store, Servers: serverService, Locks: table, Runtime: runtime},
		Resources: &resources.Service{DB: database, Identity: store, Authz: authorization, Sample: serverService.Sample},
		stop:      make(chan struct{})}
	a.Events = &events.Hub{Docker: client, Identity: store, Authz: authorization, Refresh: func(ctx context.Context) error {
		_, err := serverService.RefreshNow(ctx)
		return err
	}}
	monitoring.Changed = func(serverID string) { go a.Events.Changed(context.Background(), serverID) }
	runner.Changed = monitoring.Changed
	a.Players = &players.Service{Servers: serverService, Identity: store,
		Source:  &players.GameSource{Identity: store, Observer: observer, Runtime: runtime, Locks: table},
		Changed: func(serverID string) { go a.Events.Changed(context.Background(), serverID) },
		Monitored: func(ctx context.Context) map[string]bool {
			monitored, _ := monitoring.Monitored(ctx)
			return monitored
		}}
	monitoring.Game = func(serverID string, container servers.Container) monitor.GameAnswer {
		switch game, _ := a.Players.Game(serverID, container); game {
		case players.NotChecked:
			return monitor.GameNotChecked
		case players.Answering:
			return monitor.GameAnswering
		case players.NotAnswering:
			return monitor.GameNotAnswering
		}
		return monitor.GameUnknown
	}
	monitoring.MeasureDisk = a.measureDisk
	a.Resources.Recorded = monitoring.CheckMemory
	a.Sessions.Commanded = func(serverID string) { a.Players.Check(context.Background(), serverID) }
	scheduleService.Players = schedules.ServerPlayers{Servers: serverService, Players: a.Players}
	a.Attention = &attention.Service{DB: database, Authz: authorization, Identity: store, Monitor: monitoring, Schedules: scheduleService,
		Refresh: func(ctx context.Context) error {
			_, err := serverService.Refresh(ctx)
			return err
		}}
	return a, nil
}

// Start removes leftover file and console helpers and reconciles interrupted
// operations, then runs the tick every 15 s. Recovery is retried with backoff until it
// succeeds, since no work may take locks while an earlier writer remains.
func (a *App) Start(ctx context.Context) {
	// A page opened between ticks shows players without waiting for the next.
	a.Events.Watching = func() {
		select {
		case <-a.stop:
		default:
			a.pollPlayers(ctx, true)
		}
	}
	// Requests wait for recovery to take its operations' locks; Start runs
	// before the server listens, so none can take them first.
	lift := a.Locks.Fence()
	recoverStartup := func() error {
		// A file command interrupted by the restart may still be writing,
		// and a console helper may still be sending a command.
		if err := a.Files.RemoveLeftovers(ctx); err != nil {
			return err
		}
		if err := a.Console.RemoveLeftovers(ctx); err != nil {
			return err
		}
		if err := a.Files.RemoveStages(ctx, a.fileTarget); err != nil {
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
				case <-a.Backups.CopyWake():
					a.copyBackups(ctx)
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
			a.checkImages(ctx, current, now)
			a.checkDisks(ctx, current, now)
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
	a.copyBackups(ctx)
	// Player counts are shown only on pages that follow server changes, and a
	// reading can start a console helper, so nobody watching means no reading
	// except the status queries of monitored servers.
	a.pollPlayers(ctx, a.Events.Watched())
	a.deliver(ctx)
}

// copyBackups makes due second copies of backups beside other work, once at
// a time, since a copy can take as long as the archive takes to write.
func (a *App) copyBackups(ctx context.Context) {
	if !a.copying.TryLock() {
		return
	}
	a.background.Add(1)
	go func() {
		defer a.background.Done()
		defer a.copying.Unlock()
		if err := a.Backups.CopyDue(ctx); err != nil && ctx.Err() == nil {
			log.Warn("Could not make second copies of backups; they will be retried")
		}
	}()
}

// pollPlayers reads players beside other work, once at a time.
func (a *App) pollPlayers(ctx context.Context, watched bool) {
	if !a.polling.TryLock() {
		return
	}
	a.background.Add(1)
	go func() {
		defer a.background.Done()
		defer a.polling.Unlock()
		a.Players.Poll(ctx, watched)
	}()
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

// checkImages looks for newer server images beside the tick, once at a time,
// since a registry can be slow to answer.
func (a *App) checkImages(ctx context.Context, current map[string]servers.Container, now int64) {
	if current == nil || !a.checking.TryLock() {
		return
	}
	a.background.Add(1)
	go func() {
		defer a.background.Done()
		defer a.checking.Unlock()
		a.Checks.Run(ctx, current, now)
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
