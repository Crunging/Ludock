// Package locks serializes work on servers, Compose projects, volumes, and
// host paths. Path keys conflict with their parents and children.
package locks

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
)

var ErrConflict = apperr.New("OPERATION_CONFLICT", 409, "A conflicting operation is already running")

var ErrRecovering = apperr.New("RECOVERY_PENDING", 409, "Ludock is recovering interrupted operations. Try again shortly.")

// BackgroundConflict reports a conflict only with background work, such as
// a countdown warning. Cancel asks those holders to stop; they keep their
// keys until their cleanup finishes.
type BackgroundConflict struct {
	cancels []func()
}

func (e *BackgroundConflict) Error() string { return ErrConflict.Message }

// Unwrap lets apperr.As see the public error.
func (e *BackgroundConflict) Unwrap() error { return ErrConflict }

func (e *BackgroundConflict) Cancel() {
	for _, cancel := range e.cancels {
		cancel()
	}
}

type holder struct {
	background func() // nil for foreground work
}

type Table struct {
	mu       sync.Mutex
	active   map[string]*holder
	released chan struct{}
	drained  []chan struct{}
	fences   int
}

var Default = New()

func New() *Table { return &Table{active: map[string]*holder{}} }

func normalize(key string) string {
	if strings.HasPrefix(key, "path:") {
		resolved := path.Clean("/" + strings.TrimPrefix(key, "path:"))
		return "path:" + resolved
	}
	return key
}

func overlaps(left, right string) bool {
	return left == right || (strings.HasPrefix(left, "path:") && strings.HasPrefix(right, "path:") &&
		(left == "path:/" || right == "path:/" || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")))
}

// Fence refuses Acquire until lifted. After a restart nothing holds an
// interrupted operation's keys until recovery reaches it, so other work could
// take them first and leave that operation unreconciled.
func (t *Table) Fence() (lift func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fences++
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.fences--
		})
	}
}

// Acquire takes every key or none. background, when set, marks the holder
// as cancellable background work.
func (t *Table) Acquire(keys []string, background func()) (func(), error) {
	return t.acquire(keys, background, false)
}

const backgroundYield = 10 * time.Second

// AcquirePreempting is Acquire for work that background work must not hold up,
// such as a request someone is waiting on. It cancels background holders and
// waits up to backgroundYield for their cleanup to release the keys.
// background, when set, marks this holder as background work too.
func (t *Table) AcquirePreempting(ctx context.Context, keys []string, background func()) (func(), error) {
	deadline := time.NewTimer(backgroundYield)
	defer deadline.Stop()
	for {
		released := t.NextRelease()
		release, err := t.Acquire(keys, background)
		var holders *BackgroundConflict
		if !errors.As(err, &holders) {
			return release, err
		}
		holders.Cancel()
		select {
		case <-released:
		case <-deadline.C:
			return nil, ErrConflict
		case <-ctx.Done():
			return nil, ErrConflict
		}
	}
}

// AcquireForRecovery is Acquire for reconciling interrupted operations, which
// a Fence does not hold back.
func (t *Table) AcquireForRecovery(keys []string) (func(), error) {
	return t.acquire(keys, nil, true)
}

func (t *Table) acquire(keys []string, background func(), recovering bool) (func(), error) {
	seen := map[string]bool{}
	var ordered []string
	for _, key := range keys {
		normalized := normalize(key)
		if !seen[normalized] {
			seen[normalized] = true
			ordered = append(ordered, normalized)
		}
	}
	sort.Strings(ordered)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fences > 0 && !recovering {
		return nil, ErrRecovering
	}
	var conflicts []*holder
	for held, owner := range t.active {
		for _, key := range ordered {
			if overlaps(key, held) {
				conflicts = append(conflicts, owner)
				break
			}
		}
	}
	if len(conflicts) > 0 {
		var cancels []func()
		for _, owner := range conflicts {
			if owner.background == nil {
				return nil, ErrConflict
			}
			cancels = append(cancels, owner.background)
		}
		return nil, &BackgroundConflict{cancels: cancels}
	}
	owner := &holder{background: background}
	for _, key := range ordered {
		t.active[key] = owner
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			for _, key := range ordered {
				delete(t.active, key)
			}
			if t.released != nil {
				close(t.released)
				t.released = nil
			}
			if len(t.active) == 0 {
				for _, waiter := range t.drained {
					close(waiter)
				}
				t.drained = nil
			}
		})
	}, nil
}

// NextRelease is closed at the next release of any keys.
func (t *Table) NextRelease() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.released == nil {
		t.released = make(chan struct{})
	}
	return t.released
}

// ServerBusy reports whether work holds a server. Background holders don't
// count: whatever needs the server cancels them.
func (t *Table) ServerBusy(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	owner, busy := t.active["server:"+id]
	return busy && owner.background == nil
}

// Drained is closed once no keys are held. Call it after stopping new
// requests and jobs so stream cleanup and console commands finish before the
// database closes.
func (t *Table) Drained() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	done := make(chan struct{})
	if len(t.active) == 0 {
		close(done)
	} else {
		t.drained = append(t.drained, done)
	}
	return done
}
