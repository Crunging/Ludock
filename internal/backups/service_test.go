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

func TestBackgroundDeletionKeepsABackupWhoseDeletionWasRefused(t *testing.T) {
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
	// Background cleanup lists the backup while a Delete has it marked
	// deleting, then waits for the locks until that Delete is refused.
	listed, proceed, cleaned := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	testHook = func(point string) {
		close(listed)
		<-proceed
	}
	t.Cleanup(func() { testHook = nil })
	err = s.Delete(ctx, backup.ServerID, backup.ID, func() error {
		if exists, _ := s.Exists(ctx, backup.ID, true); !exists {
			go func() { cleaned <- s.FinishDeletions(ctx) }()
			<-listed
			return apperr.New("FORBIDDEN", 403, "Forbidden")
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
	if _, err := os.Stat(archive); err != nil {
		t.Fatal("removed a backup whose deletion was refused", err)
	}
	if exists, _ := s.Exists(ctx, backup.ID, true); !exists {
		t.Fatal("backup no longer offered")
	}
}
