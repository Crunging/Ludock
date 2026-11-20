package backups

import (
	"archive/tar"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/servers"
)

// dataFiles lists the saves in folder, oldest first.
func dataFiles(t *testing.T, folder string) []string {
	t.Helper()
	entries, err := os.ReadDir(folder)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "ludock-data-") {
			names = append(names, entry.Name())
		}
	}
	return names
}

// forceDue makes the next save, or its copy, due now.
func (f *copyFixture) forceDue(t *testing.T, key string) {
	t.Helper()
	folder := f.main
	if key == dataCopyKey {
		folder = f.copies
	}
	track, err := f.s.dataTrack(ctx, key, folder)
	if err != nil {
		t.Fatal(err)
	}
	track.At, track.NextAt, track.Name = 0, 0, track.Name+"-old"
	if err := f.s.DB.SetSetting(ctx, nil, key, track); err != nil {
		t.Fatal(err)
	}
}

// Saving writes one owner-only tar of the database and key into the backup
// folder, and it isn't a game backup: no record, no space in their totals.
// The next save waits for the day to pass or for a change worth keeping.
func TestSaveDataWritesOwnerOnlyArchive(t *testing.T) {
	f := newCopyFixture(t)
	if err := f.s.SaveData(ctx); err != nil {
		t.Fatal(err)
	}
	names := dataFiles(t, f.main)
	if len(names) != 1 || !dataName.MatchString(names[0]) {
		t.Fatal("saved", names)
	}
	info, err := os.Stat(filepath.Join(f.main, names[0]))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("save isn't owner-only", info.Mode(), err)
	}
	file, _ := os.Open(filepath.Join(f.main, names[0]))
	defer file.Close()
	var entries []string
	for reader := tar.NewReader(file); ; {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, header.Name)
	}
	if !slices.Equal(entries, []string{"ludock.db", "ludock.db.identity-key/", "ludock.db.identity-key/key"}) {
		t.Fatal("archive holds", entries)
	}
	var rows int
	f.s.DB.QueryRow("SELECT COUNT(*) FROM backups").Scan(&rows)
	if used, _, _ := f.s.ArchiveBytes(ctx); rows != 0 || used != 0 {
		t.Fatal("the save counts as a game backup", rows, used)
	}
	status := f.s.Status(ctx)
	if status.DataSavedAt == 0 || status.DataProblem != "" {
		t.Fatal("status", status.DataSavedAt, status.DataProblem)
	}

	// Not due again yet, even after a change, until dataSoonest has passed.
	f.add(t, "world", time.Now().UnixMilli())
	if err := f.s.SaveData(ctx); err != nil || len(dataFiles(t, f.main)) != 1 {
		t.Fatal("saved again at once", err)
	}
	track, _ := f.s.dataTrack(ctx, dataSaveKey, f.main)
	track.At -= dataSoonest.Milliseconds()
	f.s.DB.SetSetting(ctx, nil, dataSaveKey, track)
	if due, err := f.s.dataDue(ctx, track, f.main, time.Now()); err != nil || !due {
		t.Fatal("a new game backup doesn't make a save due", err)
	}
	f.s.DB.Exec("DELETE FROM backups")
	if due, err := f.s.dataDue(ctx, track, f.main, time.Now()); err != nil || due {
		t.Fatal("a save is due with nothing changed", err)
	}
}

// Each folder keeps the newest three saves and the newest of each of the
// last seven days.
func TestDataKeeps(t *testing.T) {
	var names []string
	start := time.Date(2026, 11, 20, 23, 0, 0, 0, time.UTC)
	for day := range 10 {
		for hour := range 3 {
			names = append(names, dataFileName(start.Add(-time.Duration(day)*24*time.Hour-time.Duration(hour)*time.Hour)))
		}
	}
	keep := dataKeeps(names, time.UTC)
	var kept []string
	for _, name := range names {
		if keep[name] {
			kept = append(kept, name)
		}
	}
	want := []string{"ludock-data-20261120-230000.tar", "ludock-data-20261120-220000.tar", "ludock-data-20261120-210000.tar",
		"ludock-data-20261119-230000.tar", "ludock-data-20261118-230000.tar", "ludock-data-20261117-230000.tar",
		"ludock-data-20261116-230000.tar", "ludock-data-20261115-230000.tar", "ludock-data-20261114-230000.tar"}
	if !slices.Equal(kept, want) {
		t.Fatalf("kept %v", kept)
	}
}

