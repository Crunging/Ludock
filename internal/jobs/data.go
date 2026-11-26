package jobs

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
)

func fail(code, message string) error { return apperr.New(code, 409, message) }

const restartingGuidance = "The server kept stopping and starting again before this. Running again, it stays up if this fixed the cause, and otherwise goes on restarting."

var stoppedStates = []string{"exited", "dead", "created"}

// dataAuthority rechecks the owner's capability for the data operation and
// the original binding.
func (j *Jobs) dataAuthority(ctx context.Context, server *servers.Context, job *ops.Context) error {
	for _, capability := range dataCapabilities(job) {
		if err := j.assertCapability(ctx, job, capability); err != nil {
			return err
		}
	}
	_, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, server.Observation, server.Logical.BindingRevision)
	return err
}

// dataAuthorityIn is dataAuthority through tx, for a write the data
// operation makes.
func (j *Jobs) dataAuthorityIn(ctx context.Context, tx *sql.Tx, server *servers.Context, job *ops.Context) error {
	if err := j.allowedIn(ctx, tx, job, dataCapabilities(job)...); err != nil {
		return err
	}
	_, err := j.Identity.AssertObservedBindingIn(ctx, tx, server.Logical.ID, server.Observation, server.Logical.BindingRevision)
	return err
}

func dataCapabilities(job *ops.Context) []authz.Capability {
	switch job.Job.Kind {
	case ops.KindRestart:
		return []authz.Capability{authz.ServerRestart, authz.BackupsCreate}
	case ops.KindBackup:
		return []authz.Capability{authz.BackupsCreate}
	case ops.KindRestore, ops.KindUndoRestore:
		return []authz.Capability{authz.BackupsRestore}
	}
	return []authz.Capability{authz.ServerUpdate}
}

// withSave adds the outcome of the save before the stop to a result, and
// says what starting a server whose game kept exiting again means.
func withSave(job *ops.Context, result *ops.Result) *ops.Result {
	if recovery := job.Job.Recovery; recovery.SaveStatus != "" {
		result.SaveStatus, result.SaveReason = recovery.SaveStatus, recovery.SaveReason
	}
	if job.Job.Recovery.SaveReason == "restarting" && wasRunning(job) {
		result.Guidance = strings.TrimSpace(result.Guidance + " " + restartingGuidance)
	}
	if undone := job.Job.Recovery.Undone; undone != "" {
		result.UndoNote = undone
	}
	if leftOut := job.Job.Recovery.LeftOut; leftOut != "" {
		result.LeftOut = leftOut
	}
	return result
}

// stopForBackup stops the server for a backup, making room for it first as
// retention would after it. A restore's source stays.
func (j *Jobs) stopForBackup(ctx context.Context, server *servers.Context, job *ops.Context) error {
	// Refused before any backup is removed for it.
	if err := j.assertPendingBeforeStop(ctx, server, job); err != nil {
		return err
	}
	source, _ := text(job.Job.Input["backupId"])
	if err := j.Backups.MakeRoom(ctx, server, func() error { return j.dataAuthority(ctx, server, job) },
		func(tx *sql.Tx) error { return j.dataAuthorityIn(ctx, tx, server, job) }, source); err != nil {
		return err
	}
	// Checked again: making room takes time, and the server can change.
	return j.stopForData(ctx, server, job)
}

// assertPendingBeforeStop refuses, while the game still runs, work whose
// stop would kick players for nothing; see assertPendingUndoable. Work
// resuming after its stop has none to avoid.
func (j *Jobs) assertPendingBeforeStop(ctx context.Context, server *servers.Context, job *ops.Context) error {
	if job.Job.Recovery.InitialRunning != nil {
		return nil
	}
	return j.assertPendingUndoable(ctx, server, job, func() error { return j.dataAuthority(ctx, server, job) })
}

