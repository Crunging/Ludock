package backups

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/self"
	"github.com/Crunging/Ludock/internal/servers"
)

var forbidden = apperr.New("FORBIDDEN", 403, "Forbidden")

// recorded saves one complete backup with its archive.
func recorded(t *testing.T) (*Service, Backup, string) {
	t.Helper()
	d := dbtest.Open(t)
	store := &identity.Store{DB: d}
	list, err := store.Reconcile(ctx, []identity.Observation{{ContainerID: "mc", Name: "mc", DisplayName: "Minecraft", GameType: "minecraft"}}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	s := &Service{DB: d, Storage: &Storage{Roots: destination, Own: self.New(nil, "")}, Locks: locks.New()}
	backup := Backup{ID: "6f9619ff-8b86-4011-b42d-00c04fc964ff", ServerID: list[0].ID, CreatedAt: 1, Size: int64(len("archive"))}
	archive := filepath.Join(destination, backup.ID+".tar")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, backup, list[0].BindingFingerprint, destination); err != nil {
		t.Fatal(err)
	}
	return s, backup, archive
}

func kept(t *testing.T, s *Service, backup Backup, archive string) {
	t.Helper()
	if err := s.FinishDeletions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatal("removed a backup whose deletion was refused", err)
	}
	if exists, _ := s.Exists(ctx, backup.ID, true); !exists {
		t.Fatal("backup no longer offered")
	}
}

// Background deletion keeps a backup whose deletion was refused.
func TestRefusedDeletionSurvivesCleanup(t *testing.T) {
	s, backup, archive := recorded(t)
	// Background cleanup lists backups while a Delete is under way, then
	// waits for the locks until that Delete is refused.
	listed, proceed, cleaned := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	testHook = func(point string) {
		if point == "pending" {
			close(listed)
			<-proceed
		}
	}
	t.Cleanup(func() { testHook = nil })
	checks := 0
	_, err := s.Delete(ctx, backup.ServerID, backup.ID, false, func() error {
		if checks++; checks > 1 {
			go func() { cleaned <- s.FinishDeletions(ctx) }()
			<-listed
			return forbidden
		}
		return nil
	})
	if !apperr.Is(err, "FORBIDDEN") {
		t.Fatal(err)
	}
	close(proceed)
	if err := <-cleaned; err != nil {
		t.Fatal(err)
	}
	testHook = nil
	kept(t, s, backup, archive)
}

// A refused deletion needs no write to keep the backup.
func TestRefusedDeletionWritesNothing(t *testing.T) {
	s, backup, archive := recorded(t)
	// Any write that would put the backup back fails, as with a full disk.
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_restore BEFORE UPDATE OF state ON backups WHEN NEW.state='complete'
		BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	checks := 0
	_, err := s.Delete(ctx, backup.ServerID, backup.ID, false, func() error {
		if checks++; checks > 1 {
			return forbidden
		}
		return nil
	})
	if !apperr.Is(err, "FORBIDDEN") || checks != 2 {
		t.Fatal(err, checks)
	}
	kept(t, s, backup, archive)
}

func TestABackupWaitsForStorageSetupUnderWay(t *testing.T) {
	d := dbtest.Open(t)
	s := &Service{DB: d, Storage: &Storage{Roots: t.TempDir(), Own: self.New(nil, "")}, Locks: locks.New()}
	// Background setup is saving the default settings when a backup that
	// is due at startup looks for them.
	entered, saved := make(chan struct{}), make(chan struct{})
	testHook = func(point string) {
		if point != "initializing" {
			return
		}
		testHook = nil
		close(entered)
		time.Sleep(100 * time.Millisecond)
		if _, err := d.Exec(`INSERT INTO settings(key,value_json) VALUES('backups',?)`,
			`{"destination":"`+s.Storage.Roots+`","maxBytes":0,"reserveBytes":0}`); err != nil {
			t.Error(err)
		}
		close(saved)
	}
	t.Cleanup(func() { testHook = nil })
	go s.Initialize(ctx)
	<-entered
	_, err := s.ForBackup(ctx)
	<-saved
	// The saved settings are then validated; this test daemon has no
	// container for Ludock, so that is where it stops.
	if !apperr.Is(err, "BACKUP_MOUNT_UNVERIFIED") {
		t.Fatal("did not wait for setup under way:", err)
	}
}

func TestFailingDeletionsDoNotBlockOthers(t *testing.T) {
	s, backup, archive := recorded(t)
	previous := deletionBatch
	deletionBatch = 2
	t.Cleanup(func() { deletionBatch = previous })
	// The oldest deleting rows name a destination that no longer exists.
	gone := filepath.Join(t.TempDir(), "gone")
	for i, id := range []string{"1f9619ff-8b86-4011-b42d-00c04fc964ff", "2f9619ff-8b86-4011-b42d-00c04fc964ff", "3f9619ff-8b86-4011-b42d-00c04fc964ff"} {
		if _, err := s.DB.Exec(`INSERT INTO backups(id,server_id,binding_fingerprint,destination,roots_json,size,checksum,created_at,state,game_type)
      VALUES(?,?,'x',?,'[]',0,'',?,'deleting','')`, id, backup.ServerID, gone, i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB.Exec("UPDATE backups SET state='deleting',created_at=10 WHERE id=?", backup.ID); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		s.FinishDeletions(ctx)
	}
	if _, err := os.Stat(archive); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a removable archive stayed behind failing deletions", err)
	}
	if exists, _ := s.Exists(ctx, backup.ID, false); exists {
		t.Fatal("its row stayed")
	}
}

