package jobs

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/mountinfo"
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
	if guidance := restore.Result.Guidance; !strings.Contains(guidance, "stopped partway") || !strings.Contains(guidance, helper.StageName(restore.ID)) {
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
	"rollbackOld world: rolling_back rolling_back,staging kept unsafe",
	"cleanup world: rolling_back rolled_back,staging kept unsafe",
	"kept world: rolling_back rolled_back,staging kept unsafe", "kept config: rolling_back rolled_back,staging kept unsafe"}

// checkUndone confirms stoppedRestore was finished: previous data in place,
// what the game wrote kept beside it, no stage folder, and both Activity
// entries saying so, the restore's still at the time it failed.
func (f *fixture) checkUndone(t *testing.T, failed *ops.Job, later *ops.Job) {
	t.Helper()
	restoreID := failed.ID
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
	if guidance := restore.Result.Guidance; restore.Status != ops.Failed || restore.UpdatedAt != failed.UpdatedAt ||
		f.savedJournal(restoreID) != "rolled_back put back kept" ||
		!strings.Contains(guidance, "back in place") || !strings.Contains(guidance, aside) {
		t.Errorf("%+v, journal %s", restore, f.savedJournal(restoreID))
	}
	if undone := later.Result.UndoNote; !strings.Contains(undone, "back in place") || !strings.Contains(undone, aside) {
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
	// The undo is checked before room is made for the backup, again before
	// the stop, and once more when stopped.
	want := append([]string{undoneLines[0], undoneLines[0]}, undoneLines...)
	if got := trace.lines()[before:]; !slices.Equal(got, want) {
		t.Errorf("trace:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	f.checkUndone(t, restore, backup)
	if pending, _ := f.jobs.pendingRestores(ctx, f.serverID); len(pending) != 0 {
		t.Fatal(pending)
	}
}

// A nightly restart that backs up first, after a restore stopped partway,
// finishes the undo, backs up, and leaves the server running.
func TestRestartWithBackupFinishesStoppedRestore(t *testing.T) {
	f := setup(t)
	restore, _ := f.stoppedRestore(t)
	restart := f.run(t, "restart", map[string]any{"createBackup": true})
	if restart.Status != ops.Succeeded || !f.running() {
		t.Fatalf("%+v", restart)
	}
	f.checkUndone(t, restore, restart)
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
	if f.savedJournal(restore.ID) != "rolled_back put back kept" || f.running() {
		t.Fatal(f.savedJournal(restore.ID))
	}
	if read(t, filepath.Join(f.data, "world", "large.dat")) != "changed" ||
		read(t, filepath.Join(f.data, "world", helper.SetAsideName(restore.ID), "generated.dat")) != "generated" {
		t.Fatal("previous data not returned, or the game's writes not kept")
	}
}

// A restore stops partway, then the owner renames the container, which keeps
// its data folders, and an administrator moves the missing server to the
// renamed one. The restore's undo is finished there: by startup while the
// new server is stopped, or by its next backup while it runs.
func TestTakeOverFinishesStoppedRestore(t *testing.T) {
	for _, stopped := range []bool{true, false} {
		t.Run(map[bool]string{true: "startup", false: "backup"}[stopped], func(t *testing.T) {
			f := setup(t)
			restore, _ := f.stoppedRestore(t)
			f.game = f.engine.Recreate(f.game, url.Values{"name": {"mc-world"}})
			if _, err := f.jobs.Servers.RefreshNow(ctx); err != nil {
				t.Fatal(err)
			}
			var renamed string
			f.jobs.DB.QueryRow("SELECT id FROM logical_servers WHERE id<>? AND status='active'", f.serverID).Scan(&renamed)
			if _, err := f.jobs.TakeOver(ctx, f.admin, f.adminSession, f.serverID, renamed, -1, ""); err != nil {
				t.Fatal(err)
			}
			f.serverID = renamed
			if stopped {
				f.runner.Stop()
				if err := f.jobs.Docker.Stop(ctx, f.game, nil); err != nil {
					t.Fatal(err)
				}
				if err := f.runner.Start(ctx, nil); err != nil {
					t.Fatal(err)
				}
				if f.savedJournal(restore.ID) != "rolled_back put back kept" || f.running() ||
					read(t, filepath.Join(f.data, "world", "large.dat")) != "changed" ||
					read(t, filepath.Join(f.data, "world", helper.SetAsideName(restore.ID), "generated.dat")) != "generated" {
					t.Fatal(f.savedJournal(restore.ID))
				}
				return
			}
			backup := f.run(t, "backup", nil)
			if backup.Status != ops.Succeeded || !f.running() {
				t.Fatalf("%+v", backup)
			}
			f.checkUndone(t, restore, backup)
		})
	}
}

// A restore stops partway, then the owner moves the server's data folders
// in Docker, so Ludock can't finish undoing it there. Work that would finish
// it is refused before the server stops, as in refusedBeforeStop, with the
// plain message about that restore, and retention removes no backup for it.
func TestUnfinishableRestoreRefusesBeforeStop(t *testing.T) {
	f := setup(t)
	for range 2 {
		f.jobs.DB.Exec("UPDATE backups SET reason='restore' WHERE id=?", f.run(t, "backup", nil).Result.BackupID)
	}
	restore, _ := f.stoppedRestore(t)
	f.moveData(t)
	// The next backup fits only once retention removes those safety backups.
	settings, _, _ := f.jobs.Backups.SavedSettings(ctx)
	settings.MaxBytes, _, _ = f.jobs.Backups.FoundBytes(ctx)
	if _, err := f.jobs.Backups.SaveSettings(ctx, *settings, f.admin, f.adminSession); err != nil {
		t.Fatal(err)
	}
	f.refusedBeforeStop(t, restore.ID, errPendingBlocked.Error())
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
	if undone := backup.Result.UndoNote; backup.Status != ops.Succeeded || !f.running() || !strings.Contains(undone, "nothing to put back") ||
		f.savedJournal(restoreID) != "rolled_back stage missing" {
		t.Fatalf("%+v, running %v, journal %s", backup, f.running(), f.savedJournal(restoreID))
	}
	if pending, _ := f.jobs.pendingRestores(ctx, f.serverID); len(pending) != 0 {
		t.Fatal(pending)
	}
}

// refusedBeforeStop runs a backup, a restart with a backup first, an undo,
// an update, and the restore again, each with and without a warning to
// players, and requires each to fail with message before anyone is warned,
// a backup is removed to make room, or the server stops, leaving the
// restore's journal as it was.
func (f *fixture) refusedBeforeStop(t *testing.T, restoreID, message string) {
	t.Helper()
	f.jobs.WarningLead = 100 * time.Millisecond
	journal := f.savedJournal(restoreID)
	restore, _ := f.runner.Get(ctx, restoreID)
	backupCount := func() (count int) {
		f.jobs.DB.QueryRow("SELECT COUNT(*) FROM backups WHERE state='complete'").Scan(&count)
		return count
	}
	for _, warn := range []bool{false, true} {
		for _, work := range []struct {
			kind  string
			input map[string]any
		}{
			{ops.KindBackup, map[string]any{}},
			{ops.KindRestart, map[string]any{"createBackup": true}},
			{ops.KindUndoRestore, map[string]any{}},
			{ops.KindUpdate, map[string]any{"request": skipBackup["request"]}},
			{ops.KindRestore, map[string]any{"backupId": restore.Input["backupId"], "confirmation": "mc", MovedInput: true}},
		} {
			if warn {
				work.input[WarnInput] = true
			}
			before, backups := len(f.engine.Requests), backupCount()
			job := f.run(t, work.kind, work.input)
			stopped := slices.ContainsFunc(f.engine.Requests[before:], func(request string) bool { return strings.Contains(request, f.game+"/stop") })
			if job.Status != ops.Failed || job.Error != message || job.Recovery.WarnUntil != 0 || stopped || !f.running() ||
				backupCount() != backups || f.savedJournal(restoreID) != journal {
				t.Errorf("%s, warn %v: %s %q, warned %v, stopped %v, running %v, backups %d of %d, journal %s", work.kind, warn, job.Status, job.Error,
					job.Recovery.WarnUntil != 0, stopped, f.running(), backupCount(), backups, f.savedJournal(restoreID))
			}
		}
	}
}

// Finishing a restore that stopped partway would move live data holding
// another restore's stage folder, or a mount the host added: work that would
// finish it is refused before the server stops, as in refusedBeforeStop. A
// step that fails after the stop, before any live data moves, fails the
// backup, but the server it stopped is running again and the undo is left
// for the next operation.
func TestFailedUndoReturnsServerToRunning(t *testing.T) {
	f := setup(t)
	restore, _ := f.stoppedRestore(t)
	region := filepath.Join(f.data, "world", "region")
	other := filepath.Join(region, helper.StageName("2f9619ff-8b86-4011-b42d-00c04fc964ff"))
	os.MkdirAll(filepath.Join(other, "old"), 0o700)
	f.refusedBeforeStop(t, restore.ID, backups.StageInside(region).Message)
	if _, err := os.Stat(filepath.Join(other, "old")); err != nil {
		t.Fatal("moved the other stage")
	}
	previous := mountinfo.Table
	t.Cleanup(func() { mountinfo.Table = previous })
	mountinfo.Table = func() ([]byte, error) { return []byte("40 22 0:45 / " + region + " rw - nfs4 server:/share rw\n"), nil }
	f.refusedBeforeStop(t, restore.ID, backups.ErrHostMount.Message)
	mountinfo.Table = previous
	os.RemoveAll(region)
	f.failStep, f.failTimes = helper.RestoreCleanup, 1
	backup := f.run(t, "backup", nil)
	if backup.Status != ops.Failed || !f.running() || f.savedJournal(restore.ID) != "rolling_back old_moved,staging unsafe" {
		t.Fatalf("%+v, running %v, journal %s", backup, f.running(), f.savedJournal(restore.ID))
	}
}

// Ludock restarts while an Undo runs. It ends as what it did: finished, with
// its note, once no restore is left to undo; skipped when it undid nothing,
// as when other work finished the restore first; and interrupted, saying to
// undo again, while a restore is still left.
func TestUndoRecoveryEndsAsWhatItDid(t *testing.T) {
	for _, c := range []struct {
		name, undone, status, guidance string
		pending                        bool
	}{
		{name: "finished", undone: "Ludock finished undoing a restore that was stopped partway.", status: ops.Succeeded,
			guidance: "Ludock restarted while this ran, and finished it."},
		{name: "nothing to undo", status: ops.Skipped, guidance: "Ludock restarted while this ran. There was nothing left to undo."},
		{name: "restore still left", pending: true, status: ops.Interrupted,
			guidance: "Ludock restarted before the undo finished. Undo the restore again from Needs attention."},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			f.runner.Stop()
			logical, _ := f.jobs.Identity.Get(ctx, f.serverID)
			if c.pending {
				if _, err := f.jobs.DB.Exec(`INSERT INTO operations (id,server_id,actor_id,kind,status,phase,input_json,recovery_json,binding_revision,created_at,updated_at)
					VALUES (?,?,'admin','restore','failed','failed','{}',?,?,1,1)`, identity.NewUUID(), f.serverID,
					`{"restore":{"status":"replacing","roots":[{"root":{"id":"root-1","path":"/data"},"phase":"old_moved"}]}}`, logical.BindingRevision); err != nil {
					t.Fatal(err)
				}
			}
			recovery, _ := json.Marshal(ops.Recovery{Undone: c.undone})
			id := identity.NewUUID()
			if _, err := f.jobs.DB.Exec(`INSERT INTO operations (id,server_id,actor_id,kind,status,phase,input_json,recovery_json,binding_revision,created_at,updated_at)
				VALUES (?,?,'admin','undo_restore','running','stopping','{}',?,?,2,2)`, id, f.serverID, string(recovery), logical.BindingRevision); err != nil {
				t.Fatal(err)
			}
			runner := &ops.Runner{DB: f.jobs.DB, Locks: f.jobs.Locks}
			f.jobs.Register(runner)
			if err := runner.Start(ctx, nil); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(runner.Stop)
			job, _ := runner.Get(ctx, id)
			if job.Status != c.status || job.Result.Guidance != c.guidance || job.Result.UndoNote != c.undone {
				t.Fatalf("%s %q %q, undo %q", job.Status, job.Error, job.Result.Guidance, job.Result.UndoNote)
			}
		})
	}
}

// Another container that can write to the server's data starts while a
// restore moves it, after the old data moved out or once the backup's is in
// place, and writes a save there. The restore stops, and the owner stops
// that container. Whichever finishes the undo, Undo, the next backup, or
// startup with the server stopped, puts the previous data back and keeps the
// save, never deleting it: nothing tells it apart from what the restore put
// there.
func TestUndoKeepsWhatAnotherWriterWrote(t *testing.T) {
	for _, step := range []string{helper.RestoreMoveOld, helper.RestoreMoveNew} {
		for _, finish := range []string{"undo", "backup", "startup"} {
			t.Run(step+" "+finish, func(t *testing.T) {
				f := setup(t)
				input := map[string]any{"backupId": twoRootBackup(t, f), "confirmation": "mc"}
				for _, root := range []string{"world", "config"} {
					os.WriteFile(filepath.Join(f.data, root, "large.dat"), []byte("changed"), 0o644)
				}
				f.traceRestore(once(step, "world", func(_ *fakedocker.Container, code int) int {
					f.writeElsewhere()
					os.WriteFile(filepath.Join(f.data, "world", "sidecar-save"), []byte("new progress"), 0o600)
					return code
				}))
				if restore := f.run(t, "restore", input); restore.Status != ops.Failed || f.running() {
					t.Fatalf("%+v", restore)
				}
				if err := f.jobs.Docker.Stop(ctx, f.containerNamed("sidecar").ID, nil); err != nil {
					t.Fatal(err)
				}
				var later *ops.Job
				switch finish {
				case "startup":
					f.runner.Stop()
					if err := f.runner.Start(ctx, nil); err != nil {
						t.Fatal(err)
					}
				default:
					if later = f.run(t, map[string]string{"undo": ops.KindUndoRestore, "backup": "backup"}[finish], nil); later.Status != ops.Succeeded {
						t.Fatalf("%+v", later)
					}
				}
				restore := lastRestore(f)
				saved := filepath.Join(f.data, "world", helper.SetAsideName(restore), "sidecar-save")
				if read(t, filepath.Join(f.data, "world", "large.dat")) != "changed" || read(t, saved) != "new progress" {
					t.Errorf("previous data not back, or the save not kept; journal %s", f.savedJournal(restore))
				}
				if later != nil && !strings.Contains(later.Result.UndoNote, helper.SetAsideName(restore)) {
					t.Errorf("%+v", later.Result)
				}
			})
		}
	}
}

// Something outside Ludock starts the server while an undo of a stopped
// restore clears the live folder or returns the previous data. The undo's
// helper stops within about a second, the restore stays pending, and the
// undo says the server was started. Whichever finishes it next, Undo, the
// next backup, or startup with the server stopped, puts the previous data
// back and keeps what the game wrote.
func TestUndoStopsWhenStarted(t *testing.T) {
	for _, step := range []string{helper.RestoreSetAside, helper.RestoreRollbackOld} {
		for _, finish := range []string{"undo", "backup", "startup"} {
			t.Run(step+" "+finish, func(t *testing.T) {
				f := setup(t)
				restore, _ := f.stoppedRestore(t)
				startedDuring(step, "world")(t, f)
				undo := f.run(t, ops.KindUndoRestore, nil)
				if undo.Status != ops.Failed || !strings.Contains(undo.Error, "started or changed the server meanwhile") || !f.running() {
					t.Fatalf("%s %q, running %v", undo.Status, undo.Error, f.running())
				}
				if pending, _ := f.jobs.pendingRestores(ctx, f.serverID); len(pending) != 1 {
					t.Fatalf("pending %v, journal %s", pending, f.savedJournal(restore.ID))
				}
				var later *ops.Job
				switch finish {
				case "startup":
					f.runner.Stop()
					if err := f.jobs.Docker.Stop(ctx, f.game, nil); err != nil {
						t.Fatal(err)
					}
					if err := f.runner.Start(ctx, nil); err != nil {
						t.Fatal(err)
					}
					later = undo
				default:
					if later = f.run(t, map[string]string{"undo": ops.KindUndoRestore, "backup": "backup"}[finish], nil); later.Status != ops.Succeeded {
						t.Fatalf("%+v", later)
					}
				}
				if f.savedJournal(restore.ID) != "rolled_back put back kept" || read(t, filepath.Join(f.data, "world", "large.dat")) != "changed" ||
					read(t, filepath.Join(f.data, "world", helper.SetAsideName(restore.ID), "generated.dat")) != "generated" {
					t.Errorf("journal %s", f.savedJournal(restore.ID))
				}
			})
		}
	}
}
