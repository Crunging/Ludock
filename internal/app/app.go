// Package app wires Ludock's services together and runs the background
// tick.
package app

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Crunging/Ludock/internal/advanced"
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
	"github.com/Crunging/Ludock/internal/self"
	"github.com/Crunging/Ludock/internal/selfupdate"
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
	Advanced  *advanced.Service
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
	// SelfUpdate updates Ludock's own container; its Version is set by main.
	SelfUpdate *selfupdate.Service

	tick       sync.Mutex
	sampling   sync.Mutex
	background sync.WaitGroup
	storage    sync.Mutex
	copying    sync.Mutex
	saving     sync.Mutex
	delivering sync.Mutex
	// alertsFailing is the failing destinations as last seen by a delivery
	// pass, guarded by delivering.
	alertsFailing string
	polling       sync.Mutex
	checking      sync.Mutex
	measuring     sync.Mutex
	// deliverAgain and pollAgain ask the running pass for another.
	deliverAgain atomic.Bool
	pollAgain    atomic.Bool
	stop         chan struct{}
}

func New(cfg config.Config, database *db.DB, setup *auth.Setup) (*App, error) {
	clientIP := &auth.ClientIP{}
	settings := &advanced.Service{DB: database, ClientIP: clientIP, Env: cfg}
	settings.Apply(context.Background())
	client := docker.New(cfg.DockerSocket)
	table := locks.Default
	store := &identity.Store{DB: database}
	authorization := &authz.Service{DB: database, Servers: store}
	own := self.New(client, cfg.SelfContainer)
	observer := &servers.Observer{Docker: client, Choices: store, Own: own}
	serverService := &servers.Service{DB: database, Identity: store, Authz: authorization, Observer: observer}
	notifications := &notify.Service{DB: database}
	images := launch.NewImages(cfg.HelperImage, own)
	storage := backups.NewStorage(cfg.BackupRoots, own)
	storage.DevCopyRoot = cfg.DevCopyRoot
	backupService := &backups.Service{DB: database, Storage: storage, Docker: client, Observer: observer, Locks: table,
		Helpers: &backups.Helpers{Docker: client, Images: images, Storage: storage}}
	runner := &ops.Runner{DB: database, Locks: table, Queue: notifications.QueueIn, Alerted: notifications.Poke}
	monitoring := &monitor.Service{DB: database, Locks: table, Runner: runner, Queue: notifications.QueueIn, Alerted: notifications.Poke, Store: store}
	serverService.Looping = monitoring.Looping
	runtime := &console.Runtime{Docker: client, Images: images}
	files.SetSensitivePaths(cfg.SensitivePaths)
	updates := &update.Service{Docker: client, DockerConfig: cfg.DockerConfig}
	jobService := &jobs.Jobs{DB: database, Authz: authorization, Identity: store, Servers: serverService, Docker: client, Locks: table,
		Monitor: monitoring, Notify: notifications.Notify, Console: runtime, Backups: backupService, Updates: updates}
	checks := &update.Checks{Updates: updates, DB: database, Identity: store, Notify: notifications.Notify, Self: cfg.SelfContainer}
	backupService.CopyFailed = func(ctx context.Context, tx *sql.Tx, backupID, serverID, problem string) error {
		name := "a server"
		var display string
		if tx.QueryRowContext(ctx, "SELECT display_name FROM logical_servers WHERE id=?", serverID).Scan(&display) == nil {
			name = display
		}
		_, err := notifications.QueueIn(ctx, tx, notify.FailedWork, "backup-copy-failed:"+backupID,
			name+": the second copy of a backup couldn’t be made. "+problem+" Ludock keeps trying; its Backups tab shows which backup.")
		return err
	}
	backupService.DataFailed = func(ctx context.Context, tx *sql.Tx, key, message string) error {
		_, err := notifications.QueueIn(ctx, tx, notify.FailedWork, key, message)
		return err
	}
	backupService.Alerted = notifications.Poke
	jobService.Pulled = checks.Forget
	jobService.Register(runner)
	scheduleService := &schedules.Service{DB: database, Authz: authorization, Identity: store, Runner: runner, Servers: serverService,
		Locks: table, Queue: notifications.QueueIn, Alerted: notifications.Poke, Console: console.Broadcaster{Runtime: runtime}}
	a := &App{Config: cfg, DB: database, Docker: client, Locks: table, Identity: store, Authz: authorization,
		Auth: auth.NewService(database, authorization, setup), ClientIP: clientIP, Advanced: settings, Observer: observer, Servers: serverService,
		Images: images, Files: &files.Service{DB: database, Docker: client, Images: images, Own: own, Managed: observer.Managed}, Backups: backupService, Runner: runner, Jobs: jobService,
		Schedules: scheduleService, Monitor: monitoring, Notify: notifications, Console: runtime, Updates: updates, Checks: checks,
		History:   &history.Service{DB: database, Authz: authorization, Identity: store},
		Sessions:  &console.Sessions{DB: database, Authz: authorization, Identity: store, Servers: serverService, Locks: table, Runtime: runtime},
		Resources: &resources.Service{DB: database, Identity: store, Authz: authorization, Sample: serverService.Sample},
		stop:      make(chan struct{})}
	a.SelfUpdate = &selfupdate.Service{DB: database, Docker: client, Own: own, Updates: updates, Checks: checks, Locks: table,
		Notify: notifications.Notify, DataDir: cfg.DataDir, Socket: cfg.DockerSocket, MountID: cfg.MountID, DevSelfProof: cfg.DevSelfProof}
	a.Events = &events.Hub{Docker: client, Identity: store, Authz: authorization, Refresh: func(ctx context.Context) error {
		_, err := serverService.RefreshNow(ctx)
		return err
	}}
	monitoring.Changed = func(serverID string) { go a.Events.Changed(context.Background(), serverID) }
	runner.Changed = monitoring.Changed
	a.Players = &players.Service{Servers: serverService, Identity: store,
		Source:  &players.GameSource{Servers: serverService, Runtime: runtime, Locks: table},
		Changed: func(serverID string) { go a.Events.Changed(context.Background(), serverID) },
		Monitored: func(ctx context.Context) map[string]bool {
			monitored, _ := monitoring.Monitored(ctx)
			return monitored
		}}
	watchGames(monitoring, a.Players)
	monitoring.MeasureDisk = a.measureDisk
	jobService.Hung = func(serverID string, container servers.Container) (bool, int64) {
		return monitoring.Game(serverID, container) == monitor.GameNotAnswering, monitoring.LastAnswer(serverID, container)
	}
	a.Resources.Recorded = monitoring.CheckMemory
	a.Sessions.Commanded = func(serverID string) { a.Players.Check(context.Background(), serverID) }
	scheduleService.Players = schedules.ServerPlayers{Servers: serverService, Players: a.Players}
	jobService.Players = a.Players
	a.Attention = &attention.Service{DB: database, Authz: authorization, Identity: store, Monitor: monitoring, Schedules: scheduleService,
		Notify: notifications, Refresh: func(ctx context.Context) error {
			_, err := serverService.Refresh(ctx)
			return err
		}}
	a.Attention.Ludock = func(ctx context.Context) *attention.Item {
		failed := a.SelfUpdate.Failed(ctx)
		if failed == nil {
			return nil
		}
		return &attention.Item{ID: "ludock:" + failed.ID, Kind: "ludock", Since: failed.FinishedAt, Reason: failed.Reason, Version: failed.From,
			Data: failed.Data, Leftover: failed.Leftover, Unchanged: failed.State == selfupdate.StateFailed,
			Interrupted: failed.State == selfupdate.StateInterrupted, Finished: failed.Finished}
	}
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
			a.pollPlayers(ctx)
		}
	}
	// Requests wait for recovery to take its operations' locks; Start runs
	// before the server listens, so none can take them first.
	lift := a.Locks.Fence()
	// Likewise no work starts while an update of Ludock is still ending.
	a.SelfUpdate.Recover(ctx, ctx)
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
		for delay := time.Second; ; delay = min(2*delay, time.Minute) {
			err := recoverStartup()
			if err == nil {
				break
			}
			fields := logging.Cause(err, docker.StatusCode(err))
			if docker.Unavailable(err) && docker.StatusCode(err) == 0 {
				fields = append(fields, logging.F("docker", "unreachable"))
			}
			log.Error("Could not recover interrupted operations; retrying", fields...)
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
// backup storage setup, saving Ludock's data, notification delivery, and
// player readings can each wait on Docker, the network, or a game console,
// so they run beside it.
func (a *App) Tick(ctx context.Context) {
	a.runAlone(&a.tick, func() {
		// Monitoring and schedules judge the fleet as it is now, not as
		// a page read saw it a moment ago.
		current, err := a.Servers.RefreshNow(ctx)
		now := time.Now().UnixMilli()
		if err != nil {
			log.Warn("Discovery and schedules are temporarily unavailable", logging.Cause(err, docker.StatusCode(err))...)
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
	})
	a.runAlone(&a.storage, func() {
		if err := a.Backups.Initialize(ctx); err != nil {
			log.Warn("Automatic backup storage setup is temporarily unavailable")
		}
		if err := a.Backups.FinishDeletions(ctx); err != nil {
			log.Warn("Could not remove deleted backup archives; they will be retried")
		}
	})
	a.copyBackups(ctx)
	a.saveData(ctx)
	// Player counts are shown only on pages that follow server changes, and a
	// reading starts a helper, so a server no open page shows is read only
	// for the status query of a monitored one.
	a.pollPlayers(ctx)
	a.deliver(ctx)
}

// copyBackups makes due second copies of backups beside other work, once at
// a time, since a copy can take as long as the archive takes to write.
func (a *App) copyBackups(ctx context.Context) {
	a.runAlone(&a.copying, func() {
		if err := a.Backups.CopyDue(ctx); err != nil && ctx.Err() == nil {
			log.Warn("Could not make second copies of backups; they will be retried")
		}
		if err := a.Backups.CopyData(ctx); err != nil && ctx.Err() == nil {
			log.Warn("Could not record the second copy of Ludock's own data; it will be retried")
		}
	})
}

// saveData saves Ludock's own data into the backup folder when due, once at
// a time, so a slow folder never piles saves up.
func (a *App) saveData(ctx context.Context) {
	a.runAlone(&a.saving, func() {
		if err := a.Backups.SaveData(ctx); err != nil && ctx.Err() == nil {
			log.Warn("Could not record a save of Ludock's own data; it will be retried")
		}
	})
}

// pollPlayers reads players beside other work, once at a time. A call while
// a pass runs makes it run once more, so a page opened meanwhile is read
// without waiting for the next tick.
func (a *App) pollPlayers(ctx context.Context) {
	a.pollAgain.Store(true)
	if !a.polling.TryLock() {
		return
	}
	a.background.Add(1)
	go func() {
		defer a.background.Done()
		for {
			for a.pollAgain.Swap(false) {
				var watched func(string) bool
				if a.Events.Watched() {
					watched = a.Events.Follows
				}
				a.Players.Poll(ctx, watched)
			}
			a.polling.Unlock()
			// A call between the last pass and the unlock found the lock held.
			if !a.pollAgain.Load() || !a.polling.TryLock() {
				return
			}
		}
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
				a.noteAlertHealth(ctx)
			}
			a.delivering.Unlock()
			// A call between the last pass and the unlock found the lock held.
			if !a.deliverAgain.Load() || !a.delivering.TryLock() {
				return
			}
		}
	}()
}

