package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
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
	// StageMissing: the undo found a stage folder gone, so that root's live
	// data was left as it was, which may not be the server's previous data.
	StageMissing bool `json:"stageMissing,omitempty"`
	// PutBack, on a rolled_back journal: live data had moved and the undo
	// put all of the server's previous data back. Without it and
	// StageMissing, live data never moved.
	PutBack bool `json:"putBack,omitempty"`
	// Kept: returning the previous data found the set-aside folder in a
	// root, as when a server started meanwhile had made a newer copy of
	// something it returned.
	Kept bool `json:"kept,omitempty"`
	// keptUnknown, never saved: a set-aside folder couldn't be read, so
	// Activity says where to look instead of what is there.
	keptUnknown bool
}

var errJournal = fail("INVALID_RESTORE_JOURNAL", "Restore recovery records are invalid. Keep the server stopped and review its recovery state.")

func restoreState(job *ops.Context) (*restoreRecovery, error) {
	if len(job.Job.Recovery.Restore) == 0 && !job.Job.Recovery.DataUnsafe {
		return nil, nil
	}
	var state restoreRecovery
	decoder := json.NewDecoder(bytes.NewReader(job.Job.Recovery.Restore))
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
	// unknown: a restored copy of Ludock's data dropped a journal that
	// wasn't final (db.settleOperations).
	case "cleaned", "rolled_back", "unknown":
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
	change, err := journalChange(state)
	if err != nil {
		return err
	}
	return job.Progress(phase, change, nil)
}

func journalChange(state restoreRecovery) (func(*ops.Recovery), error) {
	encoded, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	return func(r *ops.Recovery) {
		r.Restore, r.DataUnsafe = encoded, state.Status == "replacing" || state.Status == "rolling_back"
	}, nil
}

// restoreFinish is what Activity says once a restore's journal is final:
// note, for the restore, and for by, the operation finishing it, if any.
type restoreFinish struct {
	note func(final restoreRecovery) string
	by   *ops.Context
}

// saveFinal saves a restore's final journal, with finish's notes in the same
// transaction, so an interruption can't leave the data's state without
// them.
func (j *Jobs) saveFinal(ctx context.Context, job *ops.Context, final restoreRecovery, finish *restoreFinish) error {
	if finish == nil {
		return saveRestore(job, final, "")
	}
	change, err := journalChange(final)
	if err != nil {
		return err
	}
	note := finish.note(final)
	result := job.Job.Result
	if note != "" {
		result.Guidance = note
	}
	var saved []func()
	err = j.DB.Tx(ctx, func(tx *sql.Tx) error {
		if finish.by != nil {
			undone := strings.TrimSpace(finish.by.Job.Recovery.Undone + " " + note)
			byResult := finish.by.Job.Result
			byResult.UndoNote = undone
			done, err := finish.by.ProgressIn(tx, finish.by.Job.Phase, func(r *ops.Recovery) { r.Undone = undone }, &byResult)
			if err != nil {
				return err
			}
			saved = append(saved, done)
		}
		done, err := job.ProgressIn(tx, "restore_"+final.Status, change, &result)
		saved = append(saved, done)
		return err
	})
	if err != nil {
		return err
	}
	for _, done := range saved {
		done()
	}
	return nil
}

