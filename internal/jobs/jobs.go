// Package jobs implements the persisted server operations: start, stop,
// restart (optionally with a backup), backup, restore, image update, and
// scheduled console commands.
// Each acts only after rechecking its owner's current access and the
// server's binding, and records enough recovery state to reconcile an
// interruption instead of replaying it.
package jobs

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/console"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/monitor"
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/players"
	"github.com/Crunging/Ludock/internal/schedules"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/update"
)

var log = logging.New("jobs")

type Jobs struct {
	DB       *db.DB
	Authz    *authz.Service
	Identity *identity.Store
	Servers  *servers.Service
	Docker   *docker.Client
	Locks    *locks.Table
	Monitor  *monitor.Service
	Notify   func(alert notify.Alert, key, message string)
	Console  *console.Runtime
	Backups  *backups.Service
	Updates  *update.Service
	// Players reads who is online before a warning; it may be nil.
	Players *players.Service
	// Pulled runs after an update pulls a server's image, whether or not
	// it recreated the container, since the pull changes what the image
	// records about its registry version.
	Pulled func(serverID string)
	// SaveSettle is how long to wait after a save command; tests shorten it.
	SaveSettle time.Duration
	// ReplacementSettle is how long an updated container without a
	// healthcheck must keep running before it replaces the original; tests
	// shorten it.
	ReplacementSettle time.Duration
	// Poll is the reconciliation poll interval; tests shorten it.
	Poll time.Duration
	// WarningLead is how long an operation waits after warning players;
	// tests shorten it.
	WarningLead time.Duration
	// Hung reports whether a running server's game still doesn't answer,
	// which a recovery restart requires, and when it last answered. It may
	// be nil.
	Hung func(serverID string, container servers.Container) (bool, int64)

	runner *ops.Runner
}

