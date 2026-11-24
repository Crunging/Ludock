package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/ops"
)

// stoppedRestore runs a two-root restore that a server started elsewhere
// stops between moving the world's data out and the backup's in, then has
// the game write into the emptied folder. The world's previous data is left
// in the restore's stage folder.
func (f *fixture) stoppedRestore(t *testing.T) (*ops.Job, *restoreTrace) {
	t.Helper()
	input := map[string]any{"backupId": twoRootBackup(t, f), "confirmation": "mc"}
	for _, root := range []string{"world", "config"} {
		os.WriteFile(filepath.Join(f.data, root, "large.dat"), []byte("changed"), 0o644)
	}
	trace := f.traceRestore(startedAfter(helper.RestoreMoveOld, "world")(t, f))
	restore := f.run(t, "restore", input)
	if restore.Status != ops.Failed || f.savedJournal(restore.ID) != "rolling_back old_moved,staging unsafe" || !f.running() {
		t.Fatalf("%+v, journal %s", restore, f.savedJournal(restore.ID))
	}
	if guidance, _ := restore.Result["guidance"].(string); !strings.Contains(guidance, "stopped partway") || !strings.Contains(guidance, helper.StageName(restore.ID)) {
		t.Fatal(restore.Result)
	}
	os.WriteFile(filepath.Join(f.data, "world", "generated.dat"), []byte("generated"), 0o644)
	return restore, trace
}

// undoneLines is how the next operation finishes stoppedRestore's undo:
// the world's stage folder is still there; the config root held only its
// stage folder, which the failed restore already removed; the world's live
// data is set aside before its previous data returns.
var undoneLines = []string{"stageExists world: rolling_back old_moved,staging unsafe", "cleanup config: rolling_back old_moved,staging unsafe",
	"setAside world: rolling_back old_moved,staging unsafe",
	"rollbackOld world: rolling_back rolling_back,staging unsafe",
	"cleanup world: rolling_back rolled_back,staging unsafe"}

// checkUndone confirms stoppedRestore was finished: previous data in place,
// what the game wrote kept beside it, no stage folder, and both Activity
// entries saying so.
func (f *fixture) checkUndone(t *testing.T, restoreID string, later *ops.Job) {
	t.Helper()
	aside := helper.SetAsideName(restoreID)
	if read(t, filepath.Join(f.data, "world", "large.dat")) != "changed" || read(t, filepath.Join(f.data, "world", aside, "generated.dat")) != "generated" {
		t.Error("previous data not returned, or the game's writes not kept")
	}
	for _, root := range []string{"world", "config"} {
		entries, _ := os.ReadDir(filepath.Join(f.data, root))
		if slices.ContainsFunc(entries, func(entry os.DirEntry) bool { return strings.HasPrefix(entry.Name(), helper.StagePrefix) }) {
			t.Error("stage folder left in", root)
		}
	}
	restore, _ := f.runner.Get(ctx, restoreID)
	if guidance, _ := restore.Result["guidance"].(string); restore.Status != ops.Failed || f.savedJournal(restoreID) != "rolled_back" ||
		!strings.Contains(guidance, "back in place") || !strings.Contains(guidance, aside) {
		t.Errorf("%+v, journal %s", restore, f.savedJournal(restoreID))
	}
	if undone, _ := later.Result["undone"].(string); !strings.Contains(undone, "back in place") || !strings.Contains(undone, aside) {
		t.Errorf("%+v", later)
	}
}