// stopForData stops the server, recording its initial state and the locks
// it needs before the stop is dispatched.
func (j *Jobs) stopForData(ctx context.Context, server *servers.Context, job *ops.Context) error {
	if err := j.assertPendingBeforeStop(ctx, server, job); err != nil {
		return err
	}
	// The copy is only as fresh as the game's last save. A job that already
	// saved, or resumes after its stop, doesn't save again.
	if job.Job.Recovery.InitialRunning == nil && job.Job.Recovery.SaveStatus == "" {
		// Decided by the server as it is now: it may have been started or
		// stopped elsewhere since the work resolved it.
		current, err := j.current(ctx, server)
		if err != nil {
			return err
		}
		if current.Container.State == "running" || current.Container.State == "restarting" {
			if err := j.saveWorld(ctx, job, current, func() error { return j.dataAuthority(ctx, current, job) }); err != nil {
				return err
			}
		}
	}
	// dispatching is set once the checks before a stop request pass, and
	// cleared once it is known never to have reached Docker.
	dispatching := false
	// No stop reached Docker, so there is no shutdown to wait out.
	unsent := func() error {
		dispatching = false
		if !stopPending(job) {
			return nil
		}
		return job.Progress(job.Job.Phase, func(r *ops.Recovery) { r.StopStarted = false }, nil)
	}
	err := j.Servers.Observer.ChangeStateUnsent(ctx, server.Container.ID, servers.Stop, func(observation identity.Observation, info *docker.ContainerInspect) error {
		if err := backups.AssertDataOperationState(info.State.Status); err != nil {
			return err
		}
		if err := j.dataAuthority(ctx, server, job); err != nil {
			return err
		}
		if _, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, observation, server.Logical.BindingRevision); err != nil {
			return err
		}
		// The stop is recorded, and outage tracking paused, in the
		// transaction that confirms the job may still stop the server, so a
		// refusal leaves neither to undo. A server already stopped elsewhere
		// keeps its outage, which was alerted once. Called again after a stop
		// that never reached Docker: the new inspection replaces the stop
		// state, but not the initial state.
		var saved func()
		record := func(tx *sql.Tx) error {
			if err := j.dataAuthorityIn(ctx, tx, server, job); err != nil {
				return err
			}
			var err error
			saved, err = job.ProgressIn(tx, "stopping", func(r *ops.Recovery) {
				recordStop(r, info)
				if r.InitialRunning == nil {
					initial := r.StopStarted
					r.InitialRunning = &initial
				}
				r.ContainerID, r.Fingerprint, r.LockKeys = server.Container.ID, server.Logical.BindingFingerprint, server.LockKeys
				r.DataUnsafe = false
			}, nil)
			return err
		}
		var err error
		if info.State.Running {
			err = j.Monitor.Suppress(ctx, server.Logical.ID, record)
		} else {
			err = j.DB.Tx(ctx, record)
		}
		if err != nil {
			return err
		}
		saved()
		dispatching = true
		return nil
	}, unsent)
	if err != nil && !dispatching {
		if err := unsent(); err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	stopped, err := j.Docker.Inspect(ctx, server.Container.ID)
	if err != nil {
		return err
	}
	if servers.IsRunning(stopped) {
		return fail("SERVER_DID_NOT_STOP", "The server did not stop; no data was copied.")
	}
	if err := job.Progress("stopped", func(r *ops.Recovery) {
		r.StoppedStartedAt, r.StopSettled = stopped.State.StartedAt, true
	}, nil); err != nil {
		return err
	}
	if err := j.finishPendingRestores(ctx, server, job, stopped.State.StartedAt); err != nil {
		return err
	}
	// Only a stop this work made is judged: a server killed earlier, such as
	// one Docker had to kill when stopped outside Ludock, is copied as it is.
	if forcedStop(&job.Job.Recovery, stopped) {
		return fail("UNCLEAN_SHUTDOWN", "The server didn’t shut down by itself and had to be forced to stop, so Ludock went no further: its last save may be incomplete. Give it more time to stop where it is defined (its stop timeout), then try again.")
	}
	return nil
}

// current is server as Docker shows it now, under the same binding.
func (j *Jobs) current(ctx context.Context, server *servers.Context) (*servers.Context, error) {
	observed, _, err := j.Servers.Observer.Inspect(ctx, server.Container.ID)
	if err != nil {
		return nil, err
	}
	if _, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, observed.Observation, server.Logical.BindingRevision); err != nil {
		return nil, err
	}
	current := *server
	current.Container, current.Observation = observed.Container, observed.Observation
	return &current, nil
}

