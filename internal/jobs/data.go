package jobs

import (
	"bytes"
	"context"
	"slices"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
)

func fail(code, message string) error { return apperr.New(code, 409, message) }

var stoppedStates = []string{"exited", "dead", "created"}

// dataAuthority rechecks the owner's capability for the data operation and
// the original binding.
func (j *Jobs) dataAuthority(ctx context.Context, server *servers.Context, job *ops.Context) error {
	if job.Job.Kind == "restart" {
		if err := j.assertCapability(ctx, job, authz.ServerRestart); err != nil {
			return err
		}
	}
	capability := authz.ServerUpdate
	switch job.Job.Kind {
	case "backup", "restart":
		capability = authz.BackupsCreate
	case "restore":
		capability = authz.BackupsRestore
	}
	if err := j.assertCapability(ctx, job, capability); err != nil {
		return err
	}
	_, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, server.Observation, server.Logical.BindingRevision)
	return err
}

// stopForData stops the server, recording its initial state and the locks
// it needs before the stop is dispatched.
func (j *Jobs) stopForData(ctx context.Context, server *servers.Context, job *ops.Context) error {
	err := j.Servers.Observer.ChangeState(ctx, server.Container.ID, servers.Stop, func(observation identity.Observation, info *docker.ContainerInspect) error {
		if err := backups.AssertDataOperationState(info.State.Status); err != nil {
			return err
		}
		if err := j.dataAuthority(ctx, server, job); err != nil {
			return err
		}
		if _, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, observation, server.Logical.BindingRevision); err != nil {
			return err
		}
		if job.Job.Recovery.InitialRunning == nil {
			if err := job.Progress("stopping", func(r *ops.Recovery) {
				recordStop(r, info)
				running := r.StopStarted
				r.InitialRunning = &running
				r.ContainerID, r.Fingerprint, r.LockKeys = server.Container.ID, server.Logical.BindingFingerprint, server.LockKeys
				r.DataUnsafe = false
			}, nil); err != nil {
				return err
			}
		}
		return j.Monitor.Suppress(ctx, server.Logical.ID)
	})
	if err != nil {
		return err
	}
	stopped, err := j.Docker.Inspect(ctx, server.Container.ID)
	if err != nil {
		return err
	}
	if stopped.State.Running || stopped.State.Status == "running" {
		return fail("SERVER_DID_NOT_STOP", "The server did not stop; no data was copied.")
	}
	if err := job.Progress("stopped", func(r *ops.Recovery) {
		r.StoppedStartedAt, r.StopSettled = stopped.State.StartedAt, true
	}, nil); err != nil {
		return err
	}
	if stopped.State.ExitCode == 137 || stopped.State.OOMKilled {
		return fail("UNCLEAN_SHUTDOWN", "The server needed forced termination. The backup was not created; review its graceful shutdown configuration before retrying.")
	}
	return nil
}

// assertStopped confirms the server is still the stopped instance and that
// no other container writes to its data.
func (j *Jobs) assertStopped(ctx context.Context, server *servers.Context, job *ops.Context) error {
	if err := j.assertStillStopped(ctx, server, job); err != nil {
		return err
	}
	return j.Backups.AssertNoOtherWriters(ctx, server, job.Job.Recovery.DataHelperID)
}

// assertStillStopped confirms the server is still the instance this
// operation stopped, under the same binding.
func (j *Jobs) assertStillStopped(ctx context.Context, server *servers.Context, job *ops.Context) error {
	observed, info, err := j.Servers.Observer.Inspect(ctx, server.Container.ID)
	if err != nil {
		return err
	}
	if _, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, observed.Observation, server.Logical.BindingRevision); err != nil {
		return err
	}
	if info.State.Running || info.State.Status == "running" || info.State.Paused || info.State.Restarting ||
		info.State.StartedAt != job.Job.Recovery.StoppedStartedAt {
		return fail("EXTERNAL_SERVER_CHANGE", "The server was started or changed by another manager during the operation. Review its state before retrying.")
	}
	return nil
}