// After a restore stopped partway, the next backup checks the undo can be
// finished, stops the server, finishes undoing the restore before its own copy, keeping what the game
// wrote meanwhile, backs up, and returns the server to running.
func TestNextBackupFinishesStoppedRestore(t *testing.T) {
	f := setup(t)
	restore, trace := f.stoppedRestore(t)
	before := len(trace.lines())
	backup := f.run(t, "backup", nil)
	if backup.Status != ops.Succeeded || !f.running() {
		t.Fatalf("%+v", backup)
	}
	// The undo is checked once before the stop and again once stopped.
	want := append([]string{undoneLines[0]}, undoneLines...)
	if got := trace.lines()[before:]; !slices.Equal(got, want) {
		t.Errorf("trace:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	f.checkUndone(t, restore.ID, backup)
	if pending, _ := f.jobs.pendingRestores(ctx, f.serverID); len(pending) != 0 {
		t.Fatal(pending)
	}
}

// A nightly restart that backs up first, after a restore stopped partway,
// finishes the undo, backs up, and leaves the server running, where before
// the backup refused and the server stayed down.
func TestRestartWithBackupFinishesStoppedRestore(t *testing.T) {
	f := setup(t)
	restore, _ := f.stoppedRestore(t)
	restart := f.run(t, "restart", map[string]any{"createBackup": true})
	if restart.Status != ops.Succeeded || !f.running() {
		t.Fatalf("%+v", restart)
	}
	f.checkUndone(t, restore.ID, restart)
}

// Ludock restarts while the server is stopped after a restore stopped
// partway: startup finishes the undo and leaves the server stopped.
func TestStartupFinishesStoppedRestoreOnStoppedServer(t *testing.T) {
	f := setup(t)
	restore, _ := f.stoppedRestore(t)
	f.runner.Stop()
	if err := f.jobs.Docker.Stop(ctx, f.game, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if f.savedJournal(restore.ID) != "rolled_back" || f.running() {
		t.Fatal(f.savedJournal(restore.ID))
	}
	if read(t, filepath.Join(f.data, "world", "large.dat")) != "changed" ||
		read(t, filepath.Join(f.data, "world", helper.SetAsideName(restore.ID), "generated.dat")) != "generated" {
		t.Fatal("previous data not returned, or the game's writes not kept")
	}
}

// A restore that can't be finished, because its roots no longer match the
// server, still blocks the backup, which refuses before copying.
func TestBackupRefusesUnfinishableRestore(t *testing.T) {
	f := setup(t)
	restore, _ := f.stoppedRestore(t)
	if _, err := f.jobs.DB.Exec(`UPDATE operations SET recovery_json=json_set(recovery_json,'$.restore.roots[0].root.path','/elsewhere') WHERE id=?`, restore.ID); err != nil {
		t.Fatal(err)
	}
	backup := f.run(t, "backup", nil)
	if backup.Status != ops.Failed || !strings.Contains(backup.Error, "can’t finish undoing it safely") {
		t.Fatalf("%+v", backup)
	}
	if f.savedJournal(restore.ID) != "rolling_back old_moved,staging unsafe" {
		t.Fatal(f.savedJournal(restore.ID))
	}
}

// Ludock's data is saved while a restore moves the backup's data in, the
// restore then finishes, and later the owner restores Ludock from that save:
// the copy's journal names a stage folder that is gone. The next backup finds
// nothing to put back, says so, backs up, and leaves the server running.
func TestNextBackupAfterStaleRestoreJournal(t *testing.T) {
	f := setup(t)
	input := map[string]any{"backupId": twoRootBackup(t, f), "confirmation": "mc"}
	var restoreID, saved string
	f.traceRestore(once(helper.RestoreMoveNew, "world", func(_ *fakedocker.Container, code int) int {
		f.jobs.DB.QueryRow("SELECT id,recovery_json FROM operations WHERE kind='restore'").Scan(&restoreID, &saved)
		return code
	}))
	if restore := f.run(t, "restore", input); restore.Status != ops.Succeeded || saved == "" || !f.running() {
		t.Fatalf("%+v", restore)
	}
	if _, err := f.jobs.DB.Exec("UPDATE operations SET status='interrupted', recovery_json=? WHERE id=?", saved, restoreID); err != nil {
		t.Fatal(err)
	}
	backup := f.run(t, "backup", nil)
	if undone, _ := backup.Result["undone"].(string); backup.Status != ops.Succeeded || !f.running() || !strings.Contains(undone, "nothing to put back") {
		t.Fatalf("%+v, running %v", backup, f.running())
	}
	if pending, _ := f.jobs.pendingRestores(ctx, f.serverID); len(pending) != 0 {
		t.Fatal(pending)
	}
}

// Finishing a restore that stopped partway would move live data holding
// another restore's stage folder: a backup, a restart with a backup first,
// an undo, and an update, each asked to warn players, are refused before
// anyone is warned or the server stops. A step that fails after the stop,
// before any live data moves, fails the backup, but the server it stopped
// is running again and the undo is left for the next operation.
func TestFailedUndoReturnsServerToRunning(t *testing.T) {
	f := setup(t)
	restore, _ := f.stoppedRestore(t)
	region := filepath.Join(f.data, "world", "region")
	other := filepath.Join(region, helper.StageName("2f9619ff-8b86-4011-b42d-00c04fc964ff"))
	os.MkdirAll(filepath.Join(other, "old"), 0o700)
	f.jobs.WarningLead = 100 * time.Millisecond
	for _, work := range []struct {
		kind  string
		input map[string]any
	}{
		{ops.KindBackup, map[string]any{WarnInput: true}},
		{ops.KindRestart, map[string]any{"createBackup": true, WarnInput: true}},
		{ops.KindUndoRestore, map[string]any{WarnInput: true}},
		{ops.KindUpdate, map[string]any{"request": skipBackup["request"], WarnInput: true}},
	} {
		before := len(f.engine.Requests)
		job := f.run(t, work.kind, work.input)
		stopped := slices.ContainsFunc(f.engine.Requests[before:], func(request string) bool { return strings.Contains(request, f.game+"/stop") })
		if job.Status != ops.Failed || job.Error != backups.StageInside(region).Message || job.Recovery.WarnUntil != 0 || stopped || !f.running() ||
			f.savedJournal(restore.ID) != "rolling_back old_moved,staging unsafe" {
			t.Errorf("%s: %s %q, warned %v, stopped %v, running %v, journal %s", work.kind, job.Status, job.Error,
				job.Recovery.WarnUntil != 0, stopped, f.running(), f.savedJournal(restore.ID))
		}
	}
	if _, err := os.Stat(filepath.Join(other, "old")); err != nil {
		t.Fatal("moved the other stage")
	}
	os.RemoveAll(region)
	f.failStep, f.failTimes = helper.RestoreCleanup, 1
	backup := f.run(t, "backup", nil)
	if backup.Status != ops.Failed || !f.running() || f.savedJournal(restore.ID) != "rolling_back old_moved,staging unsafe" {
		t.Fatalf("%+v, running %v, journal %s", backup, f.running(), f.savedJournal(restore.ID))
	}
}