func (j *Jobs) Register(queue *ops.Runner) {
	j.runner = queue
	queue.AfterRecovery = j.finishStoppedRestores
	runner := registrar{queue, j}
	for _, action := range []servers.Action{servers.Start, servers.Stop} {
		runner.Register(string(action), ops.Handler{
			Run: func(ctx context.Context, job *ops.Context) (*ops.Result, error) {
				if action == servers.Stop {
					if err := j.warnFirst(ctx, job, capabilityFor(action), false); err != nil {
						return nil, calledOff(job, err)
					}
				}
				result, err := j.runLifecycle(ctx, job, action)
				return warnedResult(job, result, err)
			},
			Recover: func(ctx context.Context, job *ops.Context) (*ops.Result, error) {
				if resumable(job) {
					return nil, ops.ErrResume
				}
				return j.recoverLifecycle(ctx, job)
			},
		})
	}
	runner.Register(ops.KindRestart, ops.Handler{
		Run: func(ctx context.Context, job *ops.Context) (*ops.Result, error) {
			if err := j.warnFirst(ctx, job, authz.ServerRestart, false); err != nil {
				return nil, calledOff(job, err)
			}
			if job.Job.Input["createBackup"] == true {
				// Past the wait, an interruption is recovered, which removes
				// the helper the check before the stop may leave.
				if warnRequested(job) {
					if err := job.Progress("validating", nil, nil); err != nil {
						return nil, calledOff(job, err)
					}
				}
				result, err := j.runBackupRestart(ctx, job)
				return warnedResult(job, result, err)
			}
			result, err := j.runLifecycle(ctx, job, servers.Restart)
			return warnedResult(job, result, err)
		},
		Recover: func(ctx context.Context, job *ops.Context) (*ops.Result, error) {
			if resumable(job) {
				return nil, ops.ErrResume
			}
			if job.Job.Input["createBackup"] == true {
				return j.recoverBackupRestart(ctx, job)
			}
			return j.recoverLifecycle(ctx, job)
		},
	})
	runner.Register(ops.KindBackup, ops.Handler{Run: j.warnedFirst(authz.BackupsCreate, j.runBackup), Recover: func(ctx context.Context, job *ops.Context) (*ops.Result, error) {
		if resumable(job) {
			return nil, ops.ErrResume
		}
		completed := j.backupCompleted(ctx, job)
		if !completed {
			defer j.notifyInterrupted(ctx, job, notify.FailedWork, "backup-failed:"+job.Job.ID, backupInterrupted)
		}
		outcome := func() *ops.Result {
			result := job.Job.Result
			result.Guidance = "Ludock restarted before the backup finished, so there’s no new backup."
			if completed {
				result.Guidance, result.BackupID = "The backup had finished.", job.Job.Recovery.BackupID
				withSave(job, &result)
			}
			return &result
		}
		if err := j.recoverBackup(ctx, job); err != nil {
			// The outcome keeps what is known of the backup.
			job.Progress(job.Job.Phase, nil, outcome())
			return nil, err
		}
		return withServerAfterRecovery(job, outcome()), nil
	}})
	// An interrupted command is reported as interrupted and never sent again.
	// It is sent on time or not at all, so it doesn't wait for Docker.
	runner.Register(ops.KindCommand, ops.Handler{Run: func(ctx context.Context, job *ops.Context) (*ops.Result, error) {
		return j.runCommand(docker.Promptly(ctx), job)
	}, Recover: func(context.Context, *ops.Context) (*ops.Result, error) {
		return &ops.Result{}, nil
	}})
	runner.Register(ops.KindRestore, ops.Handler{Run: func(ctx context.Context, job *ops.Context) (*ops.Result, error) {
		result, err := j.runRestore(ctx, job)
		return warnedResult(job, result, err)
	}, Recover: func(ctx context.Context, job *ops.Context) (*ops.Result, error) {
		if resumable(job) {
			return nil, ops.ErrResume
		}
		return j.recoverRestore(ctx, job)
	}})
	// Undoing a restore that stopped partway stops the server, which another
	// manager may have started, finishes the undo, and returns the server to
	// how it was.
	runner.Register(ops.KindUndoRestore, ops.Handler{Run: j.warnedFirst(authz.BackupsRestore, j.runUndoRestore), Recover: func(ctx context.Context, job *ops.Context) (*ops.Result, error) {
		if resumable(job) {
			return nil, ops.ErrResume
		}
		if err := j.recoverBackup(ctx, job); err != nil {
			return nil, err
		}
		result := job.Job.Result
		return withServerAfterRecovery(job, &result), nil
	}})
	runner.Register(ops.KindUpdate, ops.Handler{Run: func(ctx context.Context, job *ops.Context) (*ops.Result, error) {
		result, err := j.runUpdate(ctx, job)
		return warnedResult(job, result, err)
	}, Recover: func(ctx context.Context, job *ops.Context) (*ops.Result, error) {
		if resumable(job) {
			// A check before the stop may have left a helper.
			if err := j.removeOperationHelpers(ctx, job); err != nil {
				return nil, err
			}
			return nil, ops.ErrResume
		}
		return j.recoverUpdate(ctx, job)
	}})
}

func (j *Jobs) notify(alert notify.Alert, key, message string) {
	if j.Notify != nil {
		j.Notify(alert, key, message)
	}
}

// notifyInterrupted reports, on the next start, work that Ludock's restart
// cut short. Work stopped by shutdown alerts only here, so a crash and a
// restart send the same alert once.
func (j *Jobs) notifyInterrupted(ctx context.Context, job *ops.Context, alert notify.Alert, key, message string) {
	name := "A server"
	if logical, err := j.Identity.Get(ctx, job.Job.ServerID); err == nil && logical != nil && logical.DisplayName != "" {
		name = logical.DisplayName
	}
	j.notify(alert, key, name+": "+message)
}

const backupInterrupted = "Ludock restarted during a backup, so there’s no new backup. Its Activity says more."

func (j *Jobs) poll() time.Duration {
	if j.Poll > 0 {
		return j.Poll
	}
	return time.Second
}

// Recovery values come back from JSON, so numbers are float64.
func number(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, !math.IsNaN(typed) && !math.IsInf(typed, 0)
	case int64:
		return float64(typed), true
	case int:
		return float64(typed), true
	}
	return 0, false
}

func text(value any) (string, bool) {
	result, ok := value.(string)
	return result, ok
}

var (
	errRevoked         = apperr.New("ACCESS_REVOKED", 403, "The person who asked for this can no longer use the server")
	errScheduleRevoked = apperr.New("SCHEDULE_REVOKED", 403, "The schedule was deleted, disabled, or changed")
)

