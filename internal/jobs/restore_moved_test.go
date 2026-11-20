package jobs

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/ops"
)

// moveData copies the server's data to a new host folder and points its
// mount there, keeping the path inside the container, as a rebuilt host would.
func (f *fixture) moveData(t *testing.T) string {
	t.Helper()
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.CopyFS(moved, os.DirFS(f.data)); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(moved, "world", "level.dat"), []byte("after the move"), 0o644)
	f.game = f.engine.Recreate(f.game, url.Values{"mount": {f.data + "=" + moved}})
	if _, err := f.jobs.Servers.RefreshNow(ctx); err != nil {
		t.Fatal(err)
	}
	return moved
}

// A backup taken before the data moved restores into the new host folder
// only when the job carries the confirmation, with the usual safety backup,
// and leaves the old folder alone.
func TestRestoreAfterDataMoved(t *testing.T) {
	f := setup(t)
	backup := f.run(t, "backup", nil)
	backupID := backup.Result["backupId"].(string)
	moved := f.moveData(t)
	unconfirmed := f.run(t, "restore", map[string]any{"backupId": backupID, "confirmation": "mc"})
	if unconfirmed.Status != ops.Failed || !strings.Contains(unconfirmed.Error, "lived somewhere else") || !f.running() ||
		read(t, filepath.Join(moved, "world", "level.dat")) != "after the move" {
		t.Fatalf("%+v", unconfirmed)
	}
	os.WriteFile(filepath.Join(f.data, "world", "level.dat"), []byte("left behind"), 0o644)
	restored := f.run(t, "restore", map[string]any{"backupId": backupID, "confirmation": "mc", MovedInput: true})
	if restored.Status != ops.Succeeded || restored.Result[MovedInput] != true || restored.Result["safetyBackupId"] == nil || !f.running() {
		t.Fatalf("%+v", restored)
	}
	if read(t, filepath.Join(moved, "world", "level.dat")) != "original" || read(t, filepath.Join(f.data, "world", "level.dat")) != "left behind" {
		t.Fatal("restored into the wrong folder")
	}
	// The safety backup is of the data where it is now, so it restores as usual.
	safety := restored.Result["safetyBackupId"].(string)
	if again := f.run(t, "restore", map[string]any{"backupId": safety, "confirmation": "mc"}); again.Status != ops.Succeeded ||
		read(t, filepath.Join(moved, "world", "level.dat")) != "after the move" {
		t.Fatalf("%+v", again)
	}
}

// A backup whose folder is another path inside the server is refused,
// with or without the confirmation, before the server stops.
func TestRestoreRefusesOtherFolders(t *testing.T) {
	f := setup(t)
	backup := f.run(t, "backup", nil)
	f.game = f.engine.Recreate(f.game, url.Values{"label": {"ludock.files=" + filepath.Join(f.data, "world")}})
	if _, err := f.jobs.Servers.RefreshNow(ctx); err != nil {
		t.Fatal(err)
	}
	job := f.run(t, "restore", map[string]any{"backupId": backup.Result["backupId"], "confirmation": "mc", MovedInput: true})
	if job.Status != ops.Failed || !strings.Contains(job.Error, "which the server no longer has") || !f.running() {
		t.Fatalf("%+v", job)
	}
	if strings.Contains(strings.Join(f.commands, "\n"), `"operation":"moveOld"`) {
		t.Fatal("replaced data")
	}
}