// persistRoot saves a root's phase, and whether copies were kept, together.
func persistRoot(job *ops.Context, rootID, phase string, kept bool) error {
	state, err := restoreState(job)
	if err != nil || state == nil || len(state.Roots) == 0 {
		return errJournal
	}
	index := indexOf(state.Roots, rootID)
	if index < 0 {
		return errJournal
	}
	state.Roots[index].Phase, state.Kept = phase, state.Kept || kept
	return saveRestore(job, *state, "restore_"+phase)
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
	if operation == helper.RestoreCleanup || operation == helper.RestoreRollbackClean || operation == helper.RestoreSetAside {
		limit = removalLimit
	}
	request := helper.RestoreRequest{Root: root, Stage: stageName(job), Operation: operation, Item: item, Base: backups.HelperBaseOf(root)}
	if operation == helper.RestoreSetAside || operation == helper.RestoreRollbackOld {
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
	// what is on the item's path. Another restore's stage below the top would
	// move with what holds it; the helper refuses it too, but only once the
	// server is stopped.
	var stages []backups.StageFolder
	if item == "" || job.Job.Input["createBackup"] == true {
		stages, err = j.Backups.CheckOwnMounts(ctx, server, data, "", assertAccess)
	}
	if err == nil && item != "" && job.Job.Input["createBackup"] == true {
		// The safety backup leaves out a stage beside the item.
		holder := ""
		if len(stages) > 0 {
			holder = stages[0].Holder()
		}
		err = job.Progress(job.Job.Phase, func(r *ops.Recovery) { r.LeftOut = holder }, nil)
	}
	if err == nil && item != "" {
		// A mount on the item's path is one in the data, but none there says
		// nothing about the rest.
		if stages, err = j.Backups.Helpers.AssertOwnMounts(ctx, server, &backups.DataHelper{Container: data.Container, Roots: roots}, item, assertAccess); err != nil {
			j.Backups.NoteHostMount(server, err)
		}
	}
	if err != nil {
		return err
	}
	if len(stages) > 0 {
		return backups.StageInside(stages[0].Holder())
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
	checked, err := j.restoreStep(ctx, data, destination, item, job, helper.RestoreCheck, assertAccess)
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
		space, err := j.restoreStep(ctx, data, destination, "", job, helper.RestoreSpace, assertAccess)
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
func (j *Jobs) rollbackRestore(ctx context.Context, server *servers.Context, job *ops.Context, data *backups.DataHelper, beforeLive func() error,
	finish *restoreFinish) error {
	state, err := restoreState(job)
	if err != nil {
		return err
	}
	var entries []journal
	setAside, missing, kept, unknown := false, false, false, false
	if state != nil {
		entries, setAside, missing, kept = state.Roots, state.SetAside, state.StageMissing, state.Kept
	}
	// A root past staging had live data moved, even one a restart midway
	// through this undo already put back.
	touched := slices.ContainsFunc(entries, func(entry journal) bool { return entry.Phase != "staging" })
	if err := saveRestore(job, restoreRecovery{Status: "rolling_back", Roots: entries, SetAside: setAside, StageMissing: missing, Kept: kept}, ""); err != nil {
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
			clean := helper.RestoreRollbackClean
			if setAside {
				clean = helper.RestoreSetAside
			}
			cleaned, err := j.restoreStep(ctx, data, rootPath, entry.Item, job, clean, live)
			if err != nil {
				return err
			}
			if clean == helper.RestoreSetAside {
				kept = kept || cleaned["setAside"] == true
			}
		}
		if holdsLive(entry) {
			// Persist after removing new data and before returning any old
			// entry: a restart midway must never delete returned originals.
			// Copies just set aside are journaled with it.
			if err := persistRoot(job, entry.Root.ID, "rolling_back", kept); err != nil {
				return err
			}
			// A server started while new data was removed must not have old
			// data returned under it.
			if blocked = j.assertStopped(ctx, server, job); blocked != nil {
				continue
			}
			returned, err := j.restoreStep(ctx, data, rootPath, entry.Item, job, helper.RestoreRollbackOld, live)
			if err != nil {
				return err
			}
			kept = kept || returned["setAside"] == true
			if err := persistRoot(job, entry.Root.ID, "rolled_back", kept); err != nil {
				return err
			}
		}
		if _, err := j.restoreStep(ctx, data, rootPath, "", job, helper.RestoreCleanup, nil); err != nil {
			return err
		}
	}
	if blocked != nil {
		return blocked
	}
	// What an undo kept is read from the folders as they are now, also
	// those of roots an earlier attempt finished: their owner may have moved
	// them away since, and an attempt whose result was lost may have kept
	// one. Copies an earlier attempt moved are made durable now.
	if touched {
		kept, unknown = j.keptOnDisk(ctx, server, data, job, &restoreRecovery{Roots: entries}, true)
	}
	return j.saveFinal(ctx, job, restoreRecovery{Status: "rolled_back", StageMissing: missing, PutBack: touched && !missing, Kept: kept, keptUnknown: unknown}, finish)
}

func (j *Jobs) cleanupRestore(ctx context.Context, server *servers.Context, job *ops.Context, data *backups.DataHelper, finish *restoreFinish) error {
	entries, err := journals(job)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		rootPath, err := backups.HelperRoot(server, entry.Root)
		if err != nil {
			return err
		}
		if _, err := j.restoreStep(ctx, data, rootPath, "", job, helper.RestoreCleanup, nil); err != nil {
			return err
		}
	}
	return j.saveFinal(ctx, job, restoreRecovery{Status: "cleaned"}, finish)
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
	restored *ops.Result
}

func (j *Jobs) runRestore(ctx context.Context, job *ops.Context) (*ops.Result, error) {
	r, err := j.planRestore(ctx, job)
	if err != nil {
		return nil, err
	}
	// The archive is validated once its locks are held.
	return job.WithLocks(append(slices.Clone(r.server.LockKeys), locks.StorageKey), func() (result *ops.Result, err error) {
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
func (r *restoreRun) run() (*ops.Result, error) {
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
	if err := r.j.cleanupRestore(r.ctx, r.server, r.job, r.data, nil); err != nil {
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
		if err := r.j.Backups.AssertReady(r.ctx, r.server, r.row.ID); err != nil {
			return err
		}
	} else if err := r.j.Backups.AssertNoOtherWriters(r.ctx, r.server, ""); err != nil {
		return err
	}
	return r.j.assertStagingRoom(r.ctx, r.server, r.job, r.roots, r.item, r.staged, r.reserve, r.assertAccess)
}

// stop stops the server and takes the safety backup. One item's replaced
// copy is kept in the stage until the swap commits; a whole restore, or one
// asked to, also keeps a safety backup.
func (r *restoreRun) stop() error {
	backup := r.whole || r.job.Job.Input["createBackup"] == true
	stop := r.j.stopForData
	if backup {
		stop = r.j.stopForBackup
	}
	if err := stop(r.ctx, r.server, r.job); err != nil {
		return err
	}
	if backup {
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
		if _, err := r.j.restoreStep(r.ctx, r.data, target.Path, "", r.job, helper.RestoreStage, r.assertAccess); err != nil {
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
		aclsLost, err := r.j.Backups.Helpers.ExtractToStage(r.ctx, r.folder, r.row.ID, r.data.Container, r.targets, stageName(r.job),
			r.row.Roots, r.row.Size, r.reserve, r.row.Checksum, r.assertAccess)
		if err == nil && aclsLost {
			err = r.job.Progress(r.job.Job.Phase, func(recovery *ops.Recovery) { recovery.ACLsLost = true }, nil)
		}
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
		for _, operation := range []string{helper.RestoreCleanup, helper.RestoreStage} {
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
	if _, err := r.j.restoreStep(r.ctx, r.data, destination, r.item, r.job, helper.RestoreMoveOld, r.assertAccess); err != nil {
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
	if _, err := r.j.restoreStep(r.ctx, r.data, destination, r.item, r.job, helper.RestoreMoveNew, r.assertAccess); err != nil {
		return err
	}
	return persistJournal(r.job, index, "replaced")
}

func (r *restoreRun) result() *ops.Result {
	restored := withSave(r.job, &ops.Result{RestoredBackupID: r.row.ID, DataMoved: r.moved, ACLsLost: r.job.Job.Recovery.ACLsLost})
	if r.safety != nil {
		restored.SafetyBackupID = r.safety.ID
	}
	if !r.whole {
		restored.RestoredRoot, restored.RestoredItem = r.roots[0].ID, r.item
	}
	return restored
}

// failed finishes a committed restore whose cleanup failed, or rolls back
// one that had not committed. Activity says what state the data was left in.
func (r *restoreRun) failed(err error) (*ops.Result, error) {
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
	touched, seen := false, keptSeen{}
	if state != nil {
		touched, seen.kept = slices.ContainsFunc(state.Roots, holdsLive), state.Kept
	}
	if (r.data != nil || r.stageMade) && (state == nil || (state.Status != "committed" && state.Status != "cleaned")) {
		// Activity is saved with the final journal, so a stop right after
		// still says where to look.
		heard := &restoreFinish{note: func(final restoreRecovery) string {
			seen = keptSeen{kept: final.Kept, unknown: final.keptUnknown}
			return endedGuidance(job, &final, touched, seen)
		}}
		if rollbackErr := r.j.rollbackAfterFailure(ctx, r.server, job, &r.data, heard); rollbackErr != nil {
			err = errors.Join(err, rollbackErr)
			// What the failed rollback kept shows once its helper is gone.
			if r.data != nil {
				r.data.Cleanup()
				r.data = nil
			}
			if touchedLive(state) {
				seen = r.j.reconcileKept(ctx, r.server, job)
			}
		}
	} else if r.data == nil && state != nil && state.Status == "staging" {
		// Without a helper no stage folder was made.
		saveRestore(job, restoreRecovery{Status: "rolled_back"}, "")
	}
	if guidance := endedRestoreGuidance(job, touched, seen); guidance != "" {
		result := job.Job.Result
		result.Guidance = guidance
		job.Progress(job.Job.Phase, nil, &result)
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
// the data as; touched says live data had moved before it was undone. Kept
// alone decides whether the set-aside folder is named.
func endedRestoreGuidance(job *ops.Context, touched bool, seen keptSeen) string {
	state, err := restoreState(job)
	if err != nil || state == nil {
		return ""
	}
	return endedGuidance(job, state, touched, seen)
}

// keptSeen is what the latest read of the set-aside folders found.
type keptSeen struct{ kept, unknown bool }

// endedGuidance is endedRestoreGuidance for the journal state, as about to
// be saved, and what the folders were last seen to hold.
func endedGuidance(job *ops.Context, state *restoreRecovery, touched bool, seen keptSeen) string {
	// A root an earlier attempt finished was put back, unless a stage was
	// found gone.
	returned := touched || state.PutBack || (!state.StageMissing && slices.ContainsFunc(state.Roots, func(entry journal) bool { return entry.Phase == "rolled_back" }))
	var guidance string
	switch {
	case state.Status == "rolled_back" && returned:
		guidance = restoreUndone
	case state.Status == "rolled_back" || state.Status == "cleaned":
		guidance = restoreNothing
	case state.Status == "committed":
		return ""
	// A return that was under way may have finished without Ludock hearing.
	case slices.ContainsFunc(state.Roots, func(entry journal) bool { return entry.Phase == "rolling_back" }):
		guidance = "The restore was stopped partway while its previous data was being put back, so Ludock can’t confirm where all of it is: look in the server’s data and in the folder " +
			stageName(job) + ". An administrator can undo the restore from Needs attention; otherwise Ludock undoes it " + pendingLater + "."
	case slices.ContainsFunc(state.Roots, holdsLive):
		guidance = "The restore was stopped partway. The server’s previous data is safe in the folder " + stageName(job) +
			" in its data, but not in place. An administrator can undo the restore from Needs attention; otherwise Ludock undoes it " + pendingLater + "."
	case returned:
		guidance = restoreUndone + " Ludock finishes removing its folder " + stageName(job) + " " + pendingLater + "."
	default:
		guidance = "Nothing was restored, but the backup’s unpacked copy is still in the folder " + stageName(job) + ". Ludock removes it " + pendingLater + "."
	}
	if seen.kept {
		guidance += " " + keptNote(job)
	} else if seen.unknown {
		guidance += " " + unknownKeptNote(job)
	}
	return guidance
}

// keptOutcome adds to a recovery's outcome where anything moved aside went.
func keptOutcome(job *ops.Context, outcome string, seen keptSeen) string {
	switch {
	case seen.kept:
		return outcome + " " + keptNote(job)
	case seen.unknown:
		return outcome + " " + unknownKeptNote(job)
	}
	return outcome
}

// reconcileKept journals whether a failed undo kept copies, which only the
// set-aside folders show once the helper whose step failed is removed, and
// says whether that stayed unknown. It reads again even after an earlier
// attempt found copies, which their owner may have moved since, and runs
// even if the work was cancelled.
func (j *Jobs) reconcileKept(ctx context.Context, server *servers.Context, job *ops.Context) keptSeen {
	state, err := restoreState(job)
	if err != nil || state == nil || len(state.Roots) == 0 {
		return keptSeen{}
	}
	unknown := false
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	kept := false
	data, err := j.Backups.Helpers.NewDataHelper(ctx, server, true, job.Job.ID)
	if err != nil {
		unknown = true
	} else {
		kept, unknown = j.keptOnDisk(ctx, server, data, job, state, false)
		data.Cleanup()
	}
	// A read whose result can't be saved isn't one Activity can stand on.
	if kept != state.Kept {
		state.Kept = kept
		if saveRestore(job, *state, "") != nil {
			unknown = true
		}
	}
	return keptSeen{kept: kept, unknown: unknown}
}

// keptOnDisk reports whether any root's set-aside folder is known to hold a
// copy, and whether any couldn't be read; a root that can't be checked
// doesn't stop the others.
func (j *Jobs) keptOnDisk(ctx context.Context, server *servers.Context, data *backups.DataHelper, job *ops.Context, state *restoreRecovery,
	sync bool) (kept, unknown bool) {
	for _, entry := range state.Roots {
		rootPath, err := backups.HelperRoot(server, entry.Root)
		var out []byte
		if err == nil {
			request := helper.RestoreRequest{Root: rootPath, Stage: stageName(job), Operation: helper.RestoreKept, Base: backups.HelperBaseOf(rootPath),
				Aside: helper.SetAsideName(job.Job.ID), Sync: sync}
			out, err = j.Backups.Helpers.ExecWithin(ctx, data.Container, helper.Command("restore", request), nil, 2*time.Minute)
		}
		var found struct{ Kept *bool }
		if err != nil || json.Unmarshal(out, &found) != nil || found.Kept == nil {
			unknown = true
			continue
		}
		kept = kept || *found.Kept
	}
	return kept, unknown
}

// unknownKeptNote says where newer copies may be when that couldn't be read.
func unknownKeptNote(restore *ops.Context) string {
	return "Ludock couldn’t check the folder " + helper.SetAsideName(restore.Job.ID) + " in the server’s data, where it moves anything in the way of the previous data."
}

// rollbackAfterFailure rolls back through a fresh helper when the command
// that failed stopped the old one, as recovery does.
func (j *Jobs) rollbackAfterFailure(ctx context.Context, server *servers.Context, job *ops.Context, data **backups.DataHelper, finish *restoreFinish) error {
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
	return j.rollbackRestore(ctx, server, job, *data, nil, finish)
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
	return j.cleanupRestore(ctx, server, job, fresh, nil)
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

func (j *Jobs) recoverRestore(ctx context.Context, job *ops.Context) (*ops.Result, error) {
	// What recovery leaves the data as, for Activity: a committed restore is
	// finished, one that began replacing is undone.
	outcome := restoreNothing
	finished, touched := false, false
	if state, _ := restoreState(job); state != nil {
		switch state.Status {
		case "committed", "cleaned":
			outcome, finished = "The restore had finished: what it restored is in place.", true
		case "replacing", "rolling_back", "rolled_back":
			outcome = restoreUndone
		}
		touched = slices.ContainsFunc(state.Roots, holdsLive)
	}
	if !finished {
		defer j.notifyInterrupted(ctx, job, notify.Results, "restore-result:"+job.Job.ID,
			"Ludock restarted during a restore. Its Activity says what the data was left as.")
	}
	seen, read := keptSeen{}, false
	if state, _ := restoreState(job); state != nil {
		seen.kept = state.Kept
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
		if len(entries) > 0 {
			err := func() error {
				if err := j.removeUnpublished(ctx, job); err != nil {
					return err
				}
				if err := j.assertStopped(ctx, server, job); err != nil {
					return err
				}
				data, err := j.Backups.Helpers.NewDataHelper(ctx, server, false, job.Job.ID)
				if err != nil {
					return err
				}
				defer data.Cleanup()
				if err := job.Progress("recovering", func(r *ops.Recovery) { r.DataHelperID = data.Container }, nil); err != nil {
					return err
				}
				if state, _ := restoreState(job); state != nil && state.Status == "committed" {
					return j.cleanupRestore(ctx, server, job, data, nil)
				}
				heard := &restoreFinish{note: func(final restoreRecovery) string {
					seen, read = keptSeen{kept: final.Kept, unknown: final.keptUnknown}, true
					return keptOutcome(job, outcome, seen)
				}}
				return j.rollbackRestore(ctx, server, job, data, nil, heard)
			}()
			// Any recovery that can't finish, such as one of a server already
			// running again, says what the folders show now.
			if state, _ := restoreState(job); err != nil && touchedLive(state) {
				seen, read = j.reconcileKept(ctx, server, job), true
			}
			if err != nil {
				return err
			}
		} else if err := j.removeUnpublished(ctx, job); err != nil {
			return err
		}
		return j.restoreInitialState(ctx, server, job)
	})
	result := job.Job.Result
	// A recovery cut after its final journal said there what couldn't be
	// read; that stays unless this run read the folders again.
	if !read && strings.Contains(result.Guidance, unknownKeptNote(job)) {
		seen.unknown = true
	}
	if err == nil {
		outcome = keptOutcome(job, outcome, seen)
	}
	if err != nil {
		// The outcome keeps this guidance: a recovery that can't finish, such
		// as one blocked by a server started elsewhere, is finished later.
		if guidance := endedRestoreGuidance(job, touched, seen); guidance != "" {
			result.Guidance = guidance
			job.Progress(job.Job.Phase, nil, &result)
		}
		return nil, err
	}
	result.Guidance = outcome
	result.ACLsLost = finished && job.Job.Recovery.ACLsLost
	return withServerAfterRecovery(job, &result), nil
}