// Actor resolves the operation's owner now. Queued work from a schedule
// also requires the schedule to be unchanged and the owner to still manage
// schedules. A deleted owner fails closed.
func (j *Jobs) Actor(ctx context.Context, job *ops.Context) (*db.SessionUser, error) {
	return j.actor(ctx, nil, job)
}

// actor is Actor, through tx when it is set.
func (j *Jobs) actor(ctx context.Context, tx *sql.Tx, job *ops.Context) (*db.SessionUser, error) {
	var q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	} = j.DB
	find := j.DB.FindUserByID
	if tx != nil {
		q = tx
		find = func(ctx context.Context, id string) (*db.UserRecord, error) { return db.FindUserIn(ctx, tx, id) }
	}
	user, err := find(ctx, job.Job.ActorID)
	if err != nil {
		return nil, err
	}
	if user == nil || user.Disabled {
		return nil, errRevoked
	}
	actor := &db.SessionUser{ID: user.ID, Username: user.Username, Role: user.Role}
	scheduleID, scheduled := job.Job.Input["scheduleId"].(string)
	if !scheduled {
		return actor, nil
	}
	var owner, input string
	var revision int64
	err = q.QueryRowContext(ctx, "SELECT COALESCE(owner_id,''),input_json,revision FROM schedules WHERE id=? AND server_id=?",
		scheduleID, job.Job.ServerID).Scan(&owner, &input, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errScheduleRevoked
	}
	if err != nil {
		return nil, err
	}
	settings, err := schedules.ParseSaved(input)
	expected, _ := number(job.Job.Input["scheduleRevision"])
	if err != nil || owner != actor.ID || expected != float64(revision) || !settings.Enabled || settings.Action != job.Job.Kind ||
		settings.CreateBackup != (job.Job.Input["createBackup"] == true) {
		return nil, errScheduleRevoked
	}
	if tx != nil {
		err = j.Authz.AssertAccountIn(ctx, tx, actor.ID, job.Job.ServerID, authz.SchedulesManage)
	} else {
		_, err = j.Authz.Assert(ctx, actor, job.Job.ServerID, authz.SchedulesManage)
	}
	if err != nil {
		return nil, err
	}
	return actor, nil
}

// allowedIn is assertCapability through tx, for work whose authority must
// hold in the transaction that writes for it.
func (j *Jobs) allowedIn(ctx context.Context, tx *sql.Tx, job *ops.Context, capabilities ...authz.Capability) error {
	if job.Job.ActorID == ops.RecoveryActor {
		return j.recoveryAllowed(ctx, tx, job)
	}
	actor, err := j.actor(ctx, tx, job)
	if err != nil {
		return err
	}
	return j.Authz.AssertAccountIn(ctx, tx, actor.ID, job.Job.ServerID, capabilities...)
}

var errRecoveryOff = apperr.New("RECOVERY_OFF", 403, "Automatic restarts were turned off for this server, or it changed and awaits review")

// recoveryAllowed is the authority of a recovery restart: the server's
// monitoring policy still asks for it, and no change to the server awaits
// an administrator's review.
// It reads through tx when that is set.
func (j *Jobs) recoveryAllowed(ctx context.Context, tx *sql.Tx, job *ops.Context) error {
	if job.Job.Kind != ops.KindRestart || job.Job.Input["createBackup"] == true {
		return errRecoveryOff
	}
	getAvailability, getServer, getRecovery := j.Monitor.Get, j.Identity.Get, j.Monitor.Recovery
	if tx != nil {
		getAvailability = func(ctx context.Context, id string) (*monitor.Availability, error) {
			return j.Monitor.GetIn(ctx, tx, id)
		}
		getServer = func(ctx context.Context, id string) (*identity.Server, error) { return j.Identity.GetIn(ctx, tx, id) }
		getRecovery = func(ctx context.Context, id string) (monitor.Recovery, error) {
			return j.Monitor.RecoveryIn(ctx, tx, id)
		}
	}
	availability, err := getAvailability(ctx, job.Job.ServerID)
	if err != nil {
		return err
	}
	logical, err := getServer(ctx, job.Job.ServerID)
	if err != nil {
		return err
	}
	if !availability.Policy.RecoveryOn() || logical == nil || logical.SharingPaused {
		return errRecoveryOff
	}
	// Turning recovery off forgets the restart it queued, even if it is on
	// again by now.
	state, err := getRecovery(ctx, job.Job.ServerID)
	if err != nil {
		return err
	}
	if !state.Pending || state.OperationID != job.Job.ID {
		return errRecoveryOff
	}
	return nil
}

