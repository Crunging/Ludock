package jobs

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
)

// PendingRestore selects restores that ended without a final journal, such as
// one stopped by a server started elsewhere. A later operation that stops
// the server, or startup, finishes them.
const PendingRestore = `kind='restore' AND status IN ('failed','interrupted') AND json_valid(recovery_json) AND
	json_extract(recovery_json,'$.restore.status') IN ('staging','replacing','rolling_back','committed')`

func (j *Jobs) pendingRestores(ctx context.Context, serverID string) ([]*ops.Context, error) {
	rows, err := j.DB.QueryContext(ctx, "SELECT id FROM operations WHERE server_id=? AND "+PendingRestore+" ORDER BY created_at DESC, id DESC", serverID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var pending []*ops.Context
	for _, id := range ids {
		job, err := j.runner.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		pending = append(pending, j.runner.Ended(job))
	}
	return pending, nil
}

// journalMatches says a journal's roots are exactly the server's current
// ones, as recovery requires.
func journalMatches(entries []journal, server *servers.Context) bool {
	if len(entries) != len(server.Container.FileRoots) && (len(entries) == 0 || entries[0].Item == "") {
		return false
	}
	for _, entry := range entries {
		if !slices.ContainsFunc(server.Container.FileRoots, func(root files.Root) bool { return root.ID == entry.Root.ID && root.Path == entry.Root.Path }) {
			return false
		}
	}
	return true
}

var errPendingBlocked = fail("RESTORE_UNDO_BLOCKED", "An earlier restore of this server stopped partway, and Ludock can’t finish undoing it safely, so it went no further. That restore’s Activity says where the server’s previous data is.")

// finishPendingRestores finishes, as startup recovery would, every restore
// of the server that ended partway, newest first, once the server is
// stopped under the caller's locks. stoppedAt is the stopped server's start
// time; job is the operation that stopped it, or nil at startup.
func (j *Jobs) finishPendingRestores(ctx context.Context, server *servers.Context, job *ops.Context, stoppedAt string) error {
	pending, err := j.pendingRestores(ctx, server.Logical.ID)
	if err != nil {
		return err
	}
	for _, restore := range pending {
		if err := j.finishPending(ctx, server, job, restore, stoppedAt); err != nil {
			return err
		}
		if job == nil {
			log.Info("Finished a restore that had stopped partway", logging.F("operation", restore.Job.ID))
		}
	}
	if job == nil || (len(pending) == 0 && !job.Job.Recovery.PendingUndo) {
		return nil
	}
	// Each restore's note joined Undone as its journal became final.
	result := job.Job.Result
	if undone := job.Job.Recovery.Undone; undone != "" {
		result.UndoNote = undone
	}
	return job.Progress(job.Job.Phase, func(r *ops.Recovery) { r.PendingUndo = false }, &result)
}

// finishPending finishes one restore that ended partway.
func (j *Jobs) finishPending(ctx context.Context, server *servers.Context, job, restore *ops.Context, stoppedAt string) error {
	// As recovery does, only the binding the restore stopped is undone.
	state, err := restoreState(restore)
	if err != nil || state == nil || !journalMatches(state.Roots, server) || restore.Job.Recovery.Fingerprint != server.Logical.BindingFingerprint {
		return errPendingBlocked
	}
	owner := restore
	if job != nil {
		owner = job
	}
	data, err := j.Backups.Helpers.NewDataHelper(ctx, server, false, owner.Job.ID)
	if err != nil {
		j.sayKept(ctx, server, job, restore, state, false)
		return err
	}
	live, missing, moved := false, false, false
	// What Activity says comes from the final journal, which also covers
	// roots an earlier attempt finished.
	note := func(final restoreRecovery) string {
		var note string
		aside := helper.SetAsideName(restore.Job.ID)
		returned, missingAny := live || final.PutBack, missing || final.StageMissing
		switch {
		case state.Status == "committed":
			note = "Ludock removed the folder " + stageName(restore) + " that a finished restore left, which held the data it replaced."
		case missingAny && !returned:
			note = "Ludock found nothing to put back for a restore that had stopped partway: the folder " + stageName(restore) +
				" that held the server’s previous data is no longer there, so its data was left as it is."
		// The folder holding something is the only evidence, and the owner
		// may have changed it since, so Activity says where anything Ludock
		// moved went, which stays true either way.
		case returned && final.Kept:
			note = "Ludock finished undoing a restore that was stopped partway: the server’s previous data is back in place. Anything Ludock moved out of its way went to the folder " +
				aside + " beside it."
		case !returned:
			note = "Ludock removed the backup’s unpacked copy that a stopped restore left in " + stageName(restore) + "."
		default:
			note = "Ludock finished undoing a restore that was stopped partway: the server’s previous data is back in place."
		}
		if missingAny && returned {
			note += " One of its data folders had nothing to put back: its folder " + stageName(restore) + " is no longer there."
		}
		// An earlier attempt may have set copies aside before this one found
		// a stage gone.
		if final.Kept && !strings.Contains(note, aside) {
			note += " " + keptNote(restore)
		} else if !final.Kept && final.keptUnknown {
			note += " " + unknownKeptNote(restore)
		}
		return note
	}
	finish := &restoreFinish{note: note, by: job}
	err = func() error {
		if job != nil {
			if err := job.Progress(job.Job.Phase, func(r *ops.Recovery) { r.DataHelperID = data.Container }, nil); err != nil {
				return err
			}
		}
		if state.Status != "committed" {
			var err error
			if live, missing, err = j.checkUndo(ctx, server, data, restore, state, nil, true); err != nil {
				return err
			}
		}
		// A server started since the restore stopped it may have written into
		// a folder the restore had emptied or filled, so that is kept.
		if restore.Job.Recovery.StoppedStartedAt != stoppedAt && slices.ContainsFunc(state.Roots, func(entry journal) bool {
			return entry.Phase == "old_moved" || entry.Phase == "replaced"
		}) && state.Status != "committed" {
			state.SetAside = true
		}
		encoded, err := json.Marshal(state)
		if err != nil {
			return err
		}
		// The restore checks the server against this operation's stop, and
		// counts its helper as no other writer.
		if err := restore.Progress("", func(r *ops.Recovery) {
			r.StoppedStartedAt, r.DataHelperID, r.Restore = stoppedAt, data.Container, encoded
		}, nil); err != nil {
			return err
		}
		if state.Status == "committed" {
			return j.cleanupRestore(ctx, server, restore, data, finish)
		}
		// Until live data moves, a failure leaves the server's data as it
		// was, so the operation that stopped it can start it again.
		return j.rollbackRestore(ctx, server, restore, data, func() error {
			moved = true
			if job == nil {
				return nil
			}
			return job.Progress(job.Job.Phase, func(r *ops.Recovery) { r.PendingUndo = true }, nil)
		}, finish)
	}()
	data.Cleanup()
	restore.Progress("", func(r *ops.Recovery) { r.DataHelperID = "" }, nil)
	if err != nil {
		j.sayKept(ctx, server, job, restore, state, moved)
	}
	if job != nil {
		if clearErr := job.Progress(job.Job.Phase, func(r *ops.Recovery) { r.DataHelperID = "" }, nil); err == nil {
			err = clearErr
		}
	}
	return err
}

// keptNote says where undoing restore moved what was in the way.
func keptNote(restore *ops.Context) string {
	return "Anything Ludock moved out of the way of the server’s previous data went to the folder " + helper.SetAsideName(restore.Job.ID) + " in its data."
}

// sayKept has a failed attempt at a restore whose live data had moved say
// what the folders show now, on the restore and on the work that tried.
func (j *Jobs) sayKept(ctx context.Context, server *servers.Context, job, restore *ops.Context, state *restoreRecovery, moved bool) {
	if !moved && !touchedLive(state) {
		return
	}
	guidance := endedRestoreGuidance(restore, true, j.reconcileKept(ctx, server, restore))
	result := restore.Job.Result
	result.Guidance = guidance
	restore.Progress("", nil, &result)
	if job != nil {
		result := job.Job.Result
		result.UndoNote = guidance
		job.Progress(job.Job.Phase, nil, &result)
	}
}

// touchedLive says a restore's live data had moved, so an undo of it may
// have set something aside.
func touchedLive(state *restoreRecovery) bool {
	return state != nil && slices.ContainsFunc(state.Roots, func(entry journal) bool { return entry.Phase != "staging" })
}

// checkUndo marks the journal's roots whose stage folder is gone as having
// nothing to put back, and refuses an undo that would move live data holding
// another restore's stage. It reports whether any root has a stage to put
// back and whether any had none.
func (j *Jobs) checkUndo(ctx context.Context, server *servers.Context, data *backups.DataHelper, restore *ops.Context, state *restoreRecovery,
	assertAccess func() error, stopped bool) (live, missing bool, err error) {
	// A root whose stage folder is gone has nothing to put back, for
	// whatever removed it, such as the restore finishing after
	// Ludock's data was saved.
	for index, entry := range state.Roots {
		if !holdsLive(entry) {
			continue
		}
		rootPath, err := backups.HelperRoot(server, entry.Root)
		if err != nil {
			return false, false, err
		}
		found, err := j.restoreStep(ctx, data, rootPath, "", restore, helper.RestoreStageExists, assertAccess)
		if err != nil {
			return false, false, err
		}
		// Only a helper that says so proves the stage is gone.
		switch found["exists"] {
		case true:
			live = true
			// Returning originals sets aside what is in their way, which is
			// refused before anything moves. Before the stop, only what the
			// game can't change is: a mount or a second name in its data may
			// go as it shuts down. A check that couldn't run refuses nothing:
			// returning the originals checks again before it moves anything.
			if entry.Phase == "moving_old" || entry.Phase == "rolling_back" {
				_, err := j.restoreStep(ctx, data, rootPath, entry.Item, restore, helper.RestoreCheckReturn, assertAccess)
				if apperr.Is(err, backups.ErrUndoEntryUnsafe.Code) || (stopped && (apperr.Is(err, backups.ErrUndoClashUnsafe.Code) ||
					apperr.Is(err, backups.ErrHostMount.Code) || apperr.Is(err, backups.StageInside("").Code))) {
					return false, false, err
				}
			}
		case false:
			state.Roots[index].Phase, missing, state.StageMissing = "rolled_back", true, true
		default:
			return false, false, errJournal
		}
	}
	// Undoing moves live data that may hold another restore's stage,
	// so that is refused before anything moves and the server can
	// start again.
	for _, entry := range state.Roots {
		if entry.Phase != "old_moved" && entry.Phase != "replaced" {
			continue
		}
		stages, err := j.Backups.Helpers.AssertOwnMounts(ctx, server, &backups.DataHelper{Container: data.Container, Roots: []backups.Root{entry.Root}}, entry.Item, assertAccess)
		if err != nil {
			return false, false, err
		}
		if len(stages) > 0 {
			return false, false, backups.StageInside(stages[0].Holder())
		}
	}
	return live, missing, nil
}

// assertPendingUndoable refuses, before players are warned or the server
// stops, work whose stop would finish a restore that ended partway that
// finishPending would refuse: its journal no longer matches the server, or
// its undo would move another restore's stage or a mount the host added.
// finishPending checks again once the server is stopped; a check that can't
// run here is left to it.
func (j *Jobs) assertPendingUndoable(ctx context.Context, server *servers.Context, job *ops.Context, assertAccess func() error) error {
	pending, err := j.pendingRestores(ctx, server.Logical.ID)
	if err != nil {
		return err
	}
	var data *backups.DataHelper
	defer func() {
		if data != nil {
			data.Cleanup()
		}
	}()
	for _, restore := range pending {
		state, err := restoreState(restore)
		if err != nil || state == nil || !journalMatches(state.Roots, server) || restore.Job.Recovery.Fingerprint != server.Logical.BindingFingerprint {
			return errPendingBlocked
		}
		if state.Status == "committed" {
			continue
		}
		if data == nil {
			if data, err = j.Backups.Helpers.NewDataHelper(ctx, server, true, job.Job.ID); err != nil {
				return nil
			}
		}
		if _, _, err := j.checkUndo(ctx, server, data, restore, state, assertAccess, false); err != nil {
			if apperr.Is(err, backups.StageInside("").Code) || apperr.Is(err, backups.ErrHostMount.Code) || apperr.Is(err, backups.ErrUndoEntryUnsafe.Code) {
				return err
			}
			return nil
		}
	}
	return nil
}

// finishStoppedRestores runs at startup: a restore that ended partway on a
// server that is now stopped is finished then. One that can't be is left
// for the next operation that stops the server.
func (j *Jobs) finishStoppedRestores(ctx context.Context) {
	rows, err := j.DB.QueryContext(ctx, "SELECT DISTINCT server_id FROM operations WHERE "+PendingRestore)
	if err != nil {
		log.Warn("Could not list restores that stopped partway")
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		log.Warn("Could not list restores that stopped partway")
		return
	}
	for _, id := range ids {
		pending, err := j.pendingRestores(ctx, id)
		if err != nil || len(pending) == 0 {
			continue
		}
		newest := pending[0]
		err = j.withRecoveryLocks(ctx, newest, func(server *servers.Context) error {
			if !slices.Contains(stoppedStates, server.Container.State) {
				return nil
			}
			info, err := j.Docker.Inspect(ctx, server.Container.ID)
			if err != nil {
				return err
			}
			return j.finishPendingRestores(ctx, server, nil, info.State.StartedAt)
		})
		if err != nil {
			log.Warn("Could not finish a restore that stopped partway; the next backup, restore or update of its server finishes it", logging.F("operation", newest.Job.ID))
		}
	}
}

func (j *Jobs) runUndoRestore(ctx context.Context, job *ops.Context) (*ops.Result, error) {
	server, err := j.resolve(ctx, job, authz.BackupsRestore, true)
	if err != nil {
		return nil, err
	}
	return job.WithLocks(append(slices.Clone(server.LockKeys), locks.StorageKey), func() (result *ops.Result, err error) {
		if err := j.assertCapability(ctx, job, authz.BackupsRestore); err != nil {
			return nil, err
		}
		pending, err := j.pendingRestores(ctx, server.Logical.ID)
		if err != nil {
			return nil, err
		}
		if len(pending) == 0 {
			return &ops.Result{Skipped: true, Guidance: "There was nothing left to undo."}, nil
		}
		defer func() {
			if restoreErr := j.restoreInitialState(context.WithoutCancel(ctx), server, job); restoreErr != nil && err == nil {
				err = restoreErr
			}
		}()
		// Stopping the server finishes the undo, which a forced stop doesn't
		// affect.
		if err := j.stopForData(ctx, server, job); err != nil && !apperr.Is(err, "UNCLEAN_SHUTDOWN") {
			return nil, err
		}
		return withSave(job, &ops.Result{}), nil
	})
}