// Among many running containers, one that can write the server's data
// refuses the backup; containers that only read it or are stopped do not.
// On a volume, another subfolder doesn't conflict; the whole volume, a
// folder nested with the server's, or one inherited with --volumes-from
// does.
func TestAssertNoOtherWriters(t *testing.T) {
	engine, client := startHelpers(t)
	data := t.TempDir()
	server := serverContext(data)
	server.Container.ID = engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true,
		Mounts: []docker.MountPoint{{Type: "bind", Source: data, Destination: "/data", RW: true}}})
	for index := range 9 {
		engine.AddContainer(fakedocker.GameOptions{Name: "other-" + strconv.Itoa(index), Image: "alpine", Running: true,
			Mounts: []docker.MountPoint{{Type: "bind", Source: t.TempDir(), Destination: "/data", RW: true}}})
	}
	engine.AddContainer(fakedocker.GameOptions{Name: "reader", Image: "alpine", Running: true,
		Mounts: []docker.MountPoint{{Type: "bind", Source: data, Destination: "/data", RW: false}}})
	engine.AddContainer(fakedocker.GameOptions{Name: "stopped", Image: "alpine",
		Mounts: []docker.MountPoint{{Type: "bind", Source: data, Destination: "/data", RW: true}}})
	s := &Service{Docker: client}
	before := len(engine.Requests)
	if err := s.AssertNoOtherWriters(ctx, server, ""); err != nil {
		t.Fatal(err)
	}
	// The check runs every second of a backup: one list, no inspections.
	for _, request := range engine.Requests[before:] {
		if !strings.Contains(request, "/containers/json") && !strings.Contains(request, "/version") {
			t.Fatal("inspected a container", request)
		}
	}
	engine.AddContainer(fakedocker.GameOptions{Name: "writer", Image: "alpine", Running: true,
		Mounts: []docker.MountPoint{{Type: "bind", Source: filepath.Join(data, "world"), Destination: "/world", RW: true}}})
	if err := s.AssertNoOtherWriters(ctx, server, ""); !apperr.Is(err, "SHARED_DATA_WRITER") {
		t.Fatal(err)
	}

	volume := "/var/lib/docker/volumes/worlds/_data"
	onVolume := func(name, subpath string, volumesFrom bool) string {
		id := engine.AddContainer(fakedocker.GameOptions{Name: name, Image: "alpine", Running: true,
			Mounts: []docker.MountPoint{{Type: "volume", Name: "worlds", Source: volume, Destination: "/data", RW: true}}})
		engine.Update(id, func(c *fakedocker.Container) {
			if volumesFrom {
				c.HostConfig = map[string]any{"VolumesFrom": []any{"seed"}}
			} else if subpath != "" {
				c.HostConfig = map[string]any{"Mounts": []any{map[string]any{"Type": "volume", "Source": "worlds", "Target": "/data",
					"VolumeOptions": map[string]any{"Subpath": subpath}}}}
			}
		})
		return id
	}
	game := &servers.Context{Observation: identity.Observation{Mounts: []identity.Mount{
		{Type: "volume", Name: "worlds", Source: volume, Destination: "/data", Writable: true, Subpath: "a"}}}}
	game.Container.ID = onVolume("game-a", "a", false)
	sibling := onVolume("game-b", "b", false)
	if err := s.AssertNoOtherWriters(ctx, game, ""); err != nil {
		t.Fatal("a game on another subfolder of the volume conflicts", err)
	}
	for index, writer := range []struct {
		subpath     string
		volumesFrom bool
	}{{"", false}, {"a/world", false}, {"", true}} {
		id := onVolume("volume-writer-"+strconv.Itoa(index), writer.subpath, writer.volumesFrom)
		if err := s.AssertNoOtherWriters(ctx, game, ""); !apperr.Is(err, "SHARED_DATA_WRITER") {
			t.Fatal(writer, err)
		}
		engine.Update(id, func(c *fakedocker.Container) { c.State.Status, c.State.Running = "exited", false })
	}
	// The whole volume conflicts with every folder of it.
	game.Observation.Mounts[0].Subpath = ""
	if err := s.AssertNoOtherWriters(ctx, game, sibling); err != nil {
		t.Fatal(err)
	}
	if err := s.AssertNoOtherWriters(ctx, game, ""); !apperr.Is(err, "SHARED_DATA_WRITER") {
		t.Fatal(err)
	}
}