func (j *Jobs) resolve(ctx context.Context, job *ops.Context, capability authz.Capability, pinned bool) (*servers.Context, error) {
	revision := int64(-1)
	if pinned {
		revision = job.Job.BindingRevision
	}
	if job.Job.ActorID == ops.RecoveryActor {
		return j.Servers.ResolveChecked(ctx, job.Job.ServerID, revision, func(ctx context.Context) error { return j.recoveryAllowed(ctx, nil, job) })
	}
	actor, err := j.Actor(ctx, job)
	if err != nil {
		return nil, err
	}
	return j.Servers.ResolveAuthorized(ctx, actor, job.Job.ServerID, capability, revision)
}

func (j *Jobs) assertCapability(ctx context.Context, job *ops.Context, capability authz.Capability) error {
	if job.Job.ActorID == ops.RecoveryActor {
		return j.recoveryAllowed(ctx, nil, job)
	}
	actor, err := j.Actor(ctx, job)
	if err != nil {
		return err
	}
	_, err = j.Authz.Assert(ctx, actor, job.Job.ServerID, capability)
	return err
}

const forceStopGuidance = "Force-stopped, recent progress may not have been saved."

func lifecycleResult(recovery ops.Recovery) *ops.Result {
	result := &ops.Result{}
	if recovery.SaveStatus != "" {
		result.SaveStatus, result.SaveReason = recovery.SaveStatus, recovery.SaveReason
	}
	if recovery.StopRecoveryTimedOut {
		result.StopRecoveryTimedOut = true
		result.Guidance = "Shutdown could not be confirmed before the recovery deadline. Review the server state before retrying."
	}
	if recovery.ForceStopped {
		result.ForceStopped = true
		result.Guidance = forceStopGuidance
	}
	return result
}

// noteAvailability logs a monitoring state that could not be saved after Docker
// already changed.
func (j *Jobs) noteAvailability(err error) {
	if err != nil {
		log.Warn("Could not save a server's monitoring state; an outage alert may follow")
	}
}

// record keeps only fixed lifecycle summaries public; console output is
// discarded.
func record(job *ops.Context, phase string, change func(*ops.Recovery)) error {
	next := job.Job.Recovery
	if change != nil {
		change(&next)
	}
	return job.Progress(phase, func(r *ops.Recovery) { *r = next }, lifecycleResult(next))
}

// saveWorld sends the game's save command, best effort. Revoked authority
// still stops the job.
func (j *Jobs) saveWorld(ctx context.Context, job *ops.Context, server *servers.Context, assertAccess func() error) error {
	if server.Container.State == "restarting" {
		return record(job, "save_skipped", func(r *ops.Recovery) { r.SaveStatus, r.SaveReason = "skipped", "restarting" })
	}
	expected := games.ResolveAdapter(server.Container.GameType, map[string]string{})
	adapter := games.ResolveAdapter(server.Container.GameType, server.Container.Labels)
	command := ""
	if adapter != nil {
		for _, item := range adapter.Commands {
			if item.Label == "Save world" {
				command = item.Command
			}
		}
	}
	if command == "" {
		reason := "unsupported"
		if integration := games.Lookup(server.Container.GameType); integration != nil && integration.SavesOnStop {
			reason = "saves_on_stop"
		}
		if expected != nil && slices.ContainsFunc(expected.Commands, func(item games.QuickCommand) bool { return item.Label == "Save world" }) {
			reason = "console_unavailable"
		}
		return record(job, "save_skipped", func(r *ops.Recovery) { r.SaveStatus, r.SaveReason = "skipped", reason })
	}
	if server.Container.State != "running" {
		return record(job, "save_skipped", func(r *ops.Recovery) { r.SaveStatus, r.SaveReason = "skipped", "not_running" })
	}
	if err := record(job, "saving_world", nil); err != nil {
		return err
	}
	// Console transports have bounded deadlines; exec cleanup keeps this
	// operation's locks until Docker confirms termination.
	err := j.Console.GameCommand(ctx, console.Target{ContainerID: server.Container.ID, Labels: server.Container.Labels,
		State: server.Container.State}, adapter, command, console.Discard, assertAccess)
	if err != nil {
		if accessErr := assertAccess(); accessErr != nil {
			return accessErr
		}
		reason := "console_unavailable"
		if console.IsTimeout(err) {
			reason = "timed_out"
		}
		return record(job, "save_skipped", func(r *ops.Recovery) { r.SaveStatus, r.SaveReason = "skipped", reason })
	}
	if err := record(job, "waiting_for_save", func(r *ops.Recovery) { r.SaveStatus, r.SaveReason = "sent", "" }); err != nil {
		return err
	}
	settle := j.SaveSettle
	if settle == 0 {
		settle = 5 * time.Second
	}
	select {
	case <-time.After(settle):
	case <-ctx.Done():
		return ctx.Err()
	}
	return assertAccess()
}

