package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/ops"
)

func (f *fixture) itemBackup(t *testing.T) (backupID, rootID string) {
	t.Helper()
	backup := f.run(t, "backup", nil)
	if backup.Status != ops.Succeeded {
		t.Fatalf("%+v", backup)
	}
	row, err := f.jobs.Backups.Get(ctx, f.serverID, backup.Result.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(f.data, "world", "level.dat"), []byte("newer"), 0o644)
	os.WriteFile(filepath.Join(f.data, "world", "today.dat"), []byte("today"), 0o644)
	return row.ID, row.Roots[0].ID
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(data)
}

func (f *fixture) staged() bool {
	entries, _ := os.ReadDir(f.data)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".ludock-restore-") {
			return true
		}
	}
	return false
}

// Restoring one file needs no typed name, replaces only it, takes no safety
// backup unless asked, and returns the server to running; a failed swap
// puts the current copy back.
func TestRestoreItem(t *testing.T) {
	f := setup(t)
	backupID, rootID := f.itemBackup(t)
	input := map[string]any{"backupId": backupID, "root": rootID, "item": "world/level.dat"}
	f.mu.Lock()
	f.failStep = helper.RestoreMoveNew
	f.mu.Unlock()
	if failed := f.run(t, "restore", input); failed.Status != ops.Failed || read(t, filepath.Join(f.data, "world", "level.dat")) != "newer" ||
		f.staged() || !f.running() {
		t.Fatalf("%+v", failed)
	}
	f.mu.Lock()
	f.failStep = ""
	f.mu.Unlock()
	restored := f.run(t, "restore", input)
	if restored.Status != ops.Succeeded || restored.Result.SafetyBackupID != "" || f.staged() || !f.running() {
		t.Fatalf("%+v", restored)
	}
	if read(t, filepath.Join(f.data, "world", "level.dat")) != "original" || read(t, filepath.Join(f.data, "world", "today.dat")) != "today" {
		t.Fatal("restored more or less than the chosen file")
	}
	if list, _ := f.jobs.Backups.List(ctx, f.serverID); len(list) != 1 {
		t.Fatal("took a safety backup of everything:", len(list))
	}
	os.WriteFile(filepath.Join(f.data, "world", "level.dat"), []byte("newest"), 0o644)
	input["createBackup"] = true
	backedUp := f.run(t, "restore", input)
	if backedUp.Status != ops.Succeeded || backedUp.Result.SafetyBackupID == "" || backedUp.Result.RestoredItem != "world/level.dat" ||
		f.staged() || !f.running() || read(t, filepath.Join(f.data, "world", "level.dat")) != "original" {
		t.Fatalf("%+v", backedUp)
	}
	safety, err := f.jobs.Backups.Get(ctx, f.serverID, backedUp.Result.SafetyBackupID)
	if err != nil || safety.Reason != "restore" || safety.State != "complete" {
		t.Fatal(safety, err)
	}
}

// Forged paths, a path through a symbolic link, and a missing folder are
// refused before the server stops.
func TestRestoreItemRefusesUnsafePaths(t *testing.T) {
	f := setup(t)
	backupID, rootID := f.itemBackup(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "level.dat"), []byte("outside"), 0o644)
	for _, item := range []string{"../escape", "/etc/passwd", "world/../../x", "", "world//level.dat", ".ludock-restore-x/item"} {
		job := f.run(t, "restore", map[string]any{"backupId": backupID, "confirmation": "mc", "root": rootID, "item": item})
		if job.Status != ops.Failed || !strings.Contains(job.Error, "path isn’t allowed") || !f.running() {
			t.Fatalf("%q: %+v", item, job)
		}
	}
	if job := f.run(t, "restore", map[string]any{"backupId": backupID, "confirmation": "mc", "root": "root-9", "item": "world"}); job.Status != ops.Failed {
		t.Fatal("restored into an unknown root")
	}
	if job := f.run(t, "restore", map[string]any{"backupId": backupID, "confirmation": "mc", "root": rootID, "item": "world/missing.dat"}); job.Status != ops.Failed ||
		!strings.Contains(job.Error, "isn’t in this backup") {
		t.Fatalf("%+v", job)
	}
	// The current world folder becomes a link to data outside the root.
	os.RemoveAll(filepath.Join(f.data, "world"))
	os.Symlink(outside, filepath.Join(f.data, "world"))
	job := f.run(t, "restore", map[string]any{"backupId": backupID, "confirmation": "mc", "root": rootID, "item": "world/level.dat"})
	if job.Status != ops.Failed || !strings.Contains(job.Error, "can’t be replaced safely") || !f.running() ||
		read(t, filepath.Join(outside, "level.dat")) != "outside" {
		t.Fatalf("%+v", job)
	}
	os.Remove(filepath.Join(f.data, "world"))
	job = f.run(t, "restore", map[string]any{"backupId": backupID, "confirmation": "mc", "root": rootID, "item": "world/level.dat"})
	if job.Status != ops.Failed || !strings.Contains(job.Error, "no longer exists") || !f.running() {
		t.Fatalf("%+v", job)
	}
}

