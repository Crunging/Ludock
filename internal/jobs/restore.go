package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
)

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
	if len(job.Job.Recovery.Restore) == 0 && !job.Job.Recovery.DataUnsafe {
		return nil, nil
	}
	var state restoreRecovery
	decoder := json.NewDecoder(bytesReader(job.Job.Recovery.Restore))
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
	return job.Progress(phase, func(r *ops.Recovery) {
		r.Restore, r.DataUnsafe = encoded, state.Status == "replacing" || state.Status == "rolling_back"
	}, nil)
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

var errRestoreCapacity = fail("RESTORE_CAPACITY", "A data root lacks space for restore staging and the configured reserve.")

// assertStagingRoom refuses, before the server stops, a restore that cannot
// fit: roots on one filesystem must fit together above the reserve. Staging
// checks again as it runs.
func (j *Jobs) assertStagingRoom(ctx context.Context, server *servers.Context, job *ops.Context, roots []backups.Root,
	staged map[string]int64, reserve int64, assertAccess func() error) error {
	data, err := j.Backups.Helpers.NewDataHelper(ctx, server, true, job.Job.ID)
	if err != nil {
		return err
	}
	defer data.Cleanup()
	return j.stagingRoom(ctx, data, server, job, roots, staged, reserve, assertAccess)
}

// stagingRoom requires every filesystem to hold its roots' staged bytes
// together above the reserve: all roots are staged before any is replaced.
func (j *Jobs) stagingRoom(ctx context.Context, data *backups.DataHelper, server *servers.Context, job *ops.Context,
	roots []backups.Root, staged map[string]int64, reserve int64, assertAccess func() error) error {
	needed, available := map[string]int64{}, map[string]float64{}
	for _, root := range roots {
		destination, err := backups.HelperRoot(server, root)
		if err != nil {
			return err
		}
		space, err := j.restoreStep(ctx, data, destination, job, "space", assertAccess)
		if err != nil {
			return err
		}
		free, ok := number(space["availableBytes"])
		if !ok {
			return errRestoreCapacity
		}
		device, _ := text(space["device"])
		if device == "" {
			device = "root:" + root.ID
		}
		needed[device] += staged[root.ID]
		available[device] = free
	}
	for device, bytes := range needed {
		if available[device] < float64(bytes+reserve) {
			return errRestoreCapacity
		}
	}
	return nil
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
	// The archive is validated once its locks are held.
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
			if err := j.assertStagingRoom(ctx, server, job, row.Roots, staged, settings.ReserveBytes, assertAccess); err != nil {
				return nil, err
			}
			if err := j.stopForData(ctx, server, job); err != nil {
				return nil, err
			}
			safety, err := j.createStoppedBackup(ctx, server, job)
			if err != nil {
				return nil, err
			}
			if err := job.Progress("restore_staging", nil, nil); err != nil {
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
			if err := job.Progress("restore_staging", func(r *ops.Recovery) { r.DataHelperID = data.Container }, nil); err != nil {
				return nil, err
			}
			if err := j.assertStopped(ctx, server, job); err != nil {
				return nil, err
			}
			// Extraction also stops at the reserve as it writes.
			if err := j.stagingRoom(ctx, data, server, job, row.Roots, staged, settings.ReserveBytes, assertAccess); err != nil {
				return nil, err
			}
			var targets []backups.StageTarget
			for _, root := range row.Roots {
				destination, err := backups.HelperRoot(server, root)
				if err != nil {
					return nil, err
				}
				targets = append(targets, backups.StageTarget{Root: root, Path: destination})
			}
			for _, target := range targets {
				if err := assertAccess(); err != nil {
					return nil, err
				}
				if _, err := j.restoreStep(ctx, data, target.Path, job, "stage", assertAccess); err != nil {
					return nil, err
				}
			}
			if err := j.assertStopped(ctx, server, job); err != nil {
				return nil, err
			}
			if err := assertAccess(); err != nil {
				return nil, err
			}
			if err := j.Backups.Helpers.ExtractToStage(ctx, row.Destination(), row.ID, data.Container, targets, stageName(job),
				row.Roots, settings.MaxBytes, settings.ReserveBytes, row.Checksum, assertAccess); err != nil {
				return nil, err
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
		if err := job.Progress(job.Job.Phase, func(r *ops.Recovery) { r.DataHelperID = fresh.Container }, nil); err != nil {
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
			if err := job.Progress("recovering", func(r *ops.Recovery) { r.DataHelperID = data.Container }, nil); err != nil {
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