// A save interrupted by a restart left a partial file in the backup folder
// and a work folder beside the database. Neither looks like a save; the next
// save removes both. A save stopped by shutdown records no failure.
func TestSaveDataInterrupted(t *testing.T) {
	f := newCopyFixture(t)
	partial := filepath.Join(f.main, "ludock-data-20261119-010203.tar.1a2b3c4d.partial")
	os.WriteFile(partial, []byte("half"), 0o600)
	var seq int
	var name, database string
	f.s.DB.QueryRow("PRAGMA database_list").Scan(&seq, &name, &database)
	work := filepath.Join(filepath.Dir(database), ".ludock-save-0123456789abcdef")
	os.Mkdir(work, 0o700)
	os.WriteFile(filepath.Join(work, "copy.db"), []byte("half"), 0o600)
	if names, _ := f.s.Storage.dataSaves(f.main); len(names) != 0 {
		t.Fatal("a partial file counts as a save", names)
	}

	stopped, cancel := context.WithCancel(ctx)
	testHook = func(point string) {
		if point == "saving" {
			cancel()
		}
	}
	t.Cleanup(func() { testHook = nil })
	if err := f.s.SaveData(stopped); err != nil {
		t.Fatal(err)
	}
	// The stopped attempt's I/O ends on its own goroutine.
	for !f.s.copies.enter(f.main) {
		time.Sleep(time.Millisecond)
	}
	f.s.copies.leave(f.main)
	if track, _ := f.s.dataTrack(ctx, dataSaveKey, f.main); track.Failures != 0 || track.At != 0 {
		t.Fatal("shutdown recorded as a save or a failure", track)
	}
	for _, name := range dataFiles(t, f.main) {
		if dataName.MatchString(name) {
			t.Fatal("a stopped save published", name)
		}
	}

	if err := f.s.SaveData(ctx); err != nil {
		t.Fatal(err)
	}
	names := dataFiles(t, f.main)
	if len(names) != 1 || !dataName.MatchString(names[0]) || exists(work) {
		t.Fatal("after the interrupted save", names, exists(work))
	}
}

// A missing backup folder fails saving quietly: each attempt records why
// and waits, one alert goes out after several, and game backups' readiness
// doesn't change. The next save after the folder is back succeeds.
func TestSaveDataFailingFolder(t *testing.T) {
	f := newCopyFixture(t)
	var alerts []string
	f.s.DataFailed = func(_ context.Context, _ *sql.Tx, key, message string) error {
		alerts = append(alerts, key+" "+message)
		return nil
	}
	os.Remove(f.main)
	for attempt := 1; attempt <= copyAttemptsBeforeAlert+1; attempt++ {
		if err := f.s.SaveData(ctx); err != nil {
			t.Fatal(err)
		}
		track, _ := f.s.dataTrack(ctx, dataSaveKey, f.main)
		if track.Failures != attempt || track.NextAt <= time.Now().UnixMilli() || !strings.Contains(track.Problem, "missing") {
			t.Fatal(attempt, track)
		}
		// Waiting out the backoff.
		if err := f.s.SaveData(ctx); err != nil {
			t.Fatal(err)
		}
		if again, _ := f.s.dataTrack(ctx, dataSaveKey, f.main); again.Failures != attempt {
			t.Fatal("retried before its backoff", again)
		}
		track.NextAt = 0
		f.s.DB.SetSetting(ctx, nil, dataSaveKey, track)
	}
	if len(alerts) != 1 || !strings.HasPrefix(alerts[0], "data-save-failed:") || !strings.Contains(alerts[0], "own data") {
		t.Fatal("alerts", alerts)
	}
	status := f.s.Status(ctx)
	if !strings.Contains(status.DataProblem, "couldn’t save its own data") {
		t.Fatal("status", status.DataProblem)
	}
	os.Mkdir(f.main, 0o755)
	status = f.s.Status(ctx)
	if len(status.Issues) != 0 {
		t.Fatal("saving Ludock's data affects game backups", status.Issues)
	}
	if err := f.s.SaveData(ctx); err != nil {
		t.Fatal(err)
	}
	if track, _ := f.s.dataTrack(ctx, dataSaveKey, f.main); track.Failures != 0 || track.Alerted || len(dataFiles(t, f.main)) != 1 {
		t.Fatal("after the folder came back", track)
	}
}

// A full backup folder stops at the free-space reserve, leaving nothing.
func TestSaveDataKeepsTheReserve(t *testing.T) {
	f := newCopyFixture(t)
	f.s.DB.Exec(`UPDATE settings SET value_json=json_set(value_json,'$.reserveBytes',?) WHERE key='backups'`, int64(maxSetting))
	if err := f.s.SaveData(ctx); err != nil {
		t.Fatal(err)
	}
	track, _ := f.s.dataTrack(ctx, dataSaveKey, f.main)
	if track.Failures != 1 || !strings.Contains(track.Problem, "reserve") || len(dataFiles(t, f.main)) != 0 {
		t.Fatal(track, dataFiles(t, f.main))
	}
}