// assertStopped confirms the server is still the stopped instance and that
// no other container writes to its data.
func (j *Jobs) assertStopped(ctx context.Context, server *servers.Context, job *ops.Context) error {
	if err := j.assertStillStopped(ctx, server, job); err != nil {
		return err
	}
	return j.Backups.AssertNoOtherWriters(ctx, server, job.Job.Recovery.DataHelperID)
}

var errServerChanged = fail("EXTERNAL_SERVER_CHANGE", "Something outside Ludock started or changed the server meanwhile. Check it before trying again.")

// assertStillStopped confirms the server is still the instance this
// operation stopped, under the same binding.
func (j *Jobs) assertStillStopped(ctx context.Context, server *servers.Context, job *ops.Context) error {
	changed := errServerChanged
	observed, info, err := j.Servers.Observer.Inspect(ctx, server.Container.ID)
	if docker.IsNotFound(err) {
		// Compose and the like replace a container to change it.
		return changed
	}
	if err != nil {
		return err
	}
	if _, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, observed.Observation, server.Logical.BindingRevision); err != nil {
		return err
	}
	if servers.IsRunning(info) || info.State.Paused || info.State.Restarting ||
		info.State.StartedAt != job.Job.Recovery.StoppedStartedAt {
		return changed
	}
	return nil
}

func wasRunning(job *ops.Context) bool {
	initial := job.Job.Recovery.InitialRunning
	return initial != nil && *initial
}

func stopPending(job *ops.Context) bool {
	return job.Job.Recovery.StopStarted && !job.Job.Recovery.StopSettled
}

func (j *Jobs) waitForStopDeadline(ctx context.Context, job *ops.Context) error {
	deadline, now := job.Job.Recovery.StopRecoveryDeadlineAt, time.Now().UnixMilli()
	if deadline <= now {
		return fail("STOP_RECOVERY_REQUIRED", "Ludock couldn’t confirm in time that the server stopped, so it left it as it is. Check the server before trying again.")
	}
	// On shutdown the operation stays running for the next startup.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(min(j.poll(), time.Duration(deadline-now)*time.Millisecond)):
		return nil
	}
}

// observeDuringStop confirms the binding, retrying daemon failures until the
// saved stop deadline.
func (j *Jobs) observeDuringStop(ctx context.Context, job *ops.Context, containerID string, logical *identity.Server) (*servers.Observed, error) {
	for {
		observed, _, err := j.Servers.Observer.Inspect(ctx, containerID)
		if err == nil {
			_, err = j.Identity.AssertObservedBinding(ctx, logical.ID, observed.Observation, logical.BindingRevision)
		}
		if err == nil {
			return observed, nil
		}
		if _, public := apperr.As(err); !stopPending(job) || public {
			return nil, err
		}
		if err := j.waitForStopDeadline(ctx, job); err != nil {
			return nil, err
		}
	}
}

// settleInterruptedStop waits for a stop that may still be executing in
// Docker without dispatching it again. It returns nil if another manager
// started the server meanwhile.
func (j *Jobs) settleInterruptedStop(ctx context.Context, server *servers.Context, job *ops.Context) (*servers.Observed, error) {
	observed, err := j.observeDuringStop(ctx, job, server.Container.ID, server.Logical)
	if err != nil || !stopPending(job) {
		return observed, err
	}
	if err := job.Progress("reconciling_stop", nil, nil); err != nil {
		return nil, err
	}
	for !slices.Contains(stoppedStates, observed.Container.State) {
		initial := job.Job.Recovery.StopInitialStartedAt
		if observed.Container.State == "running" && initial != nil && observed.Container.StateSince != 0 && observed.Container.StateSince != *initial {
			return nil, job.Progress("stop_reconciled", settleStop, nil)
		}
		if err := j.waitForStopDeadline(ctx, job); err != nil {
			return nil, err
		}
		if observed, err = j.observeDuringStop(ctx, job, server.Container.ID, server.Logical); err != nil {
			return nil, err
		}
	}
	if err := job.Progress("stop_reconciled", settleStop, nil); err != nil {
		return nil, err
	}
	return observed, nil
}

func settleStop(r *ops.Recovery) { r.StopSettled = true }

