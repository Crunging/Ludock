package backups

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
)

type copyFixture struct {
	s                 *Service
	main, copies      string
	serverID, binding string
	settings          Settings
	failures          []string
	engine            *fakedocker.Engine
}

// newCopyFixture has a backup folder and a second folder, both mounted into
// Ludock's container, with settings that copy to the second one.
func newCopyFixture(t *testing.T) *copyFixture {
	t.Helper()
	d := dbtest.Open(t)
	store := &identity.Store{DB: d}
	list, err := store.Reconcile(ctx, []identity.Observation{{ContainerID: "mc", Name: "mc", DisplayName: "Minecraft", GameType: "minecraft"}}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	f := &copyFixture{main: filepath.Join(base, "backups"), copies: filepath.Join(base, "copies"), serverID: list[0].ID,
		binding: list[0].BindingFingerprint}
	os.Mkdir(f.main, 0o755)
	os.Mkdir(f.copies, 0o755)
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	f.engine = engine
	engine.AddContainer(fakedocker.GameOptions{ID: "ludock", Name: "ludock", Image: image, Running: true, Mounts: []docker.MountPoint{
		{Type: "bind", Source: f.main, Destination: f.main, RW: true}, {Type: "bind", Source: f.copies, Destination: f.copies, RW: true}}})
	client := docker.New(engine.Socket)
	f.s = &Service{DB: d, Docker: client, Storage: &Storage{Docker: client, Roots: f.main + ":" + f.copies, Self: "ludock"}, Locks: locks.New()}
	f.s.CopyFailed = func(_ context.Context, _ *sql.Tx, _, _, problem string) error {
		f.failures = append(f.failures, problem)
		return nil
	}
	f.settings = Settings{Destination: f.main, CopyTo: f.copies}
	if _, err := d.Exec(`INSERT INTO settings(key,value_json) VALUES('backups',?)`,
		`{"destination":"`+f.main+`","maxBytes":0,"reserveBytes":0,"copyTo":"`+f.copies+`"}`); err != nil {
		t.Fatal(err)
	}
	return f
}

// add records a backup whose archive holds content.
func (f *copyFixture) add(t *testing.T, content string, createdAt int64) Backup {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	backup := Backup{ID: identity.NewUUID(), ServerID: f.serverID, CreatedAt: createdAt, State: "complete",
		Size: int64(len(content)), Checksum: hex.EncodeToString(sum[:])}
	if err := os.WriteFile(filepath.Join(f.main, backup.ID+".tar"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Record(ctx, backup, f.binding, f.main); err != nil {
		t.Fatal(err)
	}
	return backup
}

func (f *copyFixture) row(t *testing.T, id string) (state string, attempts int, nextAt int64, problem string) {
	t.Helper()
	if err := f.s.DB.QueryRow("SELECT copy_state,copy_attempts,copy_next_at,copy_error FROM backups WHERE id=?", id).
		Scan(&state, &attempts, &nextAt, &problem); err != nil {
		t.Fatal(err)
	}
	return state, attempts, nextAt, problem
}

// due makes every waiting copy due now, as if its backoff had passed.
func (f *copyFixture) due(t *testing.T) {
	t.Helper()
	if _, err := f.s.DB.Exec("UPDATE backups SET copy_next_at=0"); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Ludock restarted during a copy: the attempt was counted and a partial file
// was left. The new process waits out the backoff, then replaces the partial
// with a verified copy, and makes it only once.
func TestCopyResumesAfterRestart(t *testing.T) {
	f := newCopyFixture(t)
	backup := f.add(t, "world data", 1)
	partial := filepath.Join(f.copies, backup.ID+".tar.partial")
	os.WriteFile(partial, []byte("world"), 0o600)
	f.s.DB.Exec("UPDATE backups SET copy_attempts=1, copy_next_at=? WHERE id=?", time.Now().Add(time.Minute).UnixMilli(), backup.ID)

	restarted := &Service{DB: f.s.DB, Docker: f.s.Docker, Storage: &Storage{Docker: f.s.Docker, Roots: f.s.Storage.Roots, Self: "ludock"},
		Locks: locks.New()}
	if err := restarted.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	if state, attempts, _, _ := f.row(t, backup.ID); state != "pending" || attempts != 1 {
		t.Fatal("copied before the interrupted attempt's backoff", state, attempts)
	}
	f.due(t)
	if err := restarted.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	state, attempts, _, _ := f.row(t, backup.ID)
	copied, _ := os.ReadFile(filepath.Join(f.copies, backup.ID+".tar"))
	if state != "complete" || attempts != 2 || string(copied) != "world data" || exists(partial) {
		t.Fatal(state, attempts, string(copied), exists(partial))
	}
	f.due(t)
	if err := restarted.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, again, _, _ := f.row(t, backup.ID); again != 2 {
		t.Fatal("copied a complete copy again")
	}

	// A restart between publishing a copy and recording it: the copy is
	// adopted, even with the original gone since.
	renamed := f.add(t, "world data", 2)
	os.Rename(filepath.Join(f.main, renamed.ID+".tar"), filepath.Join(f.copies, renamed.ID+".tar"))
	if err := restarted.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _, _, _ := f.row(t, renamed.ID); state != "complete" {
		t.Fatal("did not adopt the published copy", state)
	}
}

// A missing second folder never touches the backup: each attempt records
// why, the copy is reported once after several attempts, and it is made
// once the folder is back.
func TestCopyFailingFolder(t *testing.T) {
	f := newCopyFixture(t)
	backup := f.add(t, "world data", 1)
	os.Remove(f.copies)
	for attempt := 1; attempt <= copyAttemptsBeforeAlert+1; attempt++ {
		f.due(t)
		if err := f.s.CopyDue(ctx); err != nil {
			t.Fatal(err)
		}
		state, attempts, nextAt, problem := f.row(t, backup.ID)
		want := "pending"
		if attempt >= copyAttemptsBeforeAlert {
			want = "failed"
		}
		if state != want || attempts != attempt || problem != errCopyMissing.Message || nextAt <= time.Now().UnixMilli() {
			t.Fatal(attempt, state, attempts, problem, nextAt)
		}
	}
	if len(f.failures) != 1 || f.failures[0] != errCopyMissing.Message {
		t.Fatal("reported", f.failures)
	}
	if got, err := f.s.Get(ctx, f.serverID, backup.ID); err != nil || got.State != "complete" {
		t.Fatal("the backup itself changed", err)
	}
	os.Mkdir(f.copies, 0o755)
	f.due(t)
	if err := f.s.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _, _, problem := f.row(t, backup.ID); state != "complete" || problem != "" || !exists(filepath.Join(f.copies, backup.ID+".tar")) {
		t.Fatal(state, problem)
	}
}

// A copy counts only when it matches the original: an archive changed since
// it was made, a written copy that reads back differently, and a copy that
// stops making progress all leave nothing behind.
func TestCopyVerification(t *testing.T) {
	f := newCopyFixture(t)
	backup := f.add(t, "world data", 1)
	os.WriteFile(filepath.Join(f.main, backup.ID+".tar"), []byte("WORLD DATA"), 0o600)
	if err := f.s.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _, _, problem := f.row(t, backup.ID); state != "pending" || problem != errCopySource.Message {
		t.Fatal(state, problem)
	}
	if entries, _ := os.ReadDir(f.copies); len(entries) != 0 {
		t.Fatal("left a copy of a changed archive", entries)
	}

	dir, err := f.s.Storage.pin(f.copies)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.close()
	os.WriteFile(filepath.Join(f.copies, "written"), []byte("world dat4"), 0o600)
	if err := verifyFile(ctx, dir, "written", backup.Size, backup.Checksum); !apperr.Is(err, "BACKUP_COPY_MISMATCH") {
		t.Fatal(err)
	}

	stalled, cancel := context.WithCancel(ctx)
	source := io.MultiReader(bytes.NewReader([]byte("world")), readerFunc(func([]byte) (int, error) {
		cancel()
		return 0, nil
	}), bytes.NewReader([]byte(" data")))
	err = f.s.Storage.CopyFile(stalled, source, f.copies, backup.ID+".tar", backup.Size, backup.Checksum, 0, nil)
	if !apperr.Is(err, "BACKUP_COPY_STALLED") || exists(filepath.Join(f.copies, backup.ID+".tar.partial")) {
		t.Fatal(err)
	}
}

type readerFunc func([]byte) (int, error)

func (r readerFunc) Read(p []byte) (int, error) { return r(p) }

// Retention and deletion remove a backup's copy with it, kept backups keep
// theirs, and a copy whose folder is unavailable is removed once it is back.
// Retention removes copies through the copier, never on the backup's path.
// A backup missing from its folder is read from its copy.
func TestRemovingABackupRemovesItsCopy(t *testing.T) {
	f := newCopyFixture(t)
	var list []Backup
	for index := range 5 {
		list = append(list, f.add(t, "world data", int64(index+1)*60_000))
	}
	if err := f.s.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	copyOf := func(backup Backup) string { return filepath.Join(f.copies, backup.ID+".tar") }
	for _, backup := range list {
		if !exists(copyOf(backup)) {
			t.Fatal("no copy of", backup.ID)
		}
	}
	// list is oldest first: keep the oldest; the second expires.
	f.s.DB.Exec("UPDATE backups SET kept_at=1 WHERE id=?", list[0].ID)
	if err := f.s.Prune(ctx, f.serverID, f.settings, list[4].ID); err != nil {
		t.Fatal(err)
	}
	// The copier removes copies, so retention never waits on the second
	// folder.
	if !exists(copyOf(list[1])) {
		t.Fatal("retention touched the second folder")
	}
	if err := f.s.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	if exists(copyOf(list[1])) || !exists(copyOf(list[0])) {
		t.Fatal("retention left the expired copy or removed the kept one")
	}

	os.Remove(filepath.Join(f.main, list[2].ID+".tar"))
	missing, _ := f.s.Get(ctx, f.serverID, list[2].ID)
	if folder := f.s.ArchiveFolder(missing); folder != f.copies || !missing.FromCopy {
		t.Fatal("not read from the copy", folder)
	}
	archive, err := f.s.Open(ctx, f.serverID, list[2].ID)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(archive)
	archive.Close()
	if string(content) != "world data" {
		t.Fatal(string(content))
	}

	unplugged := f.copies + "-unplugged"
	os.Rename(f.copies, unplugged)
	if err := f.s.Delete(ctx, f.serverID, list[3].ID, nil); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(f.main, list[3].ID+".tar")) {
		t.Fatal("kept the archive while the second folder was away")
	}
	if err := f.s.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	if found, _ := f.s.Exists(ctx, list[3].ID, false); !found {
		t.Fatal("forgot a copy it could not remove")
	}
	os.Rename(unplugged, f.copies)
	if err := f.s.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	if found, _ := f.s.Exists(ctx, list[3].ID, false); found || exists(copyOf(list[3])) {
		t.Fatal("did not remove the copy once its folder was back")
	}
}

// Choosing a second folder queues a copy of every backup without one, and
// turning it off stops copies not made yet; settings can't put the copy in
// the backup folder.
func TestChoosingTheSecondFolder(t *testing.T) {
	f := newCopyFixture(t)
	f.s.DB.Exec(`UPDATE settings SET value_json=json_remove(value_json,'$.copyTo') WHERE key='backups'`)
	backup := f.add(t, "world data", 1)
	if state, _, _, _ := f.row(t, backup.ID); state != "" {
		t.Fatal(state)
	}
	if err := f.s.DB.Tx(ctx, func(tx *sql.Tx) error { return retarget(ctx, tx, f.copies) }); err != nil {
		t.Fatal(err)
	}
	if state, _, _, _ := f.row(t, backup.ID); state != "pending" {
		t.Fatal(state)
	}
	if err := f.s.DB.Tx(ctx, func(tx *sql.Tx) error { return retarget(ctx, tx, "") }); err != nil {
		t.Fatal(err)
	}
	if state, _, _, _ := f.row(t, backup.ID); state != "" {
		t.Fatal(state)
	}
	for _, folder := range []string{f.main, filepath.Join(f.main, "copies")} {
		os.MkdirAll(folder, 0o755)
		if err := f.s.ValidateCopyFolder(ctx, Settings{Destination: f.main, CopyTo: folder}); !apperr.Is(err, "BACKUP_COPY_OVERLAP") {
			t.Fatal(folder, err)
		}
	}
	if err := f.s.ValidateCopyFolder(ctx, Settings{Destination: f.main, CopyTo: f.copies}); err != nil {
		t.Fatal(err)
	}
	// Another container path for the backup folder's host folder is no
	// second copy.
	alias := filepath.Join(filepath.Dir(f.main), "alias")
	os.Mkdir(alias, 0o755)
	f.s.Storage.Roots += ":" + alias
	f.engine.Update("ludock", func(c *fakedocker.Container) {
		c.Mounts = append(c.Mounts, docker.MountPoint{Type: "bind", Source: f.main, Destination: alias, RW: true})
	})
	if err := f.s.ValidateCopyFolder(ctx, Settings{Destination: f.main, CopyTo: alias}); !apperr.Is(err, "BACKUP_COPY_OVERLAP") {
		t.Fatal(err)
	}
}

// A folder mounted at /backups-copy is approved beside the configured roots,
// but not when backups are off or when the folder is only a directory.
func TestCopyRootApprovedWhenMounted(t *testing.T) {
	info := filepath.Join(t.TempDir(), "mountinfo")
	previous := mountInfo
	mountInfo = info
	t.Cleanup(func() { mountInfo = previous })
	storage := &Storage{Roots: "/backups"}
	os.WriteFile(info, []byte("36 35 98:0 /mnt1 /backups rw - ext4 /dev/root rw\n"), 0o600)
	if roots := storage.ApprovedRoots(); len(roots) != 1 {
		t.Fatal(roots)
	}
	os.WriteFile(info, []byte("36 35 98:0 /mnt1 /backups rw - ext4 /dev/root rw\n37 35 98:1 /nas /backups-copy rw - nfs nas:/x rw\n"), 0o600)
	if roots := storage.ApprovedRoots(); len(roots) != 2 || roots[1] != CopyRoot {
		t.Fatal(roots)
	}
	if roots := (&Storage{}).ApprovedRoots(); len(roots) != 0 {
		t.Fatal("offered a second folder with backups off", roots)
	}
}

// The second copy is turned off and on while an attempt that will fail is
// running: the new request starts over, so the old attempt neither records
// its failure on it nor reports it.
func TestCopyFailureAfterRetargetIsDropped(t *testing.T) {
	f := newCopyFixture(t)
	backup := f.add(t, "world data", 1)
	f.s.DB.Exec("UPDATE backups SET copy_attempts=? WHERE id=?", copyAttemptsBeforeAlert-1, backup.ID)
	os.Remove(f.copies)
	testHook = func(point string) {
		if point != "copying" {
			return
		}
		testHook = nil
		for _, folder := range []string{"", f.copies} {
			if err := f.s.DB.Tx(ctx, func(tx *sql.Tx) error { return retarget(ctx, tx, folder) }); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { testHook = nil })
	if err := f.s.copyOne(ctx, mustGet(t, f, backup.ID), 0); err != nil {
		t.Fatal(err)
	}
	if state, attempts, _, problem := f.row(t, backup.ID); state != "pending" || attempts != 0 || problem != "" || len(f.failures) != 0 {
		t.Fatal(state, attempts, problem, f.failures)
	}
}

func mustGet(t *testing.T, f *copyFixture, id string) *Backup {
	t.Helper()
	backup, err := f.s.Get(ctx, f.serverID, id)
	if err != nil {
		t.Fatal(err)
	}
	return backup
}