// noteAlertHealth refreshes administrators' open pages when a destination
// starts or stops failing, which no server event announces. Only the
// delivering goroutine calls it.
func (a *App) noteAlertHealth(ctx context.Context) {
	problems, err := a.Notify.Problems(ctx)
	if err != nil {
		return
	}
	// The reason is shown, so a new one refreshes too.
	failing := ""
	for _, problem := range problems {
		failing += string(problem.Destination) + ": " + problem.Reason + "\n"
	}
	if failing != a.alertsFailing {
		a.alertsFailing = failing
		go a.Events.NotifyAdmins(context.Background())
	}
}

// checkImages looks for newer server images beside the tick, once at a time,
// since a registry can be slow to answer.
func (a *App) checkImages(ctx context.Context, current map[string]servers.Container, now int64) {
	if current == nil {
		return
	}
	a.runAlone(&a.checking, func() { a.Checks.Run(ctx, current, now) })
}

// record samples resources beside the tick, at most once per minute.
func (a *App) record(ctx context.Context, current map[string]servers.Container, now int64) {
	a.runAlone(&a.sampling, func() {
		if err := a.Resources.Record(ctx, current, now); err != nil {
			log.Warn("Could not record resource history; it will be recorded again next minute")
		}
	})
}

// runAlone runs work in the background unless the work mu guards is still
// running. Stop waits for it.
func (a *App) runAlone(mu *sync.Mutex, work func()) {
	if !mu.TryLock() {
		return
	}
	a.background.Add(1)
	go func() {
		defer a.background.Done()
		defer mu.Unlock()
		work()
	}()
}