func stopPending(job *ops.Context) bool {
	return job.Job.Recovery.StopStarted && !job.Job.Recovery.StopSettled
}

func (j *Jobs) waitForStopDeadline(ctx context.Context, job *ops.Context) error {
	deadline, now := job.Job.Recovery.StopRecoveryDeadlineAt, time.Now().UnixMilli()
	if deadline <= now {
		return fail("STOP_RECOVERY_REQUIRED", "Shutdown could not be confirmed before the recovery deadline. Review the server state before retrying.")
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

// restoreInitialState starts a server that was running when the operation
// stopped it, once its data is safe. A start that reached Docker is never
// repeated.
func (j *Jobs) restoreInitialState(ctx context.Context, server *servers.Context, job *ops.Context) error {
	dataSafe := !job.Job.Recovery.DataUnsafe
	if job.Job.Kind == "restore" {
		restore, err := restoreState(job)
		if err != nil {
			return err
		}
		if restore != nil {
			dataSafe = restore.Status != "replacing" && restore.Status != "rolling_back"
		}
	}
	if initial := job.Job.Recovery.InitialRunning; initial == nil || !*initial || !dataSafe || job.Job.Recovery.StateRestored {
		return nil
	}
	observed, err := j.settleInterruptedStop(ctx, server, job)
	if err != nil {
		return err
	}
	if observed == nil {
		return job.Progress("state_restored", func(r *ops.Recovery) { r.StateRestored = true }, nil)
	}
	if observed.Container.State != "running" {
		if job.Job.Recovery.StartRequested {
			info, err := j.Docker.Inspect(ctx, server.Container.ID)
			if err != nil {
				return err
			}
			if info.State.StartedAt != job.Job.Recovery.StoppedStartedAt {
				return nil
			}
		}
		// Persist before dispatch, so recovery recognizes a start that reached Docker.
		if err := job.Progress("restarting", func(r *ops.Recovery) { r.StartRequested = true }, nil); err != nil {
			return err
		}
		if err := j.Servers.Observer.ChangeState(ctx, server.Container.ID, servers.Start, func(observation identity.Observation, _ *docker.ContainerInspect) error {
			_, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, observation, server.Logical.BindingRevision)
			return err
		}); err != nil {
			return err
		}
	}
	if err := job.Progress("state_restored", func(r *ops.Recovery) { r.StateRestored = true }, nil); err != nil {
		return err
	}
	return j.Monitor.Suppress(ctx, server.Logical.ID)
}

// keepStoppedAfterFailedBackup leaves the server stopped, since starting
// could update the game and change the world, and says so once stopped.
func (j *Jobs) keepStoppedAfterFailedBackup(ctx context.Context, server *servers.Context, job *ops.Context) error {
	return j.keepStopped(ctx, server, job, "the backup failed, so the server was left stopped. Review the operation, then start it.")
}

func (j *Jobs) keepStopped(ctx context.Context, server *servers.Context, job *ops.Context, reason string) error {
	observed, err := j.settleInterruptedStop(ctx, server, job)
	if err != nil || observed == nil || !slices.Contains(stoppedStates, observed.Container.State) {
		return err
	}
	j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, server.Logical.ID, true))
	j.notify("operation:"+job.Job.ID, observed.Container.DisplayName+": "+reason)
	return nil
}

// recoveryLocks reclaims the saved shared-volume and path locks before any
// Docker request can fail or hang.
func recoveryLocks(job *ops.Context) []string {
	return append([]string{"server:" + job.Job.ServerID, "backups:storage"}, job.Job.Recovery.LockKeys...)
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
		return nil, fail("RECOVERY_BINDING_CHANGED", "Recovery requires review because the server binding changed.")
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
	return action(server)
}

func bytesReader(data []byte) *bytes.Reader { return bytes.NewReader(data) }
