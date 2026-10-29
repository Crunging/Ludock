// Package jobs implements the persisted server operations: start, stop,
// restart (optionally with a backup), backup, restore, and image update.
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
	"github.com/Crunging/Ludock/internal/ops"
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
	Notify   func(key, message string)
	Console  *console.Runtime
	Backups  *backups.Service
	Updates  *update.Service
	// SaveSettle is how long to wait after a save command; tests shorten it.
	SaveSettle time.Duration
	// ReplacementSettle is how long an updated container without a
	// healthcheck must keep running before it replaces the original; tests
	// shorten it.
	ReplacementSettle time.Duration
	// Poll is the reconciliation poll interval; tests shorten it.
	Poll time.Duration
}

func (j *Jobs) Register(runner *ops.Runner) {
	for _, action := range []servers.Action{servers.Start, servers.Stop} {
		runner.Register(string(action), ops.Handler{
			Run: func(ctx context.Context, job *ops.Context) (map[string]any, error) {
				return j.runLifecycle(ctx, job, action)
			},
			Recover: j.recoverLifecycle,
		})
	}
	runner.Register("restart", ops.Handler{
		Run: func(ctx context.Context, job *ops.Context) (map[string]any, error) {
			if job.Job.Input["createBackup"] == true {
				return j.runBackupRestart(ctx, job)
			}
			return j.runLifecycle(ctx, job, servers.Restart)
		},
		Recover: func(ctx context.Context, job *ops.Context) (map[string]any, error) {
			if job.Job.Input["createBackup"] == true {
				return nil, j.recoverBackupRestart(ctx, job)
			}
			return j.recoverLifecycle(ctx, job)
		},
	})
	runner.Register("backup", ops.Handler{Run: j.runBackup, Recover: func(ctx context.Context, job *ops.Context) (map[string]any, error) {
		return nil, j.recoverBackup(ctx, job, true)
	}})
	runner.Register("restore", ops.Handler{Run: j.runRestore, Recover: func(ctx context.Context, job *ops.Context) (map[string]any, error) {
		return nil, j.recoverRestore(ctx, job)
	}})
	runner.Register("update", ops.Handler{Run: j.runUpdate, Recover: func(ctx context.Context, job *ops.Context) (map[string]any, error) {
		return nil, j.recoverUpdate(ctx, job)
	}})
}

func (j *Jobs) notify(key, message string) {
	if j.Notify != nil {
		j.Notify(key, message)
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

func stringList(value any) ([]string, bool) {
	switch typed := value.(type) {
	case nil:
		return nil, true
	case []string:
		return typed, true
	case []any:
		var result []string
		for _, item := range typed {
			entry, ok := item.(string)
			if !ok || entry == "" {
				return nil, false
			}
			result = append(result, entry)
		}
		return result, true
	}
	return nil, false
}

var (
	errRevoked         = apperr.New("ACCESS_REVOKED", 403, "The operation owner no longer has access")
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

func (j *Jobs) resolve(ctx context.Context, job *ops.Context, capability authz.Capability, pinned bool) (*servers.Context, error) {
	actor, err := j.Actor(ctx, job)
	if err != nil {
		return nil, err
	}
	revision := int64(-1)
	if pinned {
		revision = job.Job.BindingRevision
	}
	return j.Servers.ResolveAuthorized(ctx, actor, job.Job.ServerID, capability, revision)
}

func (j *Jobs) assertCapability(ctx context.Context, job *ops.Context, capability authz.Capability) error {
	actor, err := j.Actor(ctx, job)
	if err != nil {
		return err
	}
	_, err = j.Authz.Assert(ctx, actor, job.Job.ServerID, capability)
	return err
}

const forceStopGuidance = "Force-stopped, recent progress may not have been saved."

func lifecycleResult(job *ops.Context, updates map[string]any) map[string]any {
	recovery := map[string]any{}
	for key, value := range job.Job.Recovery {
		recovery[key] = value
	}
	for key, value := range updates {
		recovery[key] = value
	}
	result := map[string]any{"action": job.Job.Kind}
	if status, ok := recovery["saveStatus"].(string); ok {
		result["saveStatus"], result["saveReason"] = status, recovery["saveReason"]
	}
	if recovery["stopRecoveryTimedOut"] == true {
		result["stopRecoveryTimedOut"] = true
		result["guidance"] = "Shutdown could not be confirmed before the recovery deadline. Review the server state before retrying."
	}
	if recovery["forceStopped"] == true {
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
func record(job *ops.Context, phase string, recovery map[string]any) error {
	return job.Progress(phase, recovery, lifecycleResult(job, recovery))
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
		return record(job, "save_skipped", map[string]any{"saveStatus": "skipped", "saveReason": reason})
	}
	if server.Container.State != "running" {
		return record(job, "save_skipped", map[string]any{"saveStatus": "skipped", "saveReason": "not_running"})
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
		return record(job, "save_skipped", map[string]any{"saveStatus": "skipped", "saveReason": reason})
	}
	if err := record(job, "waiting_for_save", map[string]any{"saveStatus": "sent", "saveReason": nil}); err != nil {
		return err
	}
	settle := j.SaveSettle
	if settle == 0 {
		settle = 5 * time.Second
	}
	time.Sleep(settle)
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
	return job.WithLocks(initial.LockKeys, func() (map[string]any, error) {
		server, err := j.resolve(ctx, job, permission, true)
		if err != nil {
			return nil, err
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
			phase := "starting"
			if mutation == servers.Stop {
				phase = "stopping"
			}
			// Persist before dispatch, so recovery recognizes a change that
			// reached Docker.
			if err := record(job, phase, map[string]any{"containerId": server.Container.ID}); err != nil {
				return err
			}
			return j.Servers.Observer.ChangeState(ctx, server.Container.ID, mutation, func(observation identity.Observation, info *docker.ContainerInspect) error {
				if err := j.assertCapability(ctx, job, permission); err != nil {
					return err
				}
				if _, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, observation, job.Job.BindingRevision); err != nil {
					return err
				}
				if mutation == servers.Stop {
					// Recovery takes these locks before checking authority again.
					recovery := stopRecovery(info)
					recovery["lockKeys"] = server.LockKeys
					return record(job, "stopping", recovery)
				}
				return nil
			})
		}
		// Pause outage tracking before acting, or the change could alert.
		if err := j.Monitor.Suppress(ctx, server.Logical.ID); err != nil {
			return nil, err
		}
		defer func() { j.noteAvailability(j.Monitor.Suppress(context.WithoutCancel(ctx), server.Logical.ID)) }()
		if action != servers.Start {
			if err := j.saveWorld(ctx, job, server, assertAccess); err != nil {
				return nil, err
			}
			if err := mutate(servers.Stop); err != nil {
				return nil, err
			}
			// Persist the actual stop before a restart clears Docker's exit code.
			j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, server.Logical.ID, action == servers.Stop))
			if server, err = j.resolve(ctx, job, permission, true); err != nil {
				return nil, err
			}
			if err := record(job, "verifying_stop", map[string]any{"stopSettled": true,
				"forceStopped": job.Job.Recovery["stopStarted"] == true && server.Container.Exit != nil && server.Container.Exit.Code == 137}); err != nil {
				return nil, err
			}
		}
		if action != servers.Stop {
			if err := mutate(servers.Start); err != nil {
				return nil, err
			}
			j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, server.Logical.ID, false))
		}
		if err := assertAccess(); err != nil {
			return nil, err
		}
		return lifecycleResult(job, nil), nil
	})
}

