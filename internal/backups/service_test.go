package backups

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
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
	s := &Service{DB: d, Storage: &Storage{Roots: destination}, Locks: locks.New()}
	backup := Backup{ID: "6f9619ff-8b86-4011-b42d-00c04fc964ff", ServerID: list[0].ID, CreatedAt: 1}
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
	err := s.Delete(ctx, backup.ServerID, backup.ID, func() error {
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
	err := s.Delete(ctx, backup.ServerID, backup.ID, func() error {
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
	s := &Service{DB: d, Storage: &Storage{Roots: t.TempDir()}, Locks: locks.New()}
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
			`{"destination":"`+s.Storage.Roots+`","retentionCount":3,"maxBytes":1073741824,"reserveBytes":0}`); err != nil {
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
		if _, err := s.DB.Exec(`INSERT INTO backups(id,server_id,binding_fingerprint,destination,roots_json,size,checksum,created_at,state)
      VALUES(?,?,'x',?,'[]',0,'',?,'deleting')`, id, backup.ServerID, gone, i); err != nil {
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
	if err := s.AssertNoOtherWriters(ctx, server, ""); err != nil {
		t.Fatal(err)
	}
	engine.AddContainer(fakedocker.GameOptions{Name: "writer", Image: "alpine", Running: true,
		Mounts: []docker.MountPoint{{Type: "bind", Source: filepath.Join(data, "world"), Destination: "/world", RW: true}}})
	if err := s.AssertNoOtherWriters(ctx, server, ""); !apperr.Is(err, "SHARED_DATA_WRITER") {
		t.Fatal(err)
	}
}