// restartedAfter begins what recovery says of work that had done its part
// before Ludock restarted.
const restartedAfter = "Ludock restarted while this ran, after "

// What recovery found a server it had stopped as, for every operation. A
// start whose answer was lost counts as sent once Docker shows a newer run,
// which may be another manager's, so the start is worded as asked for.
const (
	serverStarted      = "Ludock asked Docker to start the server, and it is running."
	serverRunningAgain = "The server was running again, so Ludock left it as it was."
	serverStoppedAgain = "The server ran again and had stopped when Ludock checked it, so Ludock left it stopped."
	serverRestarting   = "The server was started again, and its game keeps exiting, so Docker keeps restarting it."
)

// withServerAfterRecovery adds to result's guidance what recovery did with
// the server the operation stopped.
func withServerAfterRecovery(job *ops.Context, result *ops.Result) *ops.Result {
	recovery := job.Job.Recovery
	var said string
	switch {
	case recovery.InitialRunning != nil && !*recovery.InitialRunning:
		said = "The server was stopped before this began, so Ludock didn’t start it."
	case !wasRunning(job):
		return result
	case recovery.StateRestored && recovery.StartRequested:
		said = serverStarted
	case recovery.StateRestored:
		said = serverRunningAgain
	// A newer run after a start recovery had requested, seen this attempt.
	case slices.Contains(stoppedStates, recovery.RunSeen):
		said = serverStoppedAgain
	case recovery.RunSeen == "restarting":
		said = serverRestarting
	case recovery.RunSeen != "":
		said = "The server was started again, and Docker shows it " + recovery.RunSeen + ", so Ludock left it as it is."
	default:
		return result
	}
	if result == nil {
		result = &ops.Result{}
	}
	result.Guidance = strings.TrimSpace(result.Guidance + " " + said)
	return result
}

// restoreInitialState starts a server that was running when the operation
// stopped it, once its data is safe. A start that reached Docker is never
// repeated.
func (j *Jobs) restoreInitialState(ctx context.Context, server *servers.Context, job *ops.Context) error {
	recovery := job.Job.Recovery
	if !wasRunning(job) || recovery.DataUnsafe || recovery.PendingUndo || recovery.StateRestored {
		return nil
	}
	observed, err := j.settleInterruptedStop(ctx, server, job)
	if err != nil {
		return err
	}
	// Running without a start from this attempt, it may be anyone's.
	foundRunning := func(r *ops.Recovery) { r.StateRestored, r.StartRequested = true, false }
	if observed == nil {
		return job.Progress("state_restored", foundRunning, nil)
	}
	if observed.Container.State != "running" {
		if job.Job.Recovery.StartRequested {
			info, err := j.Docker.Inspect(ctx, server.Container.ID)
			if err != nil {
				return err
			}
			// A newer run means the start took; it isn't sent again.
			if info.State.StartedAt != job.Job.Recovery.StoppedStartedAt {
				if info.State.Running && !info.State.Restarting && !info.State.Paused {
					return job.Progress("state_restored", foundRunning, nil)
				}
				job.Job.Recovery.RunSeen = info.State.Status
				return nil
			}
		}
		// Persist before dispatch, so recovery recognizes a start that reached Docker.
		if err := job.Progress("restarting", func(r *ops.Recovery) { r.StartRequested = true }, nil); err != nil {
			return err
		}
		err := j.Servers.Observer.ChangeStateExact(ctx, server.Container.ID, servers.Start, func(observation identity.Observation, _ *docker.ContainerInspect) error {
			_, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, observation, server.Logical.BindingRevision)
			return err
		})
		// Docker had it running already: someone else's start.
		restored := func(r *ops.Recovery) { r.StateRestored = true }
		if errors.Is(err, servers.ErrUnchanged) {
			restored, err = foundRunning, nil
		}
		if err != nil {
			return err
		}
		if err := job.Progress("state_restored", restored, nil); err != nil {
			return err
		}
		return j.Monitor.Suppress(ctx, server.Logical.ID, nil)
	}
	if err := job.Progress("state_restored", foundRunning, nil); err != nil {
		return err
	}
	return j.Monitor.Suppress(ctx, server.Logical.ID, nil)
}

