package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/helper"
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
		if _, recorded := job.Job.Recovery["initialRunning"]; !recorded {
			stop := stopRecovery(info)
			stop["initialRunning"] = stop["stopStarted"]
			stop["containerId"] = server.Container.ID
			stop["fingerprint"] = server.Logical.BindingFingerprint
			stop["bindingRevision"] = server.Logical.BindingRevision
			stop["lockKeys"] = server.LockKeys
			stop["dataSafe"] = true
			if err := job.Progress("stopping", stop, nil); err != nil {
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
	if err := job.Progress("stopped", map[string]any{"stoppedStartedAt": stopped.State.StartedAt, "stopSettled": true}, nil); err != nil {
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
	observed, info, err := j.Servers.Observer.Inspect(ctx, server.Container.ID)
	if err != nil {
		return err
	}
	if _, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, observed.Observation, server.Logical.BindingRevision); err != nil {
		return err
	}
	expected, _ := text(job.Job.Recovery["stoppedStartedAt"])
	if info.State.Running || info.State.Status == "running" || info.State.Paused || info.State.Restarting || info.State.StartedAt != expected {
		return fail("EXTERNAL_SERVER_CHANGE", "The server was started or changed by another manager during the operation. Review its state before retrying.")
	}
	helperID, _ := text(job.Job.Recovery["dataHelperId"])
	return j.Backups.AssertNoOtherWriters(ctx, server, helperID)
}

func stopPending(job *ops.Context) bool {
	return job.Job.Recovery["stopStarted"] == true && job.Job.Recovery["stopSettled"] != true
}

func (j *Jobs) waitForStopDeadline(ctx context.Context, job *ops.Context) error {
	deadline, ok := number(job.Job.Recovery["stopRecoveryDeadlineAt"])
	now := float64(time.Now().UnixMilli())
	if !ok || deadline <= now {
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

// observeDuringStop confirms the binding. While an interrupted stop may still
// run, a daemon failure is not evidence it settled: retry until the saved
// deadline, but never across an identity mismatch.
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
		initial, ok := number(job.Job.Recovery["stopInitialStartedAt"])
		if observed.Container.State == "running" && ok && observed.Container.StateSince != 0 && float64(observed.Container.StateSince) != initial {
			return nil, job.Progress("stop_reconciled", map[string]any{"stopSettled": true}, nil)
		}
		if err := j.waitForStopDeadline(ctx, job); err != nil {
			return nil, err
		}
		if observed, err = j.observeDuringStop(ctx, job, server.Container.ID, server.Logical); err != nil {
			return nil, err
		}
	}
	if err := job.Progress("stop_reconciled", map[string]any{"stopSettled": true}, nil); err != nil {
		return nil, err
	}
	return observed, nil
}

// restoreInitialState starts a server that was running when the operation
// stopped it, once its data is safe. A start that reached Docker is never
// repeated.
func (j *Jobs) restoreInitialState(ctx context.Context, server *servers.Context, job *ops.Context) error {
	dataSafe := job.Job.Recovery["dataSafe"] != false
	if job.Job.Kind == "restore" {
		restore, err := restoreState(job)
		if err != nil {
			return err
		}
		if restore != nil {
			dataSafe = restore.Status != "replacing" && restore.Status != "rolling_back"
		}
	}
	if job.Job.Recovery["initialRunning"] != true || !dataSafe || job.Job.Recovery["stateRestored"] == true {
		return nil
	}
	observed, err := j.settleInterruptedStop(ctx, server, job)
	if err != nil {
		return err
	}
	if observed == nil {
		return job.Progress("state_restored", map[string]any{"stateRestored": true}, nil)
	}
	if observed.Container.State != "running" {
		if job.Job.Recovery["startRequested"] == true {
			info, err := j.Docker.Inspect(ctx, server.Container.ID)
			if err != nil {
				return err
			}
			if expected, _ := text(job.Job.Recovery["stoppedStartedAt"]); info.State.StartedAt != expected {
				return nil
			}
		}
		// Persist before dispatch, so recovery recognizes a start that reached Docker.
		if err := job.Progress("restarting", map[string]any{"startRequested": true}, nil); err != nil {
			return err
		}
		if err := j.Servers.Observer.ChangeState(ctx, server.Container.ID, servers.Start, func(observation identity.Observation, _ *docker.ContainerInspect) error {
			_, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, observation, server.Logical.BindingRevision)
			return err
		}); err != nil {
			return err
		}
	}
	if err := job.Progress("state_restored", map[string]any{"stateRestored": true}, nil); err != nil {
		return err
	}
	return j.Monitor.Suppress(ctx, server.Logical.ID)
}

