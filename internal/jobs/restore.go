package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/locks"
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
	// SetAside, saved before a later undo of a restore that ended partway
	// moves anything, keeps what is live instead of removing it: a server
	// started meanwhile may have written there.
	SetAside bool `json:"setAside,omitempty"`
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

func stageName(job *ops.Context) string { return helper.StageName(job.Job.ID) }

// removalLimit bounds a restore step that deletes a whole data root's old or
// failed copy.
const removalLimit = 30 * time.Minute

func (j *Jobs) restoreStep(ctx context.Context, data *backups.DataHelper, root, item string, job *ops.Context, operation string, assertAccess func() error) (map[string]any, error) {
	limit := 2 * time.Minute
	if operation == "cleanup" || operation == "rollbackClean" || operation == "setAside" {
		limit = removalLimit
	}
	request := helper.RestoreRequest{Root: root, Stage: stageName(job), Operation: operation, Item: item, Base: backups.HelperBaseOf(root)}
	if operation == "setAside" {
		request.Aside = helper.SetAsideName(job.Job.ID)
	}
	out, err := j.Backups.Helpers.ExecWithin(ctx, data.Container, helper.Command("restore", request), assertAccess, limit)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	json.Unmarshal(out, &result)
	return result, nil
}

var errRestoreCapacity = fail("RESTORE_CAPACITY", "The server’s disk doesn’t have room to unpack the backup next to the current data and still keep its minimum free space. Free up space on that disk, then try again.")