// A selective restore interrupted after its swap, with the replaced copy
// still staged, is rolled back on startup: the current copy returns, the
// stage is removed, the server starts again, and Activity says it was undone.
func TestRestoreItemInterrupted(t *testing.T) {
	f := setup(t)
	backupID, rootID := f.itemBackup(t)
	logical, _ := f.jobs.Identity.Get(ctx, f.serverID)
	f.runner.Stop()
	job, err := f.runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: "admin", Kind: "restore",
		BindingRevision: logical.BindingRevision, Input: map[string]any{"backupId": backupID, "confirmation": "mc", "root": rootID, "item": "world/level.dat"}})
	if err != nil {
		t.Fatal(err)
	}
	// The swap had finished: the backup's copy is live and the current one
	// waits in the stage.
	if err := f.jobs.Docker.Stop(ctx, f.game, nil); err != nil {
		t.Fatal(err)
	}
	stopped, err := f.jobs.Docker.Inspect(ctx, f.game)
	if err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(f.data, ".ludock-restore-"+job.ID)
	os.MkdirAll(filepath.Join(stage, "new", "world"), 0o700)
	os.MkdirAll(filepath.Join(stage, "old"), 0o700)
	os.Rename(filepath.Join(f.data, "world", "level.dat"), filepath.Join(stage, "old", "item"))
	os.WriteFile(filepath.Join(f.data, "world", "level.dat"), []byte("original"), 0o644)
	journal, _ := json.Marshal(map[string]any{"status": "replacing", "roots": []map[string]any{
		{"root": map[string]any{"id": rootID, "path": f.data}, "item": "world/level.dat", "phase": "replaced"}}})
	running := true
	recovery, _ := json.Marshal(ops.Recovery{ContainerID: logical.ContainerID, Fingerprint: logical.BindingFingerprint,
		LockKeys: []string{locks.ServerKey(f.serverID)}, InitialRunning: &running, StopStarted: true, StopSettled: true,
		StoppedStartedAt: stopped.State.StartedAt, DataUnsafe: true, Restore: journal})
	if _, err := f.jobs.DB.Exec("UPDATE operations SET status='running',phase='restore_replaced',recovery_json=? WHERE id=?", string(recovery), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	recovered, _ := f.runner.Get(ctx, job.ID)
	if recovered.Status != ops.Interrupted || !strings.Contains(recovered.Error, "restarted") ||
		recovered.Result.Guidance != "The restore was undone: the server’s data is as it was. Ludock started the server again." {
		t.Fatalf("%+v", recovered)
	}
	if read(t, filepath.Join(f.data, "world", "level.dat")) != "newer" || read(t, filepath.Join(f.data, "world", "today.dat")) != "today" ||
		f.staged() || !f.running() {
		t.Fatal("recovery did not return the current copy", read(t, filepath.Join(f.data, "world", "level.dat")), f.staged(), f.running())
	}
}

// A restore interrupted after its cleanup finished says it had finished.
func TestRestoreInterruptedAfterCleanup(t *testing.T) {
	f := setup(t)
	backupID, rootID := f.itemBackup(t)
	logical, _ := f.jobs.Identity.Get(ctx, f.serverID)
	f.runner.Stop()
	job, err := f.runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: "admin", Kind: "restore",
		BindingRevision: logical.BindingRevision, Input: map[string]any{"backupId": backupID, "confirmation": "mc", "root": rootID, "item": "world/level.dat"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.jobs.Docker.Stop(ctx, f.game, nil); err != nil {
		t.Fatal(err)
	}
	stopped, _ := f.jobs.Docker.Inspect(ctx, f.game)
	running := true
	recovery, _ := json.Marshal(ops.Recovery{ContainerID: logical.ContainerID, Fingerprint: logical.BindingFingerprint,
		LockKeys: []string{"server:" + f.serverID}, InitialRunning: &running, StopStarted: true, StopSettled: true,
		StoppedStartedAt: stopped.State.StartedAt, Restore: json.RawMessage(`{"status":"cleaned"}`)})
	if _, err := f.jobs.DB.Exec("UPDATE operations SET status='running',phase='restore_cleaned',recovery_json=? WHERE id=?", string(recovery), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	recovered, _ := f.runner.Get(ctx, job.ID)
	if recovered.Status != ops.Succeeded || recovered.Result.Guidance != "Ludock restarted while this ran, after the restore had finished: what it restored is in place. Ludock started the server again." || !f.running() {
		t.Fatalf("%+v", recovered)
	}
}