// keepStoppedAfterFailedBackup leaves the server stopped, since starting
// could update the game and change the world, and says so once stopped.
func (j *Jobs) keepStoppedAfterFailedBackup(ctx context.Context, server *servers.Context, job *ops.Context) error {
	observed, err := j.settleInterruptedStop(ctx, server, job)
	if err != nil || observed == nil || !slices.Contains(stoppedStates, observed.Container.State) {
		return err
	}
	j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, server.Logical.ID, true))
	j.notify("operation:"+job.Job.ID, observed.Container.DisplayName+": the backup failed, so the server was left stopped. Review the operation, then start it.")
	return nil
}

// createStoppedBackup writes a backup of a stopped server. The caller holds
// every data and storage lock and owns restoring the running state.
func (j *Jobs) createStoppedBackup(ctx context.Context, server *servers.Context, job *ops.Context) (*backups.Backup, error) {
	settings, err := j.Backups.ForBackup(ctx)
	if err != nil {
		return nil, err
	}
	if err := j.Backups.AssertSeparateDestination(ctx, server.Observation, settings); err != nil {
		return nil, err
	}
	if err := j.assertStopped(ctx, server, job); err != nil {
		return nil, err
	}
	if err := j.Backups.Storage.AssertSpace(settings.Destination, settings.ReserveBytes, 0); err != nil {
		return nil, err
	}
	used, err := j.Backups.ArchiveBytes(ctx)
	if err != nil {
		return nil, err
	}
	available, err := backups.Remaining(settings, used)
	if err != nil {
		return nil, err
	}
	id := identity.NewUUID()
	if err := job.Progress("backing_up", map[string]any{"backupId": id, "backupDestination": settings.Destination, "backupComplete": false}, nil); err != nil {
		return nil, err
	}
	var data *backups.DataHelper
	recorded := false
	defer func() {
		if data != nil {
			data.Cleanup()
		}
		// Recovery removes helpers by label, so a stale ID only costs a lookup.
		if err := job.Progress(job.Job.Phase, map[string]any{"dataHelperId": nil}, nil); err != nil {
			log.Warn("Could not save a backup helper's removal; recovery will look for it again")
		}
	}()
	backup, err := func() (*backups.Backup, error) {
		if err := j.dataAuthority(ctx, server, job); err != nil {
			return nil, err
		}
		data, err = j.Backups.Helpers.NewDataHelper(ctx, server, true, job.Job.ID)
		if err != nil {
			return nil, err
		}
		if err := job.Progress("backing_up", map[string]any{"dataHelperId": data.Container}, nil); err != nil {
			return nil, err
		}
		var mu sync.Mutex
		var stoppedFailure error
		assertCopy := func() error {
			mu.Lock()
			failure := stoppedFailure
			mu.Unlock()
			if failure != nil {
				return failure
			}
			if err := j.assertStopped(ctx, server, job); err != nil {
				return err
			}
			return j.dataAuthority(ctx, server, job)
		}
		watch, stop := context.WithCancel(ctx)
		watched := make(chan struct{})
		go func() {
			defer close(watched)
			ticker := time.NewTicker(j.poll())
			defer ticker.Stop()
			for {
				select {
				case <-watch.Done():
					return
				case <-ticker.C:
					if err := assertCopy(); err != nil {
						mu.Lock()
						if stoppedFailure == nil {
							stoppedFailure = err
						}
						mu.Unlock()
						// Stop the copy at once; cleanup also runs later.
						data.Cleanup()
						return
					}
				}
			}
		}()
		size, checksum, err := j.Backups.Helpers.WriteSnapshot(ctx, server, data, settings, id, available, assertCopy,
			func() error { return j.dataAuthority(ctx, server, job) })
		stop()
		<-watched
		mu.Lock()
		failure := stoppedFailure
		mu.Unlock()
		if failure != nil {
			return nil, failure
		}
		if err != nil {
			return nil, err
		}
		if _, err := j.Backups.Storage.Validate(settings.Destination, id, data.Roots, settings.MaxBytes, checksum); err != nil {
			return nil, err
		}
		if err := assertCopy(); err != nil {
			return nil, err
		}
		backup := &backups.Backup{ID: id, ServerID: server.Logical.ID, Roots: data.Roots, Size: size, Checksum: checksum,
			CreatedAt: time.Now().UnixMilli(), State: "complete"}
		if err := j.Backups.Record(ctx, *backup, server.Logical.BindingFingerprint, settings.Destination); err != nil {
			return nil, err
		}
		recorded = true
		// The committed backup row is what recovery trusts; this marker only
		// saves it a lookup.
		if err := job.Progress("backup_complete", map[string]any{"backupComplete": true}, nil); err != nil {
			log.Warn("Could not save a backup's completion marker; its record is already saved")
		}
		keep, _ := text(job.Job.Input["backupId"])
		if err := j.Backups.Prune(ctx, server.Logical.ID, settings.RetentionCount, keep); err != nil {
			return nil, err
		}
		return backup, nil
	}()
	if err != nil && !recorded {
		removeErr := j.Backups.Storage.Remove(settings.Destination, id, nil)
		if removeErr == nil {
			removeErr = j.Backups.Storage.RemovePartial(settings.Destination, id)
		}
		// An archive that is neither recorded nor removed would use storage
		// unseen by retention and capacity checks; recording it as deleting
		// leaves its removal to Prune and Delete.
		if removeErr != nil {
			if recordErr := j.Backups.RecordDeleting(context.WithoutCancel(ctx), id, server.Logical.ID, server.Logical.BindingFingerprint,
				settings.Destination); recordErr != nil {
				log.Warn("Could not remove or record a failed backup's archive; it may use backup storage until removed")
			}
		}
	}
	return backup, err
}

