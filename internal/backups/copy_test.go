package backups

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
)

type copyFixture struct {
	s                 *Service
	main, copies      string
	other             string // a third approved folder
	serverID, binding string
	settings          Settings
	failures          []string
	engine            *fakedocker.Engine
}

// newCopyFixture has a backup folder and two other folders, all mounted into
// Ludock's container, with settings that copy to the first of the others.
func newCopyFixture(t *testing.T) *copyFixture {
	t.Helper()
	d := dbtest.Open(t)
	store := &identity.Store{DB: d}
	list, err := store.Reconcile(ctx, []identity.Observation{{ContainerID: "mc", Name: "mc", DisplayName: "Minecraft", GameType: "minecraft"}}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	f := &copyFixture{main: filepath.Join(base, "backups"), copies: filepath.Join(base, "copies"), other: filepath.Join(base, "other"),
		serverID: list[0].ID, binding: list[0].BindingFingerprint}
	for _, folder := range []string{f.main, f.copies, f.other} {
		os.Mkdir(folder, 0o755)
	}
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	f.engine = engine
	engine.AddContainer(fakedocker.GameOptions{ID: "ludock", Name: "ludock", Image: image, Running: true, Mounts: []docker.MountPoint{
		{Type: "bind", Source: f.main, Destination: f.main, RW: true}, {Type: "bind", Source: f.copies, Destination: f.copies, RW: true},
		{Type: "bind", Source: f.other, Destination: f.other, RW: true}}})
	client := docker.New(engine.Socket)
	f.s = &Service{DB: d, Docker: client, Storage: &Storage{Docker: client, Roots: f.main + ":" + f.copies + ":" + f.other, Self: "ludock"}, Locks: locks.New()}
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
// with a verified copy dated as its backup, and makes it only once.
func TestCopyResumesAfterRestart(t *testing.T) {
	f := newCopyFixture(t)
	backup := f.add(t, "world data", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).UnixMilli())
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
	if info, err := os.Stat(filepath.Join(f.copies, backup.ID+".tar")); err != nil || info.ModTime().UnixMilli() != backup.CreatedAt {
		t.Fatal("the copy isn't dated as its backup", info.ModTime(), err)
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
	err = f.s.Storage.CopyFile(stalled, source, f.copies, backup.ID+".tar", backup.Size, backup.Checksum, 0, nil, time.Time{})
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
	if folder, _ := f.s.ArchiveFolder(missing); folder != f.copies || !missing.FromCopy {
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
	if _, err := f.s.Delete(ctx, f.serverID, list[3].ID, false, nil); err != nil {
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

// A found copy folder starts second copies when no choice was made, but a
// choice saved while the folder was being checked wins, and an
// administrator's choice of none sticks.
func TestFoundCopyFolderStartsCopies(t *testing.T) {
	f := newCopyFixture(t)
	f.s.DB.Exec(`UPDATE settings SET value_json=json_remove(value_json,'$.copyTo') WHERE key='backups'`)
	f.s.Storage.DevCopyRoot = f.other
	backup := f.add(t, "world data", 1)
	stale, _, _ := f.s.SavedSettings(ctx)
	chosen := Settings{Destination: f.main, CopyChosen: true}
	if err := f.s.DB.SetSetting(ctx, nil, settingsKey, chosen); err != nil {
		t.Fatal(err)
	}
	if err := f.s.startCopies(ctx, *stale); err != nil {
		t.Fatal(err)
	}
	if saved, _, _ := f.s.SavedSettings(ctx); *saved != chosen {
		t.Fatal("overrode a choice saved meanwhile:", saved)
	}
	if err := f.s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if saved, _, _ := f.s.SavedSettings(ctx); saved.CopyTo != "" {
		t.Fatal("overrode a choice of no second copy:", saved)
	}
	if err := f.s.DB.SetSetting(ctx, nil, settingsKey, *stale); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if saved, _, _ := f.s.SavedSettings(ctx); saved.CopyTo != f.other || saved.CopyChosen {
		t.Fatal("didn't start copies to the found folder:", saved)
	}
	if state, _, _, _ := f.row(t, backup.ID); state != "pending" {
		t.Fatal("existing backup not queued for a copy:", state)
	}
	if status := f.s.Status(ctx); !status.CopyFound || status.CopyTo != f.other {
		t.Fatalf("%+v", status)
	}
	// Settings removed while the folder was checked are a changed choice too.
	f.s.DB.Exec(`DELETE FROM settings WHERE key='backups'`)
	if err := f.s.startCopies(ctx, *stale); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := f.s.SavedSettings(ctx); found {
		t.Fatal("brought back removed settings")
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

// The administrator moves the second copy to another folder. Backups already
// copied to the old one are copied again, and until then the page doesn't
// call them saved; the old copy still stands in for a missing archive and is
// the source of the new one. Once the new copy is made the old file is left
// where it is, untracked. Turning the copy off keeps the copy tracked, so it
// is still removed with its backup.
func TestChangingTheSecondFolderCopiesAgain(t *testing.T) {
	f := newCopyFixture(t)
	backup := f.add(t, "world data", 1)
	if err := f.s.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.Tx(ctx, func(tx *sql.Tx) error { return retarget(ctx, tx, f.other) }); err != nil {
		t.Fatal(err)
	}
	if state, attempts, _, _ := f.row(t, backup.ID); state != "pending" || attempts != 0 {
		t.Fatal("still counted as saved in the old folder", state, attempts)
	}
	os.Remove(filepath.Join(f.main, backup.ID+".tar"))
	missing := mustGet(t, f, backup.ID)
	if folder, _ := f.s.ArchiveFolder(missing); folder != f.copies || !missing.FromCopy {
		t.Fatal("the old copy no longer stands in", folder)
	}
	if err := f.s.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	moved, _ := os.ReadFile(filepath.Join(f.other, backup.ID+".tar"))
	if state, _, _, problem := f.row(t, backup.ID); state != "complete" || string(moved) != "world data" {
		t.Fatal(state, problem, string(moved))
	}
	old := filepath.Join(f.copies, backup.ID+".tar")
	if !exists(old) {
		t.Fatal("removed the copy in the old folder")
	}

	if err := f.s.DB.Tx(ctx, func(tx *sql.Tx) error { return retarget(ctx, tx, "") }); err != nil {
		t.Fatal(err)
	}
	if state, _, _, _ := f.row(t, backup.ID); state != "" {
		t.Fatal("still counted as saved with no folder chosen", state)
	}
	if _, err := f.s.Delete(ctx, f.serverID, backup.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	if found, _ := f.s.Exists(ctx, backup.ID, false); found || exists(filepath.Join(f.other, backup.ID+".tar")) || !exists(old) {
		t.Fatal("the tracked copy wasn't removed with its backup, or the untracked one was", found)
	}
}

// A second folder stops responding mid-write, with the write blocked where
// cancellation can't reach it. The copier gives the attempt up, records it,
// and returns, as does a shutdown; nothing else is started in that folder
// until the stuck write returns. The abandoned attempt then neither
// publishes nor leaves its partial file, and the next attempt succeeds.
func TestStuckSecondFolderIsAbandoned(t *testing.T) {
	f := newCopyFixture(t)
	backup := f.add(t, "world data", 1)
	previous := copyStall
	copyStall = 100 * time.Millisecond
	t.Cleanup(func() { copyStall = previous })
	release := make(chan struct{})
	var blocked atomic.Int32
	testHook = func(point string) {
		if point == "writing" && blocked.Add(1) <= 2 {
			<-release
		}
	}
	t.Cleanup(func() { testHook = nil })
	within := func(what string, run func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- run() }()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(what, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal(what, "waited on a stuck write")
		}
	}
	stuck := func() bool { return f.s.Storage.folders.busyIn(f.copies, laneWork) > 0 }

	within("copying", func() error { return f.s.CopyDue(ctx) })
	if state, attempts, _, problem := f.row(t, backup.ID); state != "pending" || attempts != 1 || problem != errCopyStalled.Message {
		t.Fatal(state, attempts, problem)
	}
	f.due(t)
	within("retrying", func() error { return f.s.CopyDue(ctx) })
	if _, attempts, _, problem := f.row(t, backup.ID); attempts != 2 || problem != errCopyStalled.Message || blocked.Load() != 1 {
		t.Fatal("started more I/O in a stuck folder", attempts, problem, blocked.Load())
	}
	close(release)
	for deadline := time.Now().Add(5 * time.Second); stuck(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the abandoned write never finished")
		}
	}
	if entries, _ := os.ReadDir(f.copies); len(entries) != 0 {
		t.Fatal("the abandoned attempt left files", entries)
	}

	// A shutdown doesn't wait for a stuck write either.
	release = make(chan struct{})
	f.due(t)
	stopping, stop := context.WithCancel(ctx)
	time.AfterFunc(20*time.Millisecond, stop)
	short := copyStall
	copyStall = time.Hour
	within("stopping", func() error { return f.s.CopyDue(stopping) })
	copyStall = short
	close(release)
	for deadline := time.Now().Add(5 * time.Second); stuck(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the abandoned write never finished")
		}
	}
	f.due(t)
	within("copying again", func() error { return f.s.CopyDue(ctx) })
	if state, _, _, _ := f.row(t, backup.ID); state != "complete" {
		t.Fatal(state)
	}
}

// Publishing keeps a valid copy already in place, as an attempt waking after
// a newer one finds, and replaces an invalid one.
func TestPublishNeverReplacesAValidCopy(t *testing.T) {
	f := newCopyFixture(t)
	dir, err := f.s.Storage.pin(f.copies)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.close()
	sum := sha256.Sum256([]byte("world data"))
	checksum := hex.EncodeToString(sum[:])
	final := filepath.Join(f.copies, "copy.tar")
	inode := func() uint64 {
		info, err := os.Stat(final)
		if err != nil {
			t.Fatal(err)
		}
		return info.Sys().(*syscall.Stat_t).Ino
	}
	os.WriteFile(final, []byte("world data"), 0o600)
	kept := inode()
	os.WriteFile(filepath.Join(f.copies, "copy.tar.a.partial"), []byte("world data"), 0o600)
	if _, err := publish(ctx, dir, "copy.tar.a.partial", "copy.tar", 10, checksum, nil); err != nil || inode() != kept {
		t.Fatal("replaced a valid copy", err)
	}
	os.WriteFile(final, []byte("WORLD DATA"), 0o600)
	os.WriteFile(filepath.Join(f.copies, "copy.tar.b.partial"), []byte("world data"), 0o600)
	if _, err := publish(ctx, dir, "copy.tar.b.partial", "copy.tar", 10, checksum, nil); err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(final); string(content) != "world data" {
		t.Fatal("kept an invalid copy", string(content))
	}
	if entries, _ := os.ReadDir(f.copies); len(entries) != 1 {
		t.Fatal("left partial files", entries)
	}
}

// The second folder is a share that has stopped responding: each look at the
// storage summary reports it without waiting, and only one free-space
// reading is ever left waiting on it.
func TestStuckFolderIsMeasuredOnce(t *testing.T) {
	f := newCopyFixture(t)
	previous := measureWait
	measureWait = 50 * time.Millisecond
	release := make(chan struct{})
	var started atomic.Int32
	testHook = func(point string) {
		if point == "measuring" {
			started.Add(1)
			<-release
		}
	}
	t.Cleanup(func() { close(release); testHook = nil; measureWait = previous })
	for range 3 {
		if status := f.s.Status(ctx); status.CopyIssue != errCopyStalled.Message {
			t.Fatal(status.CopyIssue)
		}
	}
	if started.Load() != 1 {
		t.Fatal("readings piled up on a stuck folder", started.Load())
	}
}

// Reading a large copy back to verify it takes longer than the stall limit
// while never stopping: it is progress, so the copy finishes instead of
// being abandoned and retried forever.
func TestSlowVerificationIsNotAStall(t *testing.T) {
	f := newCopyFixture(t)
	backup := f.add(t, strings.Repeat("world data ", 20_000), 1)
	previous := copyStall
	copyStall = 100 * time.Millisecond
	testHook = func(point string) {
		if point == "verifying" {
			time.Sleep(40 * time.Millisecond)
		}
	}
	t.Cleanup(func() { testHook = nil; copyStall = previous })
	if err := f.s.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _, _, problem := f.row(t, backup.ID); state != "complete" {
		t.Fatal(state, problem)
	}
}

// The disk a backup folder was on is lost, and the owner makes the second
// folder the backup folder. Retention leaves those backups listed with their
// copies, since the folder may only be unmounted for now. Deleting one by
// hand asks first, then removes it from Ludock with its second copy,
// leaving whatever is still in the old folder.
func TestBackupsInALostFolder(t *testing.T) {
	f := newCopyFixture(t)
	var list []Backup
	for index := range 5 {
		list = append(list, f.add(t, "world data", int64(index+1)*60_000))
	}
	if err := f.s.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	lost := f.main + "-lost"
	if err := os.Rename(f.main, lost); err != nil {
		t.Fatal(err)
	}
	// An unmounted disk can leave its folder empty, not gone.
	os.Mkdir(f.main, 0o755)
	if err := f.s.Prune(ctx, f.serverID, Settings{Destination: f.copies, CopyChosen: true}, list[4].ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	for _, backup := range list {
		if found, _ := f.s.Exists(ctx, backup.ID, true); !found {
			t.Fatal("retention removed a backup missing from an empty folder", backup.ID)
		}
	}
	os.Remove(f.main)
	f.settings = Settings{Destination: f.copies, CopyChosen: true}
	if _, err := f.s.DB.Exec(`UPDATE settings SET value_json=? WHERE key='backups'`,
		`{"destination":"`+f.copies+`","maxBytes":0,"reserveBytes":0,"copyTo":"","copyChosen":true}`); err != nil {
		t.Fatal(err)
	}
	copyOf := func(backup Backup) string { return filepath.Join(f.copies, backup.ID+".tar") }

	if err := f.s.Prune(ctx, f.serverID, f.settings, list[4].ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.FinishDeletions(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	for _, backup := range list {
		if found, _ := f.s.Exists(ctx, backup.ID, true); !found || !exists(copyOf(backup)) {
			t.Fatal("retention hid or removed a backup it couldn't reach", backup.ID)
		}
	}

	listed, err := f.s.List(ctx, f.serverID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Locate(listed); err != nil {
		t.Fatal(err)
	}
	if !listed[0].FolderAway || !listed[0].FromCopy {
		t.Fatalf("%+v", listed[0])
	}

	if _, err := f.s.Delete(ctx, f.serverID, list[0].ID, false, nil); !apperr.Is(err, "BACKUP_FOLDER_AWAY") {
		t.Fatal("deleted without saying the archive stays behind", err)
	}
	if found, _ := f.s.Exists(ctx, list[0].ID, true); !found {
		t.Fatal("a refused deletion changed the backup")
	}
	left, err := f.s.Delete(ctx, f.serverID, list[0].ID, true, nil)
	if err != nil || !left {
		t.Fatal(left, err)
	}
	if err := f.s.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	if found, _ := f.s.Exists(ctx, list[0].ID, false); found || exists(copyOf(list[0])) {
		t.Fatal("kept the record or the second copy")
	}
	if !exists(filepath.Join(lost, list[0].ID+".tar")) {
		t.Fatal("touched the old folder")
	}
	// A backup whose folder is back is deleted as usual.
	if err := os.Rename(lost, f.main); err != nil {
		t.Fatal(err)
	}
	if left, err := f.s.Delete(ctx, f.serverID, list[1].ID, true, nil); err != nil || left || exists(filepath.Join(f.main, list[1].ID+".tar")) {
		t.Fatal(left, err)
	}
}

// Two of a day's backups lost their archives, as a hand-cleaned folder
// would leave them; one still has its second copy. Retention keeps the
// newest three that can be restored, so the earlier ones stay, and the
// total limit doesn't count the lost archive. The lost one stays listed,
// marked missing.
func TestRetentionCountsReadableBackups(t *testing.T) {
	f := newCopyFixture(t)
	at := func(day, hour int) int64 { return time.Date(2026, 1, day, hour, 0, 0, 0, time.Local).UnixMilli() }
	earlier := f.add(t, "world data", at(9, 12))
	a, b, c := f.add(t, "world data", at(10, 10)), f.add(t, "world data", at(10, 11)), f.add(t, "world data", at(10, 12))
	if err := f.s.CopyDue(ctx); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(f.main, b.ID+".tar"), filepath.Join(f.copies, b.ID+".tar"), filepath.Join(f.main, c.ID+".tar")} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	newest := f.add(t, "world data", at(10, 13))
	// Five rows of 10 bytes, four archives: room for the next is 20 bytes,
	// or 10 counting the lost one.
	settings := f.settings
	settings.MaxBytes = 60
	if err := f.s.Prune(ctx, f.serverID, settings, newest.ID); err != nil {
		t.Fatal(err)
	}
	for _, backup := range []Backup{earlier, a, b, c, newest} {
		if found, _ := f.s.Exists(ctx, backup.ID, true); !found {
			t.Fatal("retention removed a backup that can be restored, or a lost one", backup.ID)
		}
	}
	listed, err := f.s.List(ctx, f.serverID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Locate(listed); err != nil {
		t.Fatal(err)
	}
	for _, backup := range listed {
		if lost := backup.ID == b.ID; backup.Missing != lost || backup.FromCopy != (backup.ID == c.ID) {
			t.Fatalf("%+v", backup)
		}
	}
}

// A backup folder that stops responding is given up on within a few seconds,
// so the Backups tab and a restore holding the server's locks don't wait on
// it. Until the stuck look returns, others fail at once instead of piling up.
func TestStalledBackupFolderIsGivenUp(t *testing.T) {
	f := newCopyFixture(t)
	backup := f.add(t, "world data", 60_000)
	defer func(wait, stall time.Duration) { measureWait, copyStall = wait, stall }(measureWait, copyStall)
	measureWait, copyStall = 50*time.Millisecond, 50*time.Millisecond
	release := make(chan struct{})
	var looks atomic.Int32
	testHook = func(point string) {
		if point == "reaching" && looks.Add(1) == 1 {
			<-release
		}
	}
	defer func() { testHook = nil }()

	listed, _ := f.s.List(ctx, f.serverID)
	started := time.Now()
	if err := f.s.Locate(listed); !errors.Is(err, errFolderStalled) {
		t.Fatal(err)
	}
	if _, err := f.s.ArchiveFolder(&listed[0]); !errors.Is(err, errFolderStalled) || !listed[0].FolderAway {
		t.Fatal("a stuck folder isn't offered for removal", err)
	}
	if _, err := f.s.Delete(ctx, f.serverID, backup.ID, false, nil); !errors.Is(err, errFolderStalled) {
		t.Fatal(err)
	}
	if waited := time.Since(started); waited > time.Second || looks.Load() != 1 {
		t.Fatal("looks waited on a stuck folder", waited, looks.Load())
	}
	close(release)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if folder, err := f.s.ArchiveFolder(&listed[0]); err == nil {
			if folder != f.main {
				t.Fatal(folder)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the folder stayed stuck after its look returned")
		}
	}
	if found, _ := f.s.Exists(ctx, backup.ID, true); !found {
		t.Fatal("a stuck folder changed the backup")
	}

	// A restore's check of the archive gives up when reading stalls, and
	// one that keeps reading is not a stall.
	stall := make(chan struct{})
	testHook = func(point string) {
		if point == "reaching" {
			<-stall
		}
	}
	if _, err := f.s.Validate(&listed[0], f.main, "", ""); !errors.Is(err, errFolderStalled) {
		t.Fatal(err)
	}
	close(stall)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if f.s.Storage.folders.busyIn(f.main, laneLook) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stalled read never returned")
		}
	}
}

// A folder that answers the look before a removal and then freezes holds the
// deletion, and the locks it has, only for copyStall. The removal given up
// on changes nothing when it finally runs: the backup stays, and deleting it
// again works.
func TestStalledRemovalIsGivenUp(t *testing.T) {
	f := newCopyFixture(t)
	backup := f.add(t, "world data", 60_000)
	defer func(stall time.Duration) { copyStall = stall }(copyStall)
	copyStall = 50 * time.Millisecond
	release := make(chan struct{})
	var looks atomic.Int32
	testHook = func(point string) {
		if point == "reaching" && looks.Add(1) == 2 {
			<-release
		}
	}
	defer func() { testHook = nil }()
	allowed := func() error { return nil }
	if _, err := f.s.Delete(ctx, f.serverID, backup.ID, false, allowed); !errors.Is(err, errFolderStalled) {
		t.Fatal(err)
	}
	close(release)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if f.s.Storage.folders.busyIn(f.main, laneLook) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stalled removal never returned")
		}
	}
	if found, _ := f.s.Exists(ctx, backup.ID, true); !found {
		t.Fatal("a removal given up on marked the backup")
	}
	if _, err := os.Stat(filepath.Join(f.main, backup.ID+".tar")); err != nil {
		t.Fatal("a removal given up on removed the archive", err)
	}
	if left, err := f.s.Delete(ctx, f.serverID, backup.ID, false, allowed); err != nil || left {
		t.Fatal(left, err)
	}
	if _, err := os.Stat(filepath.Join(f.main, backup.ID+".tar")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// A folder that answers a look and then freezes while its archive is read
// fails the read after readStall instead of holding it forever, as for a
// restore that already stopped the server, a browse, or a download; an
// archive that keeps answering reads to its end.
func TestStalledArchiveReadIsGivenUp(t *testing.T) {
	f := newCopyFixture(t)
	backup := f.add(t, "world data", 300_000)
	defer func(stall time.Duration, read func(*os.File, []byte) (int, error)) {
		readStall, readArchive = stall, read
	}(readStall, readArchive)
	readStall = 50 * time.Millisecond
	release := make(chan struct{})
	var reads atomic.Int32
	readArchive = func(file *os.File, p []byte) (int, error) {
		if reads.Add(1) == 2 {
			<-release
		}
		return file.Read(p)
	}
	archive, err := f.s.Storage.Open(f.main, backup.ID)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := io.Copy(io.Discard, struct{ io.Reader }{archive}); !errors.Is(err, errFolderStalled) {
		t.Fatal(err)
	}
	if _, err := archive.Read(make([]byte, 1)); !errors.Is(err, errFolderStalled) {
		t.Fatal(err)
	}
	archive.Close()
	if waited := time.Since(started); waited > 5*time.Second {
		t.Fatal("a stuck read held its reader", waited)
	}
	if f.s.Storage.folders.busyIn(f.main, laneArchive) != 1 {
		t.Fatal("the folder guard doesn't record the stuck read")
	}
	close(release)
	select {
	case <-archive.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the stuck read never let go of the file")
	}
	if f.s.Storage.folders.busyIn(f.main, laneArchive) != 0 {
		t.Fatal("the stuck read stayed recorded after it returned")
	}
	whole, err := f.s.Storage.Open(f.main, backup.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer whole.Close()
	info, _ := os.Stat(filepath.Join(f.main, backup.ID+".tar"))
	if n, err := io.Copy(io.Discard, struct{ io.Reader }{whole}); err != nil || n != info.Size() || n == 0 {
		t.Fatal(n, err)
	}
}

// busyIn reports the claims and abandoned calls in a folder's lane.
func (g *folderGuard) busyIn(folder string, l lane) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if counts := g.busy[folder]; counts != nil {
		return counts[l]
	}
	return 0
}

// The backup folder, the second folder, or Docker stops answering while an
// administrator opens Settings or saves it: the storage summary says which
// within a few seconds, and the save fails with that reason, changing
// nothing.
func TestSettingsGiveUpOnStuckFoldersAndDocker(t *testing.T) {
	defer func(measure, save time.Duration) { measureWait, saveWait = measure, save }(measureWait, saveWait)
	measureWait, saveWait = 200*time.Millisecond, 200*time.Millisecond
	for _, c := range []struct {
		name string
		// stuck is which look into a folder never returns, 0 for none.
		stuck  int32
		docker bool
		status string
		save   error
	}{
		{"backup folder", 1, false, errFolderStalled.Message, errFolderStalled},
		{"second folder", 2, false, "", errCopyStalled},
		{"Docker", 0, true, errDockerStalled.Message, errDockerStalled},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newCopyFixture(t)
			settings := Settings{Destination: f.main, CopyTo: f.other}
			release := make(chan struct{})
			var looks atomic.Int32
			testHook = func(point string) {
				if point == "reaching" && looks.Add(1) == c.stuck {
					<-release
				}
			}
			t.Cleanup(func() {
				close(release)
				// Calls given up on read the hook until they return.
				for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
					f.s.Storage.folders.mu.Lock()
					idle := len(f.s.Storage.folders.busy) == 0
					f.s.Storage.folders.mu.Unlock()
					if idle {
						break
					}
				}
				testHook = nil
			})
			if c.docker {
				client := docker.New(silentSocket(t))
				f.s.Docker, f.s.Storage.Docker = client, client
			}
			started := time.Now()
			if c.status != "" {
				status := f.s.Status(ctx)
				if len(status.Issues) != 1 || status.Issues[0].Message != c.status {
					t.Fatalf("%+v", status.Issues)
				}
				looks.Store(0)
			}
			before, _, _ := f.s.SavedSettings(ctx)
			if _, err := f.s.SaveSettings(ctx, settings, &db.SessionUser{ID: "admin", Role: db.RoleAdmin}, "token"); !errors.Is(err, c.save) {
				t.Fatal(err)
			}
			if after, _, _ := f.s.SavedSettings(ctx); *after != *before {
				t.Fatal("changed settings:", after)
			}
			if waited := time.Since(started); waited > 2*time.Second {
				t.Fatal("waited", waited)
			}
		})
	}
}

// silentSocket is a Docker socket that accepts connections and never answers.
func silentSocket(t *testing.T) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "silent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		listener.Close()
		<-done
	})
	go func() {
		defer close(done)
		var held []net.Conn
		defer func() {
			for _, conn := range held {
				conn.Close()
			}
		}()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			held = append(held, conn)
		}
	}()
	return socket
}
