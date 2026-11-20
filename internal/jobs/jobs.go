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
	runner := registrar{queue, j}
	for _, action := range []servers.Action{servers.Start, servers.Stop} {
		runner.Register(string(action), ops.Handler{
			Run: func(ctx context.Context, job *ops.Context) (map[string]any, error) {
				if action == servers.Stop {
					if err := j.warnFirst(ctx, job, capabilityFor(action), false); err != nil {
						return nil, calledOff(job, err)
					}
				}
				result, err := j.runLifecycle(ctx, job, action)
				return warnedResult(job, result, err)
			},
			Recover: func(ctx context.Context, job *ops.Context) (map[string]any, error) {
				if resumable(job) {
					return nil, ops.ErrResume
				}
				return j.recoverLifecycle(ctx, job)
			},
		})
	}
	runner.Register("restart", ops.Handler{
		Run: func(ctx context.Context, job *ops.Context) (map[string]any, error) {
			if err := j.warnFirst(ctx, job, authz.ServerRestart, false); err != nil {
				return nil, calledOff(job, err)
			}
			if job.Job.Input["createBackup"] == true {
				result, err := j.runBackupRestart(ctx, job)
				return warnedResult(job, result, err)
			}
			result, err := j.runLifecycle(ctx, job, servers.Restart)
			return warnedResult(job, result, err)
		},
		Recover: func(ctx context.Context, job *ops.Context) (map[string]any, error) {
			if resumable(job) {
				return nil, ops.ErrResume
			}
			if job.Job.Input["createBackup"] == true {
				return j.recoverBackupRestart(ctx, job)
			}
			return j.recoverLifecycle(ctx, job)
		},
	})
	runner.Register("backup", ops.Handler{Run: j.warnedFirst(authz.BackupsCreate, j.runBackup), Recover: func(ctx context.Context, job *ops.Context) (map[string]any, error) {
		if resumable(job) {
			return nil, ops.ErrResume
		}
		return nil, j.recoverBackup(ctx, job, true)
	}})
	// An interrupted command is reported as interrupted and never sent again.
	runner.Register("command", ops.Handler{Run: j.runCommand, Recover: func(context.Context, *ops.Context) (map[string]any, error) {
		return map[string]any{"action": "command"}, nil
	}})
	runner.Register("restore", ops.Handler{Run: j.warnedFirst(authz.BackupsRestore, j.runRestore), Recover: func(ctx context.Context, job *ops.Context) (map[string]any, error) {
		if resumable(job) {
			return nil, ops.ErrResume
		}
		return j.recoverRestore(ctx, job)
	}})
	runner.Register("update", ops.Handler{Run: func(ctx context.Context, job *ops.Context) (map[string]any, error) {
		result, err := j.runUpdate(ctx, job)
		return warnedResult(job, result, err)
	}, Recover: func(ctx context.Context, job *ops.Context) (map[string]any, error) {
		if resumable(job) {
			return nil, ops.ErrResume
		}
		return nil, j.recoverUpdate(ctx, job)
	}})
}

func (j *Jobs) notify(alert notify.Alert, key, message string) {
	if j.Notify != nil {
		j.Notify(alert, key, message)
	}
}

// notifyDone reports finished work nobody watched start: scheduled or
// automatic work. Work started in the panel shows its result there, and
// notifies only if it fails.
func (j *Jobs) notifyDone(job *ops.Context, key, message string) {
	if _, scheduled := job.Job.Input["scheduleId"].(string); scheduled || job.Job.ActorID == ops.RecoveryActor {
		j.notify(notify.Results, key, message)
	}
}

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
	user, err := j.DB.FindUserByID(ctx, job.Job.ActorID)
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
	err = j.DB.QueryRowContext(ctx, "SELECT owner_id,input_json,revision FROM schedules WHERE id=? AND server_id=?",
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
	if _, err := j.Authz.Assert(ctx, actor, job.Job.ServerID, authz.SchedulesManage); err != nil {
		return nil, err
	}
	return actor, nil
}

var errRecoveryOff = apperr.New("RECOVERY_OFF", 403, "Automatic restarts were turned off for this server, or it changed and awaits review")

// recoveryAllowed is the authority of a recovery restart: the server's
// monitoring policy still asks for it, and no change to the server awaits
// an administrator's review.
func (j *Jobs) recoveryAllowed(ctx context.Context, job *ops.Context) error {
	if job.Job.Kind != "restart" || job.Job.Input["createBackup"] == true {
		return errRecoveryOff
	}
	availability, err := j.Monitor.Get(ctx, job.Job.ServerID)
	if err != nil {
		return err
	}
	logical, err := j.Identity.Get(ctx, job.Job.ServerID)
	if err != nil {
		return err
	}
	if !availability.Policy.RecoveryOn() || logical == nil || logical.SharingPaused {
		return errRecoveryOff
	}
	// Turning recovery off forgets the restart it queued, even if it is on
	// again by now.
	state, err := j.Monitor.Recovery(ctx, job.Job.ServerID)
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
		return j.Servers.ResolveChecked(ctx, job.Job.ServerID, revision, func(ctx context.Context) error { return j.recoveryAllowed(ctx, job) })
	}
	actor, err := j.Actor(ctx, job)
	if err != nil {
		return nil, err
	}
	return j.Servers.ResolveAuthorized(ctx, actor, job.Job.ServerID, capability, revision)
}