// keepStoppedAfterFailedBackup leaves the server stopped, since starting
// could update the game and change the world, and says so once stopped.
func (j *Jobs) keepStoppedAfterFailedBackup(ctx context.Context, server *servers.Context, job *ops.Context) error {
	return j.keepStopped(ctx, server, job, notify.FailedWork, "the backup failed, so the server was left stopped. Check its Activity, then start it.")
}

func (j *Jobs) keepStopped(ctx context.Context, server *servers.Context, job *ops.Context, alert notify.Alert, reason string) error {
	observed, err := j.settleInterruptedStop(ctx, server, job)
	if err != nil || observed == nil || !slices.Contains(stoppedStates, observed.Container.State) {
		return err
	}
	j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, server.Logical.ID, true))
	j.notify(alert, "operation:"+job.Job.ID, observed.Container.DisplayName+": "+reason)
	return nil
}

// recoveryLocks reclaims the saved shared-volume and path locks before any
// Docker request can fail or hang.
func recoveryLocks(job *ops.Context) []string {
	return append([]string{locks.ServerKey(job.Job.ServerID), locks.StorageKey}, job.Job.Recovery.LockKeys...)
}

// removeOperationHelpers removes this operation's helpers. Failing to list
// them is not evidence that no writer remains.
func (j *Jobs) removeOperationHelpers(ctx context.Context, job *ops.Context) error {
	var helpers []docker.ContainerSummary
	for {
		var err error
		helpers, err = j.Docker.ListContainers(ctx, true, docker.Filters{"label": {"ludock.operation=" + job.Job.ID}})
		if err == nil {
			break
		}
		// On shutdown the operation stays running for the next startup.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(j.poll()):
		}
	}
	for _, candidate := range helpers {
		internal := candidate.Labels["ludock.internal"]
		if (internal != "backup-helper" && internal != "mount-validator") || candidate.Labels["ludock.operation"] != job.Job.ID {
			continue
		}
		j.Docker.RemoveOperationHelper(candidate.ID)
	}
	return job.Progress("recovering", func(r *ops.Recovery) { r.DataHelperID = "" }, nil)
}

func (j *Jobs) recoveryContext(ctx context.Context, job *ops.Context) (*servers.Context, error) {
	if err := j.removeOperationHelpers(ctx, job); err != nil {
		return nil, err
	}
	containerID := job.Job.Recovery.ContainerID
	if containerID == "" {
		return nil, nil
	}
	logical, err := j.Identity.Get(ctx, job.Job.ServerID)
	if err != nil {
		return nil, err
	}
	if logical == nil || logical.Status != identity.StatusActive || logical.ContainerID == "" || logical.ContainerID != containerID ||
		logical.BindingFingerprint != job.Job.Recovery.Fingerprint {
		return nil, fail("RECOVERY_BINDING_CHANGED", "The server changed in Docker while this was interrupted, so Ludock left it as it is. Check the server before trying again.")
	}
	observed, err := j.observeDuringStop(ctx, job, logical.ContainerID, logical)
	if err != nil {
		return nil, err
	}
	return &servers.Context{Logical: logical, Container: observed.Container, Observation: observed.Observation,
		LockKeys: servers.LockKeys(logical.ID, observed.Observation)}, nil
}

func (j *Jobs) withRecoveryLocks(ctx context.Context, job *ops.Context, action func(*servers.Context) error) error {
	held := recoveryLocks(job)
	release, err := j.Locks.AcquireForRecovery(held)
	if err != nil {
		return err
	}
	defer release()
	server, err := j.recoveryContext(ctx, job)
	if err != nil || server == nil {
		return err
	}
	var extra []string
	for _, key := range server.LockKeys {
		if !slices.Contains(held, key) {
			extra = append(extra, key)
		}
	}
	releaseExtra, err := j.Locks.AcquireForRecovery(extra)
	if err != nil {
		return err
	}
	defer releaseExtra()
	// Interrupted while it finished undoing an earlier restore: that undo
	// completes before this operation's own recovery.
	if job.Job.Recovery.PendingUndo {
		if err := j.finishPendingRestores(ctx, server, job, job.Job.Recovery.StoppedStartedAt); err != nil {
			return err
		}
	}
	return action(server)
}