// Retention leaves alone a backup an administrator keeps, even one kept
// after Prune listed it, and an archive open for a download or browse until
// it is closed. Deleting a kept backup is refused.
func TestPruneSparesKeptAndOpenArchives(t *testing.T) {
	s, first, _ := recorded(t)
	destination := s.Storage.Roots
	var fingerprint string
	s.DB.QueryRow("SELECT binding_fingerprint FROM backups WHERE id=?", first.ID).Scan(&fingerprint)
	ids := []string{first.ID}
	for index := 1; index < 7; index++ {
		backup := Backup{ID: identity.NewUUID(), ServerID: first.ServerID, CreatedAt: int64(index) * 60_000, State: "complete", Size: first.Size}
		if err := os.WriteFile(filepath.Join(destination, backup.ID+".tar"), []byte("archive"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.Record(ctx, backup, fingerprint, destination); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, backup.ID)
	}
	// ids is oldest first: keep the oldest, read the second.
	s.DB.Exec("UPDATE backups SET kept_at=1, note='before 1.21' WHERE id=?", ids[0])
	if _, err := s.Delete(ctx, first.ServerID, ids[0], false, nil); !apperr.Is(err, "BACKUP_KEPT") {
		t.Fatal("deleted a kept backup", err)
	}
	archive, err := s.Storage.Open(destination, ids[1])
	if err != nil {
		t.Fatal(err)
	}
	settings := Settings{Destination: destination}
	if err := s.Prune(ctx, first.ServerID, settings, ids[6]); err != nil {
		t.Fatal(err)
	}
	present := func() []string {
		var left []string
		for _, id := range ids {
			_, statErr := os.Stat(filepath.Join(destination, id+".tar"))
			if exists, _ := s.Exists(ctx, id, true); exists != (statErr == nil) {
				t.Fatal("record and archive disagree for", id)
			} else if exists {
				left = append(left, id)
			}
		}
		return left
	}
	if left := present(); !slices.Equal(left, []string{ids[0], ids[1], ids[4], ids[5], ids[6]}) {
		t.Fatal(left)
	}
	archive.Close()
	if err := s.Prune(ctx, first.ServerID, settings, ids[6]); err != nil {
		t.Fatal(err)
	}
	if left := present(); !slices.Equal(left, []string{ids[0], ids[4], ids[5], ids[6]}) {
		t.Fatal(left)
	}
	// A backup kept after retention chose it is not marked for deletion.
	s.DB.Exec("UPDATE backups SET kept_at=1 WHERE id=?", ids[4])
	backup, _ := s.Get(ctx, first.ServerID, ids[4])
	backup.KeptAt = 0
	if err := s.remove(ctx, backup, nil, false); !errors.Is(err, errKept) {
		t.Fatal(err)
	}
	if left := present(); !slices.Equal(left, []string{ids[0], ids[4], ids[5], ids[6]}) {
		t.Fatal(left)
	}
}

// Space pressure removes the oldest backups only until the next one fits,
// measuring again after each removal, and skips archives left in an earlier
// backup folder, which free nothing there. While that folder can't be
// reached, its archive doesn't count toward the limit, so nothing goes.
func TestPruneMakesRoomForTheNextBackup(t *testing.T) {
	s, first, _ := recorded(t)
	destination := s.Storage.Roots
	var fingerprint string
	s.DB.QueryRow("SELECT binding_fingerprint FROM backups WHERE id=?", first.ID).Scan(&fingerprint)
	s.DB.Exec("UPDATE backups SET size=100, destination='/earlier' WHERE id=?", first.ID)
	archive := make([]byte, 100)
	os.WriteFile(filepath.Join(destination, first.ID+".tar"), archive, 0o600)
	ids := []string{first.ID}
	for day := 1; day < 6; day++ {
		backup := Backup{ID: identity.NewUUID(), ServerID: first.ServerID, Size: 100, CreatedAt: int64(day) * 86_400_000, State: "complete"}
		if err := os.WriteFile(filepath.Join(destination, backup.ID+".tar"), archive, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.Record(ctx, backup, fingerprint, destination); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, backup.ID)
	}
	// Six daily backups of 100 bytes leave 50 under the limit; the next
	// needs 110.
	prune := func(want ...string) {
		t.Helper()
		if err := s.Prune(ctx, first.ServerID, Settings{Destination: destination, MaxBytes: 650}, ids[5]); err != nil {
			t.Fatal(err)
		}
		var left []string
		for _, id := range ids {
			if exists, _ := s.Exists(ctx, id, true); exists {
				left = append(left, id)
			}
		}
		if !slices.Equal(left, want) {
			t.Fatal(left)
		}
	}
	prune(ids...)
	earlier := filepath.Join(destination, "earlier")
	os.Mkdir(earlier, 0o755)
	os.Rename(filepath.Join(destination, first.ID+".tar"), filepath.Join(earlier, first.ID+".tar"))
	s.DB.Exec("UPDATE backups SET destination=? WHERE id=?", earlier, first.ID)
	prune(ids[0], ids[2], ids[3], ids[4], ids[5])
}
