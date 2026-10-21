package locks

import (
	"errors"
	"testing"
)

func TestPathsConflictWithParentsAndChildren(t *testing.T) {
	table := New()
	release, err := table.Acquire([]string{"server:a", "path:/srv/games/mc/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, keys := range [][]string{{"path:/srv/games"}, {"path:/srv/games/mc/world"}, {"path:/srv/games/x/../mc"}, {"path:/"}, {"server:a"}} {
		if _, err := table.Acquire(keys, nil); !errors.Is(err, ErrConflict) {
			t.Errorf("%v: %v", keys, err)
		}
	}
	if other, err := table.Acquire([]string{"path:/srv/games/mcx", "server:b"}, nil); err != nil {
		t.Fatal(err)
	} else {
		other()
	}
	if !table.ServerBusy("a") {
		t.Fatal("server not busy")
	}
	next := table.NextRelease()
	drained := table.Drained()
	release()
	release()
	<-next
	<-drained
}

func TestBackgroundHoldersCanBeCancelled(t *testing.T) {
	table := New()
	cancelled := false
	release, _ := table.Acquire([]string{"server:a"}, func() { cancelled = true })
	_, err := table.Acquire([]string{"server:a"}, nil)
	var background *BackgroundConflict
	if !errors.As(err, &background) || !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	background.Cancel()
	if !cancelled {
		t.Fatal("not cancelled")
	}
	release()
	foreground, _ := table.Acquire([]string{"server:a"}, nil)
	if _, err := table.Acquire([]string{"server:a"}, func() {}); errors.As(err, &background) {
		t.Fatal("foreground holder reported as background")
	}
	foreground()
}

func TestFenceHoldsBackAllButRecovery(t *testing.T) {
	table := New()
	lift := table.Fence()
	if _, err := table.Acquire([]string{"server:a"}, nil); !errors.Is(err, ErrRecovering) {
		t.Fatal(err)
	}
	release, err := table.AcquireForRecovery([]string{"server:a"})
	if err != nil {
		t.Fatal(err)
	}
	release()
	lift()
	lift()
	if release, err := table.Acquire([]string{"server:a"}, nil); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
}
