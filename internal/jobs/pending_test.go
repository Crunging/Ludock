package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	trace := f.traceRestore(startedAfter("moveOld", "world")(t, f))
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
// the config root held only its stage folder, which the failed restore
// already removed; the world's live data is set aside before its previous
// data returns.
var undoneLines = []string{"cleanup config: rolling_back old_moved,staging unsafe",
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

// After a restore stopped partway, the next backup stops the server,
// finishes undoing the restore before its own copy, keeping what the game
// wrote meanwhile, backs up, and returns the server to running.
func TestNextBackupFinishesStoppedRestore(t *testing.T) {
	f := setup(t)
	restore, trace := f.stoppedRestore(t)
	before := len(trace.lines())
	backup := f.run(t, "backup", nil)
	if backup.Status != ops.Succeeded || !f.running() {
		t.Fatalf("%+v", backup)
	}
	if got := trace.lines()[before:]; !slices.Equal(got, undoneLines) {
		t.Errorf("trace:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(undoneLines, "\n"))
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