func (j *Jobs) runBackup(ctx context.Context, job *ops.Context) (map[string]any, error) {
	server, err := j.resolve(ctx, job, authz.BackupsCreate, true)
	if err != nil {
		return nil, err
	}
	return job.WithLocks(append(slices.Clone(server.LockKeys), "backups:storage"), func() (result map[string]any, err error) {
		if err := j.assertCapability(ctx, job, authz.BackupsCreate); err != nil {
			return nil, err
		}
		if err := j.Backups.AssertReady(ctx, server); err != nil {
			return nil, err
		}
		defer func() {
			if restoreErr := j.restoreInitialState(context.WithoutCancel(ctx), server, job); restoreErr != nil && err == nil {
				err = restoreErr
			}
		}()
		backup, err := func() (*backups.Backup, error) {
			if err := j.stopForData(ctx, server, job); err != nil {
				return nil, err
			}
			return j.createStoppedBackup(ctx, server, job)
		}()
		if err != nil {
			j.notify("backup-failed:"+job.Job.ID, "Backup failed for "+server.Container.DisplayName+". Review its operation details.")
			return nil, err
		}
		return map[string]any{"backupId": backup.ID, "size": backup.Size, "checksum": backup.Checksum}, nil
	})
}

// backupCompleted: the row is committed before its completion marker, so a
// crash between the two still leaves a complete backup. A failed lookup
// counts as incomplete, which keeps the server stopped.
func (j *Jobs) backupCompleted(ctx context.Context, job *ops.Context) bool {
	if job.Job.Recovery["backupComplete"] == true {
		return true
	}
	id, ok := text(job.Job.Recovery["backupId"])
	if !ok {
		return false
	}
	complete, err := j.Backups.Exists(ctx, id, true)
	return err == nil && complete
}

func (j *Jobs) removeUnpublished(ctx context.Context, job *ops.Context) error {
	id, okID := text(job.Job.Recovery["backupId"])
	destination, okDestination := text(job.Job.Recovery["backupDestination"])
	if !okID || !okDestination {
		return nil
	}
	// Only a backup known to have no row is unpublished.
	if recorded, err := j.Backups.Exists(ctx, id, false); err != nil || recorded {
		return err
	}
	if err := j.Backups.Storage.Remove(destination, id, nil); err != nil {
		return err
	}
	return j.Backups.Storage.RemovePartial(destination, id)
}