// assertStagingRoom refuses, before the server stops, a restore that cannot
// fit, since roots on one filesystem must fit together above the reserve, or
// that would cross a mount the host added. Staging checks again as it runs.
func (j *Jobs) assertStagingRoom(ctx context.Context, server *servers.Context, job *ops.Context, roots []backups.Root, item string,
	staged map[string]int64, reserve int64, assertAccess func() error) error {
	data, err := j.Backups.Helpers.NewDataHelper(ctx, server, true, job.Job.ID)
	if err != nil {
		return err
	}
	defer data.Cleanup()
	// A safety backup takes every root whole; one item's restore moves only
	// what is on the item's path.
	if item == "" || job.Job.Input["createBackup"] == true {
		err = j.Backups.CheckOwnMounts(ctx, server, data, "", assertAccess)
	} else {
		err = j.Backups.Helpers.AssertOwnMounts(ctx, server, &backups.DataHelper{Container: data.Container, Roots: roots}, item, assertAccess)
	}
	if err != nil {
		return err
	}
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

// holdsLive says a root's journal phase may have live data moved: only its
// stage folder exists otherwise.
func holdsLive(entry journal) bool { return entry.Phase != "staging" && entry.Phase != "rolled_back" }

// rollbackRestore undoes a restore, newest root first. The game never reads
// a stage folder, so a root that holds no live restore data is cleaned up
// even if the server was started elsewhere; the others wait until it is
// stopped, and the error says why. beforeLive, when set, runs before the
// first step that changes live data is sent.
func (j *Jobs) rollbackRestore(ctx context.Context, server *servers.Context, job *ops.Context, data *backups.DataHelper, beforeLive func() error) error {
	state, err := restoreState(job)
	if err != nil {
		return err
	}
	var entries []journal
	setAside := false
	if state != nil {
		entries, setAside = state.Roots, state.SetAside
	}
	if err := saveRestore(job, restoreRecovery{Status: "rolling_back", Roots: entries, SetAside: setAside}, ""); err != nil {
		return err
	}
	// The helper's callback runs once its command exists, just before it
	// is sent: a command Docker never created moved nothing.
	live := func() error {
		if beforeLive == nil {
			return nil
		}
		err := beforeLive()
		if err == nil {
			beforeLive = nil
		}
		return err
	}
	var blocked error
	for position := len(entries) - 1; position >= 0; position-- {
		entry := entries[position]
		if holdsLive(entry) {
			if blocked == nil {
				blocked = j.assertStopped(ctx, server, job)
			}
			if blocked != nil {
				continue
			}
		}
		rootPath, err := backups.HelperRoot(server, entry.Root)
		if err != nil {
			return err
		}
		if entry.Phase == "old_moved" || entry.Phase == "replaced" {
			clean := "rollbackClean"
			if setAside {
				clean = "setAside"
			}
			if _, err := j.restoreStep(ctx, data, rootPath, entry.Item, job, clean, live); err != nil {
				return err
			}
		}
		if holdsLive(entry) {
			current, _ := journals(job)
			// Persist after removing new data and before returning any old
			// entry: a restart midway must never delete returned originals.
			if err := persistJournal(job, indexOf(current, entry.Root.ID), "rolling_back"); err != nil {
				return err
			}
			// A server started while new data was removed must not have old
			// data returned under it.
			if blocked = j.assertStopped(ctx, server, job); blocked != nil {
				continue
			}
			if _, err := j.restoreStep(ctx, data, rootPath, entry.Item, job, "rollbackOld", live); err != nil {
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
	if blocked != nil {
		return blocked
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

// MovedInput confirms restoring a backup taken when the server's data lived
// elsewhere on the host.
const MovedInput = "dataMoved"

var errMovedConfirmation = fail("RESTORE_MOVED_CONFIRMATION",
	"This backup was taken when the server’s data lived somewhere else. Confirm restoring it from the server’s backups.")

// restoreRun is one restore's state while it holds its locks.
type restoreRun struct {
	j      *Jobs
	ctx    context.Context
	job    *ops.Context
	server *servers.Context
	row    *backups.Backup
	moved  bool
	// A selective restore replaces one item of one root; a full one, every root.
	roots   []backups.Root
	item    string
	whole   bool
	what    string
	reserve int64
	// resumed says a warning was in progress when Ludock restarted.
	resumed bool
	folder  string
	staged  map[string]int64
	data    *backups.DataHelper
	targets []backups.StageTarget
	// stageMade is set once a stage folder may exist.
	stageMade bool
	safety    *backups.Backup
	// restored is the result, set once the restore commits.
	restored map[string]any
}

func (j *Jobs) runRestore(ctx context.Context, job *ops.Context) (map[string]any, error) {
	r, err := j.planRestore(ctx, job)
	if err != nil {
		return nil, err
	}
	// The archive is validated once its locks are held.
	return job.WithLocks(append(slices.Clone(r.server.LockKeys), locks.StorageKey), func() (result map[string]any, err error) {
		r.resumed = job.Job.Recovery.WarnUntil != 0
		defer func() {
			if r.data != nil {
				r.data.Cleanup()
			}
			if restoreErr := j.restoreInitialState(context.WithoutCancel(ctx), r.server, job); restoreErr != nil && err == nil {
				err = restoreErr
			}
		}()
		if result, err = r.run(); err == nil {
			return result, nil
		}
		return r.failed(err)
	})
}

// planRestore runs the checks that need no locks.
func (j *Jobs) planRestore(ctx context.Context, job *ops.Context) (*restoreRun, error) {
	server, err := j.resolve(ctx, job, authz.BackupsRestore, true)
	if err != nil {
		return nil, err
	}
	item, _ := text(job.Job.Input["item"])
	rootID, _ := text(job.Job.Input["root"])
	whole := rootID == "" && item == ""
	if confirmation, _ := text(job.Job.Input["confirmation"]); whole && confirmation != server.Container.DisplayName {
		return nil, fail("RESTORE_CONFIRMATION", "Type the server's display name to confirm this restore.")
	}
	backupID, _ := text(job.Job.Input["backupId"])
	row, err := j.Backups.Get(ctx, server.Logical.ID, backupID)
	if err != nil {
		return nil, err
	}
	fit := backups.FitFor(row, server)
	if fit.Refusal != "" {
		return nil, fail("BACKUP_FOLDERS_DIFFER", fit.Refusal)
	}
	if fit.Moved && job.Job.Input[MovedInput] != true {
		return nil, errMovedConfirmation
	}
	roots := row.Roots
	if !whole {
		index := slices.IndexFunc(row.Roots, func(root backups.Root) bool { return root.ID == rootID })
		if clean, err := backups.CleanPath(item); err != nil || clean == "" || index < 0 {
			return nil, fail("UNSAFE_PATH", "That path isn’t allowed.")
		}
		roots = row.Roots[index : index+1]
	}
	what := "restore"
	if item != "" {
		what = "restore of one item"
	}
	settings, err := j.Backups.ForBackup(ctx)
	if err != nil {
		return nil, err
	}
	return &restoreRun{j: j, ctx: ctx, job: job, server: server, row: row, moved: fit.Moved, roots: roots, item: item, whole: whole,
		what: what, reserve: settings.ReserveBytes}, nil
}

// run is the restore's phases in order. Every data change is journaled
// before it runs, since recovery acts on the journal alone.
func (r *restoreRun) run() (map[string]any, error) {
	// Players are warned once the archive is validated, which can take
	// minutes; staging reads it again and verifies its checksum before any
	// data is replaced. A warning resumed after a restart of Ludock is waited
	// out first, in validate, since players already heard it.
	if err := r.validate(); err != nil {
		return nil, err
	}
	if !r.resumed {
		if err := r.warn(); err != nil {
			return nil, err
		}
	}
	if err := r.stop(); err != nil {
		return nil, err
	}
	if err := r.job.Progress("restore_staging", nil, nil); err != nil {
		return nil, err
	}
	var entries []journal
	for _, root := range r.roots {
		entries = append(entries, journal{Root: root, Item: r.item, Phase: "staging"})
	}
	if err := saveRestore(r.job, restoreRecovery{Status: "staging", Roots: entries}, ""); err != nil {
		return nil, err
	}
	if err := r.stage(); err != nil {
		return nil, err
	}
	if err := r.unpack(); err != nil {
		return nil, err
	}
	if err := saveStatus(r.job, "replacing"); err != nil {
		return nil, err
	}
	for index, root := range r.roots {
		if err := r.replace(index, root); err != nil {
			return nil, err
		}
	}
	if err := r.stillSafe(); err != nil {
		return nil, err
	}
	if err := saveStatus(r.job, "committed"); err != nil {
		return nil, err
	}
	r.restored = r.result()
	if err := r.j.cleanupRestore(r.ctx, r.server, r.job, r.data); err != nil {
		return nil, err
	}
	return r.restored, nil
}

// saveStatus saves the journal's current roots under a new status.
func saveStatus(job *ops.Context, status string) error {
	current, _ := journals(job)
	return saveRestore(job, restoreRecovery{Status: status, Roots: current}, "")
}

func (r *restoreRun) assertAccess() error { return r.j.dataAuthority(r.ctx, r.server, r.job) }

// stillSafe confirms the server is still the one this restore stopped and
// that its requester may still restore it.
func (r *restoreRun) stillSafe() error {
	if err := r.j.assertStopped(r.ctx, r.server, r.job); err != nil {
		return err
	}
	return r.assertAccess()
}

func (r *restoreRun) warn() error {
	if err := r.j.warnFirst(r.ctx, r.job, authz.BackupsRestore, true); err != nil || !warnRequested(r.job) {
		return err
	}
	// Past the wait, an interruption is recovered like any other.
	return r.job.Progress("validating", nil, nil)
}

// validate checks the archive and that it fits, before the server stops.
func (r *restoreRun) validate() error {
	if r.resumed {
		if err := r.warn(); err != nil {
			return err
		}
	}
	if err := r.j.assertCapability(r.ctx, r.job, authz.BackupsRestore); err != nil {
		return err
	}
	// A restore reads the second copy when the archive is missing from its
	// own folder. A folder that stops responding fails the restore, which
	// releases its locks, before the server stops.
	folder, err := r.j.Backups.ArchiveFolder(r.row)
	if err != nil {
		return err
	}
	rootID := ""
	if r.item != "" {
		rootID = r.roots[0].ID
	}
	staged, err := r.j.Backups.Validate(r.row, folder, rootID, r.item)
	if err != nil {
		return err
	}
	r.folder, r.staged = folder, staged
	if r.whole || r.job.Job.Input["createBackup"] == true {
		if err := r.j.Backups.AssertReady(r.ctx, r.server); err != nil {
			return err
		}
	}
	return r.j.assertStagingRoom(r.ctx, r.server, r.job, r.roots, r.item, r.staged, r.reserve, r.assertAccess)
}

// stop stops the server and takes the safety backup. One item's replaced
// copy is kept in the stage until the swap commits; a whole restore, or one
// asked to, also keeps a safety backup.
func (r *restoreRun) stop() error {
	if err := r.j.stopForData(r.ctx, r.server, r.job); err != nil {
		return err
	}
	if r.whole || r.job.Job.Input["createBackup"] == true {
		var err error
		r.safety, err = r.j.createStoppedBackup(r.ctx, r.server, r.job)
		return err
	}
	return nil
}

// stage starts the data helper and makes each root's stage folder, which
// the staging journal already covers.
func (r *restoreRun) stage() error {
	if err := r.assertAccess(); err != nil {
		return err
	}
	var err error
	if r.data, err = r.j.Backups.Helpers.NewDataHelper(r.ctx, r.server, false, r.job.Job.ID); err != nil {
		return err
	}
	helperID := r.data.Container
	// assertStopped counts any helper but the recorded one as another writer.
	if err := r.job.Progress("restore_staging", func(recovery *ops.Recovery) { recovery.DataHelperID = helperID }, nil); err != nil {
		return err
	}
	if err := r.j.assertStopped(r.ctx, r.server, r.job); err != nil {
		return err
	}
	// Extraction also stops at the reserve as it writes.
	if err := r.j.stagingRoom(r.ctx, r.data, r.server, r.job, r.roots, r.staged, r.reserve, r.assertAccess); err != nil {
		return err
	}
	for _, root := range r.roots {
		destination, err := backups.HelperRoot(r.server, root)
		if err != nil {
			return err
		}
		r.targets = append(r.targets, backups.StageTarget{Root: root, Path: destination, Item: r.item})
	}
	r.stageMade = true
	for _, target := range r.targets {
		if err := r.assertAccess(); err != nil {
			return err
		}
		if _, err := r.j.restoreStep(r.ctx, r.data, target.Path, "", r.job, "stage", r.assertAccess); err != nil {
			return err
		}
	}
	return nil
}

// unpack extracts the archive into the stage folders. A stream cut while
// Docker restarted, or by the connection alone, only ever wrote into stage
// folders, which nothing live uses yet: once every helper that may still
// write is gone, they are made again and the archive unpacked again, at most
// three times in all. The replace that follows is never repeated.
func (r *restoreRun) unpack() error {
	var since time.Time
	for attempt := 1; ; attempt++ {
		if err := r.stillSafe(); err != nil {
			return err
		}
		err := r.j.Backups.Helpers.ExtractToStage(r.ctx, r.folder, r.row.ID, r.data.Container, r.targets, stageName(r.job),
			r.row.Roots, r.row.Size, r.reserve, r.row.Checksum, r.assertAccess)
		if err == nil {
			return r.assertAccess()
		}
		// A 5xx answer means Docker is there but failing, which unpacking
		// again won't fix.
		cut := errors.Is(err, backups.ErrStreamCut) ||
			(docker.Unavailable(err) && docker.StatusCode(err) < 500 && r.j.Docker.WaitForDocker(r.ctx, err, &since))
		if attempt == 3 || !cut {
			return err
		}
		if err := r.restage(); err != nil {
			return err
		}
	}
}

// restage removes this restore's helpers, then empties and makes again
// every stage folder through a new one.
func (r *restoreRun) restage() error {
	r.data.Cleanup()
	r.data = nil
	if err := r.j.removeOperationHelpers(r.ctx, r.job); err != nil {
		return err
	}
	data, err := r.j.Backups.Helpers.NewDataHelper(r.ctx, r.server, false, r.job.Job.ID)
	if err != nil {
		return err
	}
	r.data = data
	if err := r.job.Progress("restore_staging", func(recovery *ops.Recovery) { recovery.DataHelperID = data.Container }, nil); err != nil {
		return err
	}
	if err := r.j.assertStopped(r.ctx, r.server, r.job); err != nil {
		return err
	}
	for _, target := range r.targets {
		for _, operation := range []string{"cleanup", "stage"} {
			if _, err := r.j.restoreStep(r.ctx, r.data, target.Path, "", r.job, operation, r.assertAccess); err != nil {
				return err
			}
		}
	}
	return nil
}

// replace swaps one root's data for its staged copy, journaling each move
// before and after it runs.
func (r *restoreRun) replace(index int, root backups.Root) error {
	if err := r.stillSafe(); err != nil {
		return err
	}
	destination, err := backups.HelperRoot(r.server, root)
	if err != nil {
		return err
	}
	if err := persistJournal(r.job, index, "moving_old"); err != nil {
		return err
	}
	if _, err := r.j.restoreStep(r.ctx, r.data, destination, r.item, r.job, "moveOld", r.assertAccess); err != nil {
		return err
	}
	if err := persistJournal(r.job, index, "old_moved"); err != nil {
		return err
	}
	// The folder is empty now: a server started elsewhere meanwhile must not
	// have the backup's data moved in under it.
	if err := r.stillSafe(); err != nil {
		return err
	}
	if _, err := r.j.restoreStep(r.ctx, r.data, destination, r.item, r.job, "moveNew", r.assertAccess); err != nil {
		return err
	}
	return persistJournal(r.job, index, "replaced")
}

func (r *restoreRun) result() map[string]any {
	restored := withSave(r.job, map[string]any{"restoredBackupId": r.row.ID})
	if r.moved {
		restored[MovedInput] = true
	}
	if r.safety != nil {
		restored["safetyBackupId"] = r.safety.ID
	}
	if !r.whole {
		restored["restoredRoot"], restored["restoredItem"] = r.roots[0].ID, r.item
	}
	return restored
}

// failed finishes a committed restore whose cleanup failed, or rolls back
// one that had not committed. Activity says what state the data was left in.
func (r *restoreRun) failed(err error) (map[string]any, error) {
	ctx, job := context.WithoutCancel(r.ctx), r.job
	state, _ := restoreState(job)
	// The restored data is in place; only its staging copy of the old data
	// remains, which the next stop for a backup, restore, or update removes.
	if r.restored != nil && state != nil && state.Status == "committed" {
		if cleanErr := r.j.cleanupAfterFailure(ctx, r.server, job, &r.data); cleanErr == nil {
			return r.restored, nil
		}
		r.j.notify(notify.Results, "restore-result:"+job.Job.ID, r.server.Container.DisplayName+": "+r.what+
			" completed, but its staging folder could not be removed yet. Its Activity says more.")
		return nil, fail("RESTORE_STAGING_LEFT", "The backup was restored, but Ludock could not remove the folder "+stageName(job)+
			" from the server's data, which holds the replaced data. Ludock removes it the next time it stops the server for a backup, a restore or an update.")
	}
	touched := false
	if state != nil {
		touched = slices.ContainsFunc(state.Roots, holdsLive)
	}
	if (r.data != nil || r.stageMade) && (state == nil || (state.Status != "committed" && state.Status != "cleaned")) {
		if rollbackErr := r.j.rollbackAfterFailure(ctx, r.server, job, &r.data); rollbackErr != nil {
			err = errors.Join(err, rollbackErr)
		}
	} else if r.data == nil && state != nil && state.Status == "staging" {
		// Without a helper no stage folder was made.
		saveRestore(job, restoreRecovery{Status: "rolled_back"}, "")
	}
	if guidance := endedRestoreGuidance(job, touched); guidance != "" {
		result := maps.Clone(job.Job.Result)
		if result == nil {
			result = map[string]any{}
		}
		result["guidance"] = guidance
		job.Progress(job.Job.Phase, nil, result)
	}
	if r.ctx.Err() == nil {
		r.j.notify(notify.Results, "restore-result:"+job.Job.ID, r.server.Container.DisplayName+": "+r.what+" failed. Its Activity says why.")
	}
	return nil, publicFirst(err)
}

const (
	restoreNothing = "Nothing was restored: the server’s data is as it was."
	restoreUndone  = "The restore was undone: the server’s data is as it was."
	// pendingLater is when Ludock finishes a restore that ended partway.
	pendingLater = "the next time it stops the server for a backup, a restore or an update"
)

// endedRestoreGuidance says what a restore that ended without finishing left
// the data as; touched says live data had moved before it was undone.
func endedRestoreGuidance(job *ops.Context, touched bool) string {
	state, err := restoreState(job)
	if err != nil || state == nil {
		return ""
	}
	switch {
	case state.Status == "rolled_back" && touched:
		return restoreUndone
	case state.Status == "rolled_back" || state.Status == "cleaned":
		return restoreNothing
	case state.Status == "committed":
		return ""
	case slices.ContainsFunc(state.Roots, holdsLive):
		return "The restore was stopped partway. The server’s previous data is safe in the folder " + stageName(job) +
			" in its data, but not in place. An administrator can undo the restore from Needs attention; otherwise Ludock undoes it " + pendingLater + "."
	}
	return "Nothing was restored, but the backup’s unpacked copy is still in the folder " + stageName(job) + ". Ludock removes it " + pendingLater + "."
}

// rollbackAfterFailure rolls back through a fresh helper when the command
// that failed stopped the old one, as recovery does.
func (j *Jobs) rollbackAfterFailure(ctx context.Context, server *servers.Context, job *ops.Context, data **backups.DataHelper) error {
	running := false
	if *data != nil {
		running, _ = j.Backups.Helpers.IsRunning(ctx, (*data).Container)
	}
	if !running {
		if *data != nil {
			(*data).Cleanup()
		}
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
	return j.rollbackRestore(ctx, server, job, *data, nil)
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

func (j *Jobs) recoverRestore(ctx context.Context, job *ops.Context) (map[string]any, error) {
	// What recovery leaves the data as, for Activity: a committed restore is
	// finished, one that began replacing is undone.
	outcome := restoreNothing
	finished := false
	if state, _ := restoreState(job); state != nil {
		switch state.Status {
		case "committed", "cleaned":
			outcome, finished = "The restore had finished: what it restored is in place.", true
		case "replacing", "rolling_back", "rolled_back":
			outcome = restoreUndone
		}
	}
	if !finished {
		defer j.notifyInterrupted(ctx, job, notify.Results, "restore-result:"+job.Job.ID,
			"Ludock restarted during a restore. Its Activity says what the data was left as.")
	}
	touched := false
	if state, _ := restoreState(job); state != nil {
		touched = slices.ContainsFunc(state.Roots, holdsLive)
	}
	err := j.withRecoveryLocks(ctx, job, func(server *servers.Context) error {
		entries, err := journals(job)
		if err != nil {
			return err
		}
		// Recovery uses exactly the roots of the revalidated binding.
		if len(entries) > 0 && !journalMatches(entries, server) {
			return fail("INVALID_RESTORE_JOURNAL", "Restore recovery roots do not match this server. Keep it stopped and review its recovery state.")
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
				err = j.rollbackRestore(ctx, server, job, data, nil)
			}
			data.Cleanup()
			if err != nil {
				return err
			}
		}
		return j.restoreInitialState(ctx, server, job)
	})
	result := maps.Clone(job.Job.Result)
	if result == nil {
		result = map[string]any{}
	}
	if err != nil {
		// The outcome keeps this guidance: a recovery that can't finish, such
		// as one blocked by a server started elsewhere, is finished later.
		if guidance := endedRestoreGuidance(job, touched); guidance != "" {
			result["guidance"] = guidance
			job.Progress(job.Job.Phase, nil, result)
		}
		return nil, err
	}
	result["guidance"] = outcome
	return result, nil
}
