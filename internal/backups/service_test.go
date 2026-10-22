package backups

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db/dbtest"
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

func TestBackgroundDeletionKeepsABackupWhoseDeletionWasRefused(t *testing.T) {
	s, backup, archive := recorded(t)
	// Background cleanup lists backups while a Delete is under way, then
	// waits for the locks until that Delete is refused.
	listed, proceed, cleaned := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	testHook = func(point string) {
		close(listed)
		<-proceed
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

func TestARefusedDeletionNeedsNoWriteToKeepTheBackup(t *testing.T) {
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