// recoveryLocks reclaims the saved shared-volume and path locks before any
// Docker request can fail or hang.
func recoveryLocks(job *ops.Context) ([]string, error) {
	saved, ok := stringList(job.Job.Recovery["lockKeys"])
	if !ok {
		return nil, fail("INVALID_OPERATION_STATE", "Saved data operation locks are invalid; administrator review is required.")
	}
	return append([]string{"server:" + job.Job.ServerID, "backups:storage"}, saved...), nil
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
	return job.Progress("recovering", map[string]any{"dataHelperId": nil}, nil)
}

func (j *Jobs) recoveryContext(ctx context.Context, job *ops.Context) (*servers.Context, error) {
	if err := j.removeOperationHelpers(ctx, job); err != nil {
		return nil, err
	}
	containerID, ok := text(job.Job.Recovery["containerId"])
	if !ok {
		return nil, nil
	}
	logical, err := j.Identity.Get(ctx, job.Job.ServerID)
	if err != nil {
		return nil, err
	}
	fingerprint, _ := text(job.Job.Recovery["fingerprint"])
	if logical == nil || logical.Status != identity.StatusActive || logical.ContainerID == "" || logical.ContainerID != containerID ||
		logical.BindingFingerprint != fingerprint {
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
	held, err := recoveryLocks(job)
	if err != nil {
		return err
	}
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

func (j *Jobs) recoverBackup(ctx context.Context, job *ops.Context, startServer bool) error {
	return j.withRecoveryLocks(ctx, job, func(server *servers.Context) error {
		if err := j.removeUnpublished(ctx, job); err != nil {
			return err
		}
		if startServer {
			return j.restoreInitialState(ctx, server, job)
		}
		return j.keepStoppedAfterFailedBackup(ctx, server, job)
	})
}

// Restore journals.

type journal struct {
	Root  backups.Root `json:"root"`
	Phase string       `json:"phase"`
}

type restoreRecovery struct {
	Status string    `json:"status"`
	Roots  []journal `json:"roots,omitempty"`
}

var errJournal = fail("INVALID_RESTORE_JOURNAL", "Restore recovery records are invalid. Keep the server stopped and review its recovery state.")

func restoreState(job *ops.Context) (*restoreRecovery, error) {
	value, present := job.Job.Recovery["restore"]
	if !present && job.Job.Recovery["dataSafe"] != false {
		return nil, nil
	}
	encoded, _ := json.Marshal(value)
	var state restoreRecovery
	decoder := json.NewDecoder(bytesReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&state) != nil {
		return nil, errJournal
	}
	switch state.Status {
	case "staging", "replacing", "rolling_back", "committed":
		if len(state.Roots) < 1 || len(state.Roots) > 8 {
			return nil, errJournal
		}
		seen := map[string]bool{}
		for _, entry := range state.Roots {
			if seen[entry.Root.ID] || !slices.Contains([]string{"staging", "moving_old", "old_moved", "replaced", "rolling_back", "rolled_back"}, entry.Phase) ||
				(state.Status == "staging" && entry.Phase != "staging") || (state.Status == "committed" && entry.Phase != "replaced") {
				return nil, errJournal
			}
			seen[entry.Root.ID] = true
		}
	case "cleaned", "rolled_back":
		if len(state.Roots) > 0 {
			return nil, errJournal
		}
	default:
		return nil, errJournal
	}
	return &state, nil
}

func journals(job *ops.Context) ([]journal, error) {
	state, err := restoreState(job)
	if err != nil || state == nil {
		return nil, err
	}
	return state.Roots, nil
}

// saveRestore persists the restore journal. Callers stop on an error: the
// next data change must never run ahead of the journal a restart would read.
func saveRestore(job *ops.Context, state restoreRecovery, phase string) error {
	if phase == "" {
		phase = "restore_" + state.Status
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	var generic any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		return err
	}
	return job.Progress(phase, map[string]any{"restore": generic, "dataSafe": state.Status != "replacing" && state.Status != "rolling_back"}, nil)
}

func persistJournal(job *ops.Context, index int, phase string) error {
	state, err := restoreState(job)
	if err != nil || state == nil || len(state.Roots) == 0 {
		return errJournal
	}
	state.Roots[index].Phase = phase
	return saveRestore(job, *state, "restore_"+phase)
}

func stageName(job *ops.Context) string { return ".ludock-restore-" + job.Job.ID }

func (j *Jobs) restoreStep(ctx context.Context, data *backups.DataHelper, root string, job *ops.Context, operation string, assertAccess func() error) (map[string]any, error) {
	out, err := j.Backups.Helpers.Exec(ctx, data.Container, helper.Command("restore", helper.RestoreRequest{Root: root, Stage: stageName(job),
		Operation: operation}), assertAccess)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	json.Unmarshal(out, &result)
	return result, nil
}

func indexOf(entries []journal, id string) int {
	return slices.IndexFunc(entries, func(entry journal) bool { return entry.Root.ID == id })
}

func (j *Jobs) rollbackRestore(ctx context.Context, server *servers.Context, job *ops.Context, data *backups.DataHelper) error {
	entries, err := journals(job)
	if err != nil {
		return err
	}
	if err := saveRestore(job, restoreRecovery{Status: "rolling_back", Roots: entries}, ""); err != nil {
		return err
	}
	for position := len(entries) - 1; position >= 0; position-- {
		entry := entries[position]
		if err := j.assertStopped(ctx, server, job); err != nil {
			return err
		}
		rootPath, err := backups.HelperRoot(server, entry.Root)
		if err != nil {
			return err
		}
		if entry.Phase == "old_moved" || entry.Phase == "replaced" {
			if _, err := j.restoreStep(ctx, data, rootPath, job, "rollbackClean", nil); err != nil {
				return err
			}
		}
		if entry.Phase != "staging" && entry.Phase != "rolled_back" {
			current, _ := journals(job)
			// Persist after removing new data and before returning any old
			// entry: a restart midway must never delete returned originals.
			if err := persistJournal(job, indexOf(current, entry.Root.ID), "rolling_back"); err != nil {
				return err
			}
			if _, err := j.restoreStep(ctx, data, rootPath, job, "rollbackOld", nil); err != nil {
				return err
			}
			current, _ = journals(job)
			if err := persistJournal(job, indexOf(current, entry.Root.ID), "rolled_back"); err != nil {
				return err
			}
		}
		if _, err := j.restoreStep(ctx, data, rootPath, job, "cleanup", nil); err != nil {
			return err
		}
	}
	return saveRestore(job, restoreRecovery{Status: "rolled_back"}, "")
}

func (j *Jobs) cleanupRestore(ctx context.Context, server *servers.Context, job *ops.Context, data *backups.DataHelper) error {
	entries, err := journals(job)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		rootPath, err := backups.HelperRoot(server, entry.Root)
		if err != nil {
			return err
		}
		if _, err := j.restoreStep(ctx, data, rootPath, job, "cleanup", nil); err != nil {
			return err
		}
	}
	return saveRestore(job, restoreRecovery{Status: "cleaned"}, "")
}

func rootsEqual(left []backups.Root, right []backups.Root) bool {
	return slices.Equal(left, right)
}

func (j *Jobs) runRestore(ctx context.Context, job *ops.Context) (map[string]any, error) {
	server, err := j.resolve(ctx, job, authz.BackupsRestore, true)
	if err != nil {
		return nil, err
	}
	if confirmation, _ := text(job.Job.Input["confirmation"]); confirmation != server.Container.DisplayName {
		return nil, fail("RESTORE_CONFIRMATION", "Type the server's display name to confirm this restore.")
	}
	backupID, _ := text(job.Job.Input["backupId"])
	row, err := j.Backups.Get(ctx, server.Logical.ID, backupID)
	if err != nil {
		return nil, err
	}
	if row.Fingerprint() != server.Logical.BindingFingerprint {
		return nil, fail("BACKUP_BINDING_CHANGED", "This backup belongs to a different server configuration. Review its data roots before restoring.")
	}
	var current []backups.Root
	for _, root := range server.Container.FileRoots {
		current = append(current, backups.Root{ID: root.ID, Path: root.Path})
	}
	if !rootsEqual(row.Roots, current) {
		return nil, fail("BACKUP_ROOT_CHANGED", "The current data roots do not match this backup.")
	}
	settings, err := j.Backups.ForBackup(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := j.Backups.Storage.Validate(row.Destination(), row.ID, row.Roots, settings.MaxBytes, row.Checksum); err != nil {
		return nil, err
	}
	return job.WithLocks(append(slices.Clone(server.LockKeys), "backups:storage"), func() (result map[string]any, err error) {
		var data *backups.DataHelper
		assertAccess := func() error { return j.dataAuthority(ctx, server, job) }
		defer func() {
			if data != nil {
				data.Cleanup()
			}
			if restoreErr := j.restoreInitialState(context.WithoutCancel(ctx), server, job); restoreErr != nil && err == nil {
				err = restoreErr
			}
		}()
		result, err = func() (map[string]any, error) {
			if err := j.assertCapability(ctx, job, authz.BackupsRestore); err != nil {
				return nil, err
			}
			staged, err := j.Backups.Storage.Validate(row.Destination(), row.ID, row.Roots, settings.MaxBytes, row.Checksum)
			if err != nil {
				return nil, err
			}
			if err := j.stopForData(ctx, server, job); err != nil {
				return nil, err
			}
			safety, err := j.createStoppedBackup(ctx, server, job)
			if err != nil {
				return nil, err
			}
			if err := job.Progress("restore_staging", map[string]any{"safetyBackupId": safety.ID}, nil); err != nil {
				return nil, err
			}
			var entries []journal
			for _, root := range row.Roots {
				entries = append(entries, journal{Root: root, Phase: "staging"})
			}
			if err := saveRestore(job, restoreRecovery{Status: "staging", Roots: entries}, ""); err != nil {
				return nil, err
			}
			if err := assertAccess(); err != nil {
				return nil, err
			}
			if data, err = j.Backups.Helpers.NewDataHelper(ctx, server, false, job.Job.ID); err != nil {
				return nil, err
			}
			if err := job.Progress("restore_staging", map[string]any{"dataHelperId": data.Container}, nil); err != nil {
				return nil, err
			}
			for _, root := range row.Roots {
				if err := j.assertStopped(ctx, server, job); err != nil {
					return nil, err
				}
				destination, err := backups.HelperRoot(server, root)
				if err != nil {
					return nil, err
				}
				space, err := j.restoreStep(ctx, data, destination, job, "space", assertAccess)
				if err != nil {
					return nil, err
				}
				// Roots staged earlier may share this filesystem and have
				// already taken their space, so each needs only its own.
				available, ok := number(space["availableBytes"])
				if !ok || available < float64(staged[root.ID]+settings.ReserveBytes) {
					return nil, fail("RESTORE_CAPACITY", "A data root lacks space for restore staging and the configured reserve.")
				}
				if err := assertAccess(); err != nil {
					return nil, err
				}
				if _, err := j.restoreStep(ctx, data, destination, job, "stage", assertAccess); err != nil {
					return nil, err
				}
				if err := assertAccess(); err != nil {
					return nil, err
				}
				if err := j.Backups.Helpers.ExtractToStage(ctx, row.Destination(), row.ID, data.Container, root, destination, stageName(job),
					row.Roots, settings.MaxBytes, settings.ReserveBytes, row.Checksum, assertAccess); err != nil {
					return nil, err
				}
			}
			if err := assertAccess(); err != nil {
				return nil, err
			}
			current, _ := journals(job)
			if err := saveRestore(job, restoreRecovery{Status: "replacing", Roots: current}, ""); err != nil {
				return nil, err
			}
			for index, root := range row.Roots {
				if err := j.assertStopped(ctx, server, job); err != nil {
					return nil, err
				}
				if err := assertAccess(); err != nil {
					return nil, err
				}
				destination, err := backups.HelperRoot(server, root)
				if err != nil {
					return nil, err
				}
				if err := persistJournal(job, index, "moving_old"); err != nil {
					return nil, err
				}
				if _, err := j.restoreStep(ctx, data, destination, job, "moveOld", assertAccess); err != nil {
					return nil, err
				}
				if err := persistJournal(job, index, "old_moved"); err != nil {
					return nil, err
				}
				if err := assertAccess(); err != nil {
					return nil, err
				}
				if _, err := j.restoreStep(ctx, data, destination, job, "moveNew", assertAccess); err != nil {
					return nil, err
				}
				if err := persistJournal(job, index, "replaced"); err != nil {
					return nil, err
				}
			}
			if err := j.assertStopped(ctx, server, job); err != nil {
				return nil, err
			}
			if err := assertAccess(); err != nil {
				return nil, err
			}
			current, _ = journals(job)
			if err := saveRestore(job, restoreRecovery{Status: "committed", Roots: current}, ""); err != nil {
				return nil, err
			}
			if err := j.cleanupRestore(ctx, server, job, data); err != nil {
				return nil, err
			}
			j.notify("restore-result:"+job.Job.ID, "Restore completed for "+server.Container.DisplayName+".")
			return map[string]any{"restoredBackupId": row.ID, "safetyBackupId": safety.ID}, nil
		}()
		if err == nil {
			return result, nil
		}
		state, _ := restoreState(job)
		if data != nil && (state == nil || (state.Status != "committed" && state.Status != "cleaned")) {
			if rollbackErr := j.rollbackAfterFailure(context.WithoutCancel(ctx), server, job, &data); rollbackErr != nil {
				err = errors.Join(err, rollbackErr)
			}
		}
		j.notify("restore-result:"+job.Job.ID, "Restore failed for "+server.Container.DisplayName+". Review its recovery state.")
		return nil, publicFirst(err)
	})
}

// rollbackAfterFailure rolls back through a fresh helper when the command
// that failed stopped the old one, as recovery does.
func (j *Jobs) rollbackAfterFailure(ctx context.Context, server *servers.Context, job *ops.Context, data **backups.DataHelper) error {
	if running, err := j.Backups.Helpers.IsRunning(ctx, (*data).Container); err != nil || !running {
		(*data).Cleanup()
		*data = nil
		if err := j.assertStopped(ctx, server, job); err != nil {
			return err
		}
		fresh, err := j.Backups.Helpers.NewDataHelper(ctx, server, false, job.Job.ID)
		if err != nil {
			return err
		}
		*data = fresh
		if err := job.Progress(job.Job.Phase, map[string]any{"dataHelperId": fresh.Container}, nil); err != nil {
			return err
		}
	}
	return j.rollbackRestore(ctx, server, job, *data)
}

// publicFirst keeps the first public error of a joined error.
func publicFirst(err error) error {
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		for _, inner := range joined.Unwrap() {
			if _, ok := apperr.As(inner); ok {
				return inner
			}
		}
	}
	return err
}

func (j *Jobs) recoverRestore(ctx context.Context, job *ops.Context) error {
	return j.withRecoveryLocks(ctx, job, func(server *servers.Context) error {
		entries, err := journals(job)
		if err != nil {
			return err
		}
		// Recovery uses exactly the roots of the revalidated binding.
		if len(entries) > 0 {
			if len(entries) != len(server.Container.FileRoots) {
				return fail("INVALID_RESTORE_JOURNAL", "Restore recovery roots do not match this server. Keep it stopped and review its recovery state.")
			}
			for _, entry := range entries {
				if !slices.ContainsFunc(server.Container.FileRoots, func(root files.Root) bool { return root.ID == entry.Root.ID && root.Path == entry.Root.Path }) {
					return fail("INVALID_RESTORE_JOURNAL", "Restore recovery roots do not match this server. Keep it stopped and review its recovery state.")
				}
			}
		}
		if err := j.removeUnpublished(ctx, job); err != nil {
			return err
		}
		if len(entries) > 0 {
			if err := j.assertStopped(ctx, server, job); err != nil {
				return err
			}
			data, err := j.Backups.Helpers.NewDataHelper(ctx, server, false, job.Job.ID)
			if err != nil {
				return err
			}
			if err := job.Progress("recovering", map[string]any{"dataHelperId": data.Container}, nil); err != nil {
				data.Cleanup()
				return err
			}
			state, _ := restoreState(job)
			if state != nil && state.Status == "committed" {
				err = j.cleanupRestore(ctx, server, job, data)
			} else {
				err = j.rollbackRestore(ctx, server, job, data)
			}
			data.Cleanup()
			if err != nil {
				return err
			}
		}
		return j.restoreInitialState(ctx, server, job)
	})
}

func bytesReader(data []byte) *bytes.Reader { return bytes.NewReader(data) }
