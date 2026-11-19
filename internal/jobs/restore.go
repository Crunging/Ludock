package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
)

type journal struct {
	Root backups.Root `json:"root"`
	// Item is the one path below Root that a selective restore replaces.
	Item  string `json:"item,omitempty"`
	Phase string `json:"phase"`
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
			if entry.Item != "" {
				if clean, err := backups.CleanPath(entry.Item); err != nil || clean == "" || len(state.Roots) != 1 {
					return nil, errJournal
				}
			}
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

// removalLimit bounds a restore step that deletes a whole data root's old or
// failed copy.
const removalLimit = 30 * time.Minute

func (j *Jobs) restoreStep(ctx context.Context, data *backups.DataHelper, root, item string, job *ops.Context, operation string, assertAccess func() error) (map[string]any, error) {
	limit := 2 * time.Minute
	if operation == "cleanup" || operation == "rollbackClean" {
		limit = removalLimit
	}
	out, err := j.Backups.Helpers.ExecWithin(ctx, data.Container, helper.Command("restore", helper.RestoreRequest{Root: root, Stage: stageName(job),
		Operation: operation, Item: item}), assertAccess, limit)
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
func (j *Jobs) assertStagingRoom(ctx context.Context, server *servers.Context, job *ops.Context, roots []backups.Root, item string,
	staged map[string]int64, reserve int64, assertAccess func() error) error {
	data, err := j.Backups.Helpers.NewDataHelper(ctx, server, true, job.Job.ID)
	if err != nil {
		return err
	}
	defer data.Cleanup()
	if err := j.stagingRoom(ctx, data, server, job, roots, staged, reserve, assertAccess); err != nil {
		return err
	}
	if item == "" {
		return nil
	}
	destination, err := backups.HelperRoot(server, roots[0])
	if err != nil {
		return err
	}
	checked, err := j.restoreStep(ctx, data, destination, item, job, "check", assertAccess)
	if err != nil {
		return fail("RESTORE_ITEM_UNSAFE", "The current copy can’t be replaced safely: something on its path is a link, a special file, or another disk.")
	}
	if checked["parent"] != true {
		return fail("RESTORE_PARENT_MISSING", "The folder that held this item no longer exists. Restore the folder that contains it instead.")
	}
	return nil
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
		space, err := j.restoreStep(ctx, data, destination, "", job, "space", assertAccess)
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
			if _, err := j.restoreStep(ctx, data, rootPath, entry.Item, job, "rollbackClean", nil); err != nil {
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
			if _, err := j.restoreStep(ctx, data, rootPath, entry.Item, job, "rollbackOld", nil); err != nil {
				return err
			}
			current, _ = journals(job)
			if err := persistJournal(job, indexOf(current, entry.Root.ID), "rolled_back"); err != nil {
				return err
			}
		}
		if _, err := j.restoreStep(ctx, data, rootPath, "", job, "cleanup", nil); err != nil {
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
		if _, err := j.restoreStep(ctx, data, rootPath, "", job, "cleanup", nil); err != nil {
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
	// A selective restore replaces one item of one root; a full one, every root.
	roots := row.Roots
	item, _ := text(job.Job.Input["item"])
	if rootID, _ := text(job.Job.Input["root"]); rootID != "" || item != "" {
		index := slices.IndexFunc(row.Roots, func(root backups.Root) bool { return root.ID == rootID })
		if clean, err := backups.CleanPath(item); err != nil || clean == "" || index < 0 {
			return nil, fail("UNSAFE_PATH", "That path isn’t allowed.")
		}
		roots = row.Roots[index : index+1]
	}
	what := "Restore"
	if item != "" {
		what = "Restore of one item"
	}
	settings, err := j.Backups.ForBackup(ctx)
	if err != nil {
		return nil, err
	}
	// The archive is validated once its locks are held.
	return job.WithLocks(append(slices.Clone(server.LockKeys), "backups:storage"), func() (result map[string]any, err error) {
		var data *backups.DataHelper
		var restored map[string]any
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
			var staged map[string]int64
			if item == "" {
				staged, err = j.Backups.Storage.Validate(row.Destination(), row.ID, row.Roots, settings.MaxBytes, row.Checksum)
			} else {
				var bytes int64
				bytes, err = j.Backups.Storage.ValidateItem(row.Destination(), row.ID, row.Roots, settings.MaxBytes, row.Checksum, roots[0].ID, item)
				staged = map[string]int64{roots[0].ID: bytes}
			}
			if err != nil {
				return nil, err
			}
			if err := j.assertStagingRoom(ctx, server, job, roots, item, staged, settings.ReserveBytes, assertAccess); err != nil {
				return nil, err
			}
			if err := j.stopForData(ctx, server, job); err != nil {
				return nil, err
			}
			// One item's replaced copy is kept in the stage until the swap
			// commits; a whole restore also keeps a safety backup.
			var safety *backups.Backup
			if item == "" {
				if safety, err = j.createStoppedBackup(ctx, server, job); err != nil {
					return nil, err
				}
			}
			if err := job.Progress("restore_staging", nil, nil); err != nil {
				return nil, err
			}
			var entries []journal
			for _, root := range roots {
				entries = append(entries, journal{Root: root, Item: item, Phase: "staging"})
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
			if err := j.stagingRoom(ctx, data, server, job, roots, staged, settings.ReserveBytes, assertAccess); err != nil {
				return nil, err
			}
			var targets []backups.StageTarget
			for _, root := range roots {
				destination, err := backups.HelperRoot(server, root)
				if err != nil {
					return nil, err
				}
				targets = append(targets, backups.StageTarget{Root: root, Path: destination, Item: item})
			}
			for _, target := range targets {
				if err := assertAccess(); err != nil {
					return nil, err
				}
				if _, err := j.restoreStep(ctx, data, target.Path, "", job, "stage", assertAccess); err != nil {
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
			for index, root := range roots {
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
				if _, err := j.restoreStep(ctx, data, destination, item, job, "moveOld", assertAccess); err != nil {
					return nil, err
				}
				if err := persistJournal(job, index, "old_moved"); err != nil {
					return nil, err
				}
				if err := assertAccess(); err != nil {
					return nil, err
				}
				if _, err := j.restoreStep(ctx, data, destination, item, job, "moveNew", assertAccess); err != nil {
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
			restored = map[string]any{"restoredBackupId": row.ID}
			if safety != nil {
				restored["safetyBackupId"] = safety.ID
			} else {
				restored["restoredRoot"], restored["restoredItem"] = roots[0].ID, item
			}
			if err := j.cleanupRestore(ctx, server, job, data); err != nil {
				return nil, err
			}
			j.notify(notify.Results, "restore-result:"+job.Job.ID, what+" completed for "+server.Container.DisplayName+".")
			return restored, nil
		}()
		if err == nil {
			return result, nil
		}
		state, _ := restoreState(job)
		// The restored data is in place; only its staging copy of the old data
		// remains, which blocks every later backup until it is removed.
		if restored != nil && state != nil && state.Status == "committed" {
			if cleanErr := j.cleanupAfterFailure(context.WithoutCancel(ctx), server, job, &data); cleanErr == nil {
				j.notify(notify.Results, "restore-result:"+job.Job.ID, what+" completed for "+server.Container.DisplayName+".")
				return restored, nil
			}
			j.notify(notify.Results, "restore-result:"+job.Job.ID, what+" completed for "+server.Container.DisplayName+
				", but its staging folder could not be removed. Review the operation before backing up.")
			return nil, fail("RESTORE_STAGING_LEFT", "The backup was restored, but Ludock could not remove the folder "+stageName(job)+
				" from the server's data, which holds the replaced data. Delete that folder, then back up the server.")
		}
		if data != nil && (state == nil || (state.Status != "committed" && state.Status != "cleaned")) {
			if rollbackErr := j.rollbackAfterFailure(context.WithoutCancel(ctx), server, job, &data); rollbackErr != nil {
				err = errors.Join(err, rollbackErr)
			}
		}
		j.notify(notify.Results, "restore-result:"+job.Job.ID, what+" failed for "+server.Container.DisplayName+". Review its recovery state.")
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

// cleanupAfterFailure removes a committed restore's staging folders through
// a fresh helper, as rollbackAfterFailure does.
func (j *Jobs) cleanupAfterFailure(ctx context.Context, server *servers.Context, job *ops.Context, data **backups.DataHelper) error {
	if *data != nil {
		(*data).Cleanup()
		*data = nil
	}
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
	return j.cleanupRestore(ctx, server, job, fresh)
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
			if len(entries) != len(server.Container.FileRoots) && entries[0].Item == "" {
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