// The second folder gets each save, verified, and keeps the same saves the
// backup folder does. With the backup folder lost, the copies stay.
func TestCopyDataFollowsSaves(t *testing.T) {
	f := newCopyFixture(t)
	old := time.Now().Add(-30 * 24 * time.Hour)
	for index := range 4 {
		name := dataFileName(old.Add(time.Duration(index) * time.Minute))
		for _, folder := range []string{f.main, f.copies} {
			os.WriteFile(filepath.Join(folder, name), []byte("old"), 0o600)
		}
	}
	for range 2 {
		if err := f.s.SaveData(ctx); err != nil {
			t.Fatal(err)
		}
		if err := f.s.CopyData(ctx); err != nil {
			t.Fatal(err)
		}
		f.forceDue(t, dataSaveKey)
		time.Sleep(1100 * time.Millisecond)
	}
	main, copies := dataFiles(t, f.main), dataFiles(t, f.copies)
	if !slices.Equal(main, copies) || len(main) != dataKeepNewest || main[0] != dataFileName(old.Add(3*time.Minute)) {
		t.Fatalf("backup folder has %v, second folder %v", main, copies)
	}
	newest := main[len(main)-1]
	original, _ := os.ReadFile(filepath.Join(f.main, newest))
	copied, _ := os.ReadFile(filepath.Join(f.copies, newest))
	if string(original) != string(copied) {
		t.Fatal("the copy differs from the save")
	}
	if track, _ := f.s.dataTrack(ctx, dataCopyKey, f.copies); track.Name != newest || track.Folder != f.copies {
		t.Fatal("copy track", track)
	}

	// The backup folder is lost: saving and copying fail, and the second
	// folder keeps every save it had.
	os.RemoveAll(f.main)
	os.Mkdir(f.main, 0o755)
	if err := f.s.CopyData(ctx); err != nil {
		t.Fatal(err)
	}
	f.forceDue(t, dataCopyKey)
	if err := f.s.CopyData(ctx); err != nil {
		t.Fatal(err)
	}
	if after := dataFiles(t, f.copies); !slices.Equal(after, copies) {
		t.Fatal("losing the backup folder removed copies", after)
	}
	if track, _ := f.s.dataTrack(ctx, dataCopyKey, f.copies); track.Failures != 1 || track.Problem != errCopySource.Message {
		t.Fatal(track)
	}
	if status := f.s.Status(ctx); !strings.Contains(status.DataProblem, "second copy") {
		t.Fatal("status", status.DataProblem)
	}
}

// A backup folder that a game server mounts, even read-only, would let it and
// its file helpers read the save, so nothing is written there.
func TestSaveDataRefusesGameData(t *testing.T) {
	f := newCopyFixture(t)
	f.engine.AddContainer(fakedocker.GameOptions{ID: "mc", Name: "mc", Image: "itzg/minecraft-server", Running: true,
		Mounts: []docker.MountPoint{{Type: "bind", Source: f.main, Destination: "/data", RW: false}}})
	f.s.Observer = &servers.Observer{Docker: f.s.Docker}
	if err := f.s.SaveData(ctx); err != nil {
		t.Fatal(err)
	}
	track, _ := f.s.dataTrack(ctx, dataSaveKey, f.main)
	if track.Failures != 1 || !strings.Contains(track.Problem, "overlaps game data") || len(dataFiles(t, f.main)) != 0 {
		t.Fatal(track, dataFiles(t, f.main))
	}
}

// A save damaged in the backup folder after it was verified isn't copied,
// and the second folder keeps the copies it has.
func TestCopyDataChecksTheSnapshot(t *testing.T) {
	f := newCopyFixture(t)
	if err := f.s.SaveData(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CopyData(ctx); err != nil {
		t.Fatal(err)
	}
	first := dataFiles(t, f.copies)
	time.Sleep(1100 * time.Millisecond)
	f.forceDue(t, dataSaveKey)
	if err := f.s.SaveData(ctx); err != nil {
		t.Fatal(err)
	}
	saved, _ := f.s.dataTrack(ctx, dataSaveKey, f.main)
	os.WriteFile(filepath.Join(f.main, saved.Name), []byte("damaged"), 0o600)
	if err := f.s.CopyData(ctx); err != nil {
		t.Fatal(err)
	}
	if track, _ := f.s.dataTrack(ctx, dataCopyKey, f.copies); track.Failures != 1 || track.Problem != errCopySource.Message {
		t.Fatal(track)
	}
	if after := dataFiles(t, f.copies); !slices.Equal(after, first) {
		t.Fatal("second folder after a damaged save", after)
	}
}

// Failures in one backup folder don't delay the first save in the folder
// chosen instead.
func TestSaveDataNewFolderSavesAtOnce(t *testing.T) {
	f := newCopyFixture(t)
	os.Remove(f.main)
	if err := f.s.SaveData(ctx); err != nil {
		t.Fatal(err)
	}
	f.s.DB.Exec(`UPDATE settings SET value_json=json_set(value_json,'$.destination',?,'$.copyTo','') WHERE key='backups'`, f.other)
	if err := f.s.SaveData(ctx); err != nil {
		t.Fatal(err)
	}
	if len(dataFiles(t, f.other)) != 1 {
		t.Fatal("the new folder waited for the old one's retry")
	}
	if status := f.s.Status(ctx); status.DataSavedAt == 0 || status.DataProblem != "" {
		t.Fatal("status", status.DataSavedAt, status.DataProblem)
	}
}