func capabilityFor(action servers.Action) authz.Capability {
	return authz.Capability("server." + string(action))
}

func (j *Jobs) runLifecycle(ctx context.Context, job *ops.Context, action servers.Action) (*ops.Result, error) {
	permission := capabilityFor(action)
	initial, err := j.resolve(ctx, job, permission, true)
	if err != nil {
		return nil, err
	}
	recovery := job.Job.ActorID == ops.RecoveryActor
	return job.WithLocks(initial.LockKeys, func() (*ops.Result, error) {
		server, err := j.resolve(ctx, job, permission, true)
		if err != nil {
			return nil, err
		}
		if recovery && !j.stillHung(job, server) {
			return new(recoverySkipped), nil
		}
		var accessMu sync.Mutex
		var accessFailure error
		assertAccess := func() error {
			accessMu.Lock()
			failure := accessFailure
			accessMu.Unlock()
			if failure != nil {
				return failure
			}
			if err := j.assertCapability(ctx, job, permission); err != nil {
				return err
			}
			_, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, server.Observation, job.Job.BindingRevision)
			return err
		}
		watch, stopWatch := context.WithCancel(ctx)
		defer stopWatch()
		go func() {
			ticker := time.NewTicker(j.poll())
			defer ticker.Stop()
			for {
				select {
				case <-watch.Done():
					return
				case <-ticker.C:
					if _, err := j.resolve(watch, job, permission, true); err != nil && watch.Err() == nil {
						accessMu.Lock()
						if accessFailure == nil {
							accessFailure = err
						}
						accessMu.Unlock()
					}
				}
			}
		}()
		mutate := func(mutation servers.Action) error {
			if err := assertAccess(); err != nil {
				return err
			}
			if server, err = j.resolve(ctx, job, permission, true); err != nil {
				return err
			}
			if err := assertAccess(); err != nil {
				return err
			}
			// Someone stopped the server meanwhile; it stays down.
			if recovery && mutation == servers.Stop && !j.stillHung(job, server) {
				return errRecoveryStopped
			}
			phase := "starting"
			if mutation == servers.Stop {
				phase = "stopping"
			}
			// Persist before dispatch, so recovery recognizes a change that
			// reached Docker.
			if err := record(job, phase, func(r *ops.Recovery) { r.ContainerID = server.Container.ID }); err != nil {
				return err
			}
			change := j.Servers.Observer.ChangeState
			if recovery {
				change = j.Servers.Observer.ChangeStateExact
			}
			err := change(ctx, server.Container.ID, mutation, func(observation identity.Observation, info *docker.ContainerInspect) error {
				if err := j.assertCapability(ctx, job, permission); err != nil {
					return err
				}
				if _, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, observation, job.Job.BindingRevision); err != nil {
					return err
				}
				// Only the run judged hung is stopped and started again: one
				// stopped, or started anew, by someone else stays as they left it.
				if recovery && (!sameRun(job, info) || (mutation == servers.Stop && !info.State.Running)) {
					return errRecoveryStopped
				}
				if mutation == servers.Stop {
					// Recovery takes these locks before checking authority again.
					return record(job, "stopping", func(r *ops.Recovery) {
						recordStop(r, info)
						r.LockKeys = server.LockKeys
					})
				}
				return nil
			})
			if recovery && errors.Is(err, servers.ErrUnchanged) {
				return errRecoveryStopped
			}
			return err
		}
		// Pause outage tracking before acting, or the change could alert,
		// and only for work that may still go ahead.
		if err := j.suppressFor(job)(ctx, server.Logical.ID, func(tx *sql.Tx) error {
			return j.allowedIn(ctx, tx, job, permission)
		}); err != nil {
			return nil, err
		}
		// server is nil after a failed resolve, so the ID is kept apart.
		serverID := server.Logical.ID
		defer j.suppressAfter(ctx, job, serverID)
		if action != servers.Start {
			if err := j.saveWorld(ctx, job, server, assertAccess); err != nil {
				return nil, err
			}
			if err := mutate(servers.Stop); errors.Is(err, errRecoveryStopped) {
				return new(recoverySkipped), nil
			} else if err != nil {
				return nil, err
			}
			// Persist the actual stop before a restart clears Docker's exit code.
			j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, server.Logical.ID, action == servers.Stop))
			if server, err = j.resolve(ctx, job, permission, true); err != nil {
				return nil, err
			}
			if err := record(job, "verifying_stop", func(r *ops.Recovery) {
				r.StopSettled = true
				r.ForceStopped = r.StopStarted && !r.Restarting && server.Container.Exit != nil && server.Container.Exit.Code == 137
			}); err != nil {
				return nil, err
			}
		}
		if action != servers.Stop {
			if err := mutate(servers.Start); errors.Is(err, errRecoveryStopped) {
				return new(recoverySkipped), nil
			} else if err != nil {
				return nil, err
			}
			j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, server.Logical.ID, false))
		}
		if err := assertAccess(); err != nil {
			return nil, err
		}
		return lifecycleResult(job.Job.Recovery), nil
	})
}