// awaitStopDeadline holds the locks until a dispatched stop's saved deadline
// has passed, even after authority or the binding changed, then returns err.
func (j *Jobs) awaitStopDeadline(ctx context.Context, job *ops.Context, unsettled bool, err error) error {
	deadline, ok := number(job.Job.Recovery["stopRecoveryDeadlineAt"])
	if !unsettled || !ok {
		return err
	}
	if wait := time.Until(time.UnixMilli(int64(deadline))); wait > 0 {
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

func stopRecovery(info *docker.ContainerInspect) map[string]any {
	state := servers.StopRecoveryState(info, time.Now())
	result := map[string]any{"stopStarted": state.StopStarted, "stopRecoveryDeadlineAt": state.StopRecoveryDeadlineAt,
		"stopInitialStartedAt": nil}
	if state.StopInitialStartedAt != nil {
		result["stopInitialStartedAt"] = *state.StopInitialStartedAt
	}
	return result
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
	unsettled := recovery["stopStarted"] == true && recovery["stopSettled"] != true
	// A dispatched stop may still be running in Docker, whatever its owner's
	// authority is now: its saved locks are taken first.
	held, saved := stringList(recovery["lockKeys"])
	if !unsettled || !saved {
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
	containerID, _ := text(recovery["containerId"])
	if unsettled && containerID == server.Container.ID {
		deadline, deadlineOK := number(recovery["stopRecoveryDeadlineAt"])
		initialStarted, initialOK := number(recovery["stopInitialStartedAt"])
		if err := record(job, "reconciling_stop", nil); err != nil {
			return nil, err
		}
		for server.Container.Exit == nil {
			// A different running instance proves the old shutdown is over.
			if server.Container.State == "running" && initialOK && server.Container.StateSince != 0 &&
				float64(server.Container.StateSince) != initialStarted {
				j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, server.Logical.ID, false))
				break
			}
			if !deadlineOK {
				return nil, apperr.New("INVALID_OPERATION_STATE", 409, "Saved stop recovery deadline is invalid")
			}
			remaining := time.Duration(deadline-float64(time.Now().UnixMilli())) * time.Millisecond
			if remaining <= 0 {
				if err := record(job, "reconciliation_timed_out", map[string]any{"stopRecoveryTimedOut": true}); err != nil {
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
			time.Sleep(min(j.poll(), remaining))
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
			if err := record(job, "reconciling", map[string]any{"stopSettled": true,
				"forceStopped": recovery["forceStopped"] == true || server.Container.Exit.Code == 137}); err != nil {
				return nil, err
			}
		}
	}
	return lifecycleResult(job, nil), nil
}