// Stop ends background work and waits until deadline for running work,
// cleanup of console commands and file streams, and locks to settle. False
// means some was still running; it is left as a crash would leave it, and
// startup recovery resumes or reconciles it.
func (a *App) Stop(deadline time.Time) bool {
	close(a.stop)
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		// A delivery may be waiting on discovery, which outlasts its caller.
		a.Events.Stop()
		a.Runner.Stop()
		a.background.Wait()
		a.Schedules.Wait()
		a.Players.Stop()
		<-a.Locks.Drained()
		a.Files.Close()
		// Last: operations finishing above may still queue notifications.
		a.Notify.Stop()
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-settled:
		return true
	case <-timer.C:
		return false
	}
}

// watchGames has monitoring judge each game by what p reads of it.
func watchGames(monitoring *monitor.Service, p *players.Service) {
	monitoring.Game = func(serverID string, container servers.Container) monitor.GameAnswer {
		switch game, _ := p.Game(serverID, container); game {
		case players.NotChecked:
			return monitor.GameNotChecked
		case players.Waiting:
			// Docker has already run a health check, while a status query
			// may not have been asked since Ludock started.
			if container.HealthAnswers || p.Get(serverID, container) != nil {
				return monitor.GameWaiting
			}
		case players.Answering:
			return monitor.GameAnswering
		case players.NotAnswering:
			return monitor.GameNotAnswering
		}
		return monitor.GameUnknown
	}
	monitoring.LastAnswer = func(serverID string, container servers.Container) int64 {
		_, answered := p.Game(serverID, container)
		return answered
	}
	monitoring.Recall = p.Recall
}