// suppressFor is how a lifecycle action pauses outage tracking: an automatic
// restart announces its own outcome, so it ends the outage it acts on.
func (j *Jobs) suppressFor(job *ops.Context) func(context.Context, string, func(*sql.Tx) error) error {
	if job.Job.ActorID == ops.RecoveryActor {
		return j.Monitor.SuppressRecovered
	}
	return j.Monitor.Suppress
}

func (j *Jobs) suppressAfter(ctx context.Context, job *ops.Context, serverID string) {
	j.noteAvailability(j.suppressFor(job)(context.WithoutCancel(ctx), serverID, nil))
}

var (
	recoverySkipped    = ops.Result{Skipped: true, Guidance: "Skipped: the game answered again, or the server was no longer running"}
	errRecoveryStopped = errors.New("server stopped before its recovery restart")
)

// sameRun reports whether a container's latest run is the one a recovery
// restart was queued for; a stop keeps its start time.
func sameRun(job *ops.Context, info *docker.ContainerInspect) bool {
	run, ok := number(job.Job.Input["run"])
	started, err := time.Parse(time.RFC3339Nano, info.State.StartedAt)
	return ok && err == nil && float64(started.UnixMilli()) == run
}

// stillHung confirms right before a recovery restart that the container run
// judged hung still runs and its game still hasn't answered since the
// silence judged: an answer starts a new silence, with its own wait.
func (j *Jobs) stillHung(job *ops.Context, server *servers.Context) bool {
	run, ok := number(job.Job.Input["run"])
	answered, known := number(job.Job.Input["answered"])
	if !ok || !known || run != float64(server.Container.StateSince) || server.Container.State != "running" || j.Hung == nil {
		return false
	}
	hung, last := j.Hung(server.Logical.ID, server.Container)
	return hung && float64(last) == answered
}