func (j *Jobs) assertCapability(ctx context.Context, job *ops.Context, capability authz.Capability) error {
	if job.Job.ActorID == ops.RecoveryActor {
		return j.recoveryAllowed(ctx, job)
	}
	actor, err := j.Actor(ctx, job)
	if err != nil {
		return err
	}
	_, err = j.Authz.Assert(ctx, actor, job.Job.ServerID, capability)
	return err
}

const forceStopGuidance = "Force-stopped, recent progress may not have been saved."

func lifecycleResult(kind string, recovery ops.Recovery) map[string]any {
	result := map[string]any{"action": kind}
	if recovery.SaveStatus != "" {
		result["saveStatus"], result["saveReason"] = recovery.SaveStatus, recovery.SaveReason
	}
	if recovery.StopRecoveryTimedOut {
		result["stopRecoveryTimedOut"] = true
		result["guidance"] = "Shutdown could not be confirmed before the recovery deadline. Review the server state before retrying."
	}
	if recovery.ForceStopped {
		result["forceStopped"] = true
		result["guidance"] = forceStopGuidance
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
	return job.Progress(phase, func(r *ops.Recovery) { *r = next }, lifecycleResult(job.Job.Kind, next))
}

// saveWorld sends the game's save command, best effort. Revoked authority
// still stops the job.
func (j *Jobs) saveWorld(ctx context.Context, job *ops.Context, server *servers.Context, assertAccess func() error) error {
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

func (j *Jobs) runLifecycle(ctx context.Context, job *ops.Context, action servers.Action) (map[string]any, error) {
	permission := capabilityFor(action)
	initial, err := j.resolve(ctx, job, permission, true)
	if err != nil {
		return nil, err
	}
	recovery := job.Job.ActorID == ops.RecoveryActor
	return job.WithLocks(initial.LockKeys, func() (map[string]any, error) {
		server, err := j.resolve(ctx, job, permission, true)
		if err != nil {
			return nil, err
		}
		if recovery && !j.stillHung(job, server) {
			return recoverySkipped, nil
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
		// Pause outage tracking before acting, or the change could alert.
		if err := j.Monitor.Suppress(ctx, server.Logical.ID); err != nil {
			return nil, err
		}
		// server is nil after a failed resolve, so the ID is kept apart.
		serverID := server.Logical.ID
		defer func() { j.noteAvailability(j.Monitor.Suppress(context.WithoutCancel(ctx), serverID)) }()
		if action != servers.Start {
			if err := j.saveWorld(ctx, job, server, assertAccess); err != nil {
				return nil, err
			}
			if err := mutate(servers.Stop); errors.Is(err, errRecoveryStopped) {
				return recoverySkipped, nil
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
				r.ForceStopped = r.StopStarted && server.Container.Exit != nil && server.Container.Exit.Code == 137
			}); err != nil {
				return nil, err
			}
		}
		if action != servers.Stop {
			if err := mutate(servers.Start); errors.Is(err, errRecoveryStopped) {
				return recoverySkipped, nil
			} else if err != nil {
				return nil, err
			}
			j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, server.Logical.ID, false))
		}
		if err := assertAccess(); err != nil {
			return nil, err
		}
		return lifecycleResult(job.Job.Kind, job.Job.Recovery), nil
	})
}

var (
	recoverySkipped = map[string]any{"action": "restart", "skipped": true,
		"guidance": "Skipped: the game answered again, or the server was no longer running"}
	errRecoveryStopped = errors.New("server stopped before its recovery restart")
)

// stillHung confirms right before a recovery restart that the container run
// judged hung still runs and its game still doesn't answer.
// sameRun reports whether a container's latest run is the one a recovery
// restart was queued for; a stop keeps its start time.
func sameRun(job *ops.Context, info *docker.ContainerInspect) bool {
	run, ok := number(job.Job.Input["run"])
	started, err := time.Parse(time.RFC3339Nano, info.State.StartedAt)
	return ok && err == nil && float64(started.UnixMilli()) == run
}

// stillHung also requires the silence judged: an answer since starts a new
// one, with its own wait.
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
}

// recoverLifecycle only observes: Docker may still be handling the
// original stop, so the lock is kept until it settles, without starting or
// stopping again.
func (j *Jobs) recoverLifecycle(ctx context.Context, job *ops.Context) (map[string]any, error) {
	permission := capabilityFor(servers.Action(job.Job.Kind))
	if job.Job.Kind != "start" && job.Job.Kind != "stop" && job.Job.Kind != "restart" {
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
	defer func() { j.noteAvailability(j.Monitor.Suppress(context.WithoutCancel(ctx), server.Logical.ID)) }()
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
			j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, server.Logical.ID, job.Job.Kind == "stop"))
			if err := record(job, "reconciling", func(r *ops.Recovery) {
				r.StopSettled = true
				r.ForceStopped = r.ForceStopped || server.Container.Exit.Code == 137
			}); err != nil {
				return nil, err
			}
		}
	}
	return lifecycleResult(job.Job.Kind, job.Job.Recovery), nil
}