// awaitStopDeadline holds the locks until a dispatched stop's saved deadline
// has passed, even after authority or the binding changed, then returns err.
func (j *Jobs) awaitStopDeadline(ctx context.Context, job *ops.Context, unsettled bool, err error) error {
	deadline := job.Job.Recovery.StopRecoveryDeadlineAt
	if !unsettled || deadline == 0 {
		return err
	}
	if wait := time.Until(time.UnixMilli(deadline)); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func recordStop(r *ops.Recovery, info *docker.ContainerInspect) {
	state := servers.StopRecoveryState(info, time.Now())
	r.StopStarted, r.StopRecoveryDeadlineAt, r.StopInitialStartedAt = state.StopStarted, state.StopRecoveryDeadlineAt, state.StopInitialStartedAt
	r.Restarting = info.State.Restarting
}

// forcedStop reports whether a stop Ludock sent ended in Docker's kill. A
// container restarting when the stop was sent keeps its game's own exit,
// unless Docker started another run before the stop reached it.
func forcedStop(r *ops.Recovery, stopped *docker.ContainerInspect) bool {
	if !r.StopStarted || (stopped.State.ExitCode != 137 && !stopped.State.OOMKilled) {
		return false
	}
	started, err := time.Parse(time.RFC3339Nano, stopped.State.StartedAt)
	return !r.Restarting || r.StopInitialStartedAt == nil || err != nil || started.UnixMilli() != *r.StopInitialStartedAt
}

// recoverLifecycle only observes: Docker may still be handling the
// original stop, so the lock is kept until it settles, without starting or
// stopping again.
func (j *Jobs) recoverLifecycle(ctx context.Context, job *ops.Context) (*ops.Result, error) {
	permission := capabilityFor(servers.Action(job.Job.Kind))
	if job.Job.Kind != ops.KindStart && job.Job.Kind != ops.KindStop && job.Job.Kind != ops.KindRestart {
		return nil, apperr.New("INVALID_OPERATION_STATE", 409, "Invalid lifecycle action")
	}
	recovery := job.Job.Recovery
	unsettled := recovery.StopStarted && !recovery.StopSettled
	// A dispatched stop may still be running in Docker, whatever its owner's
	// authority is now: its saved locks are taken first.
	held := recovery.LockKeys
	if !unsettled {
		initial, err := j.resolve(ctx, job, permission, true)
		if err != nil {
			return nil, err
		}
		held = initial.LockKeys
	}
	release, err := j.Locks.AcquireForRecovery(held)
	if err != nil {
		return nil, err
	}
	defer release()
	server, err := j.resolve(ctx, job, permission, true)
	if err != nil {
		return nil, j.awaitStopDeadline(ctx, job, unsettled, err)
	}
	defer j.suppressAfter(ctx, job, server.Logical.ID)
	if unsettled && recovery.ContainerID == server.Container.ID {
		deadline, initialStarted := recovery.StopRecoveryDeadlineAt, recovery.StopInitialStartedAt
		if err := record(job, "reconciling_stop", nil); err != nil {
			return nil, err
		}
		for server.Container.Exit == nil {
			// A different running instance proves the old shutdown is over.
			if server.Container.State == "running" && initialStarted != nil && server.Container.StateSince != 0 &&
				server.Container.StateSince != *initialStarted {
				j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, server.Logical.ID, false))
				break
			}
			if deadline == 0 {
				return nil, apperr.New("INVALID_OPERATION_STATE", 409, "Saved stop recovery deadline is invalid")
			}
			remaining := time.Until(time.UnixMilli(deadline))
			if remaining <= 0 {
				if err := record(job, "reconciliation_timed_out", func(r *ops.Recovery) { r.StopRecoveryTimedOut = true }); err != nil {
					return nil, err
				}
				break
			}
			if err := j.assertCapability(ctx, job, permission); err != nil {
				return nil, j.awaitStopDeadline(ctx, job, true, err)
			}
			if _, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, server.Observation, job.Job.BindingRevision); err != nil {
				return nil, j.awaitStopDeadline(ctx, job, true, err)
			}
			select {
			case <-time.After(min(j.poll(), remaining)):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			refreshed, err := j.resolve(ctx, job, permission, true)
			if err != nil {
				// Retry transient daemon failures within the deadline; changes to
				// authority or the binding stop reconciliation at once.
				if _, public := apperr.As(err); public {
					return nil, j.awaitStopDeadline(ctx, job, true, err)
				}
				continue
			}
			server = refreshed
		}
		if server.Container.Exit != nil {
			j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, server.Logical.ID, job.Job.Kind == ops.KindStop))
			if err := record(job, "reconciling", func(r *ops.Recovery) {
				r.StopSettled = true
				r.ForceStopped = r.ForceStopped || (!r.Restarting && server.Container.Exit.Code == 137)
			}); err != nil {
				return nil, err
			}
		}
	}
	return lifecycleResult(job.Job.Recovery), nil
}
