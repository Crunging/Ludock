package db

import (
	"context"
	"strings"
	"sync"

	"github.com/Crunging/Ludock/internal/apperr"
)

// ErrUpdating refuses a write while Ludock updates itself: putting a failed
// update back restores the data saved before it, which would silently undo
// the write, so it is never admitted.
var ErrUpdating = apperr.New("LUDOCK_UPDATING", 503, "Ludock is updating itself. Try again in a minute, once it is back.")

// writeGate admits writes where they begin, in the driver: every
// transaction, and every statement outside one that writes.
type writeGate struct {
	mu     sync.Mutex
	closed int
	active int
	idle   chan struct{}
}

type maintenanceKey struct{}

// Maintenance marks ctx as the update's own, whose writes pass a closed gate.
func Maintenance(ctx context.Context) context.Context {
	return context.WithValue(ctx, maintenanceKey{}, true)
}

func (g *writeGate) enter(ctx context.Context) (leave func(), err error) {
	if g == nil || ctx.Value(maintenanceKey{}) != nil {
		return func() {}, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed > 0 {
		return nil, ErrUpdating
	}
	g.active++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.active--; g.active == 0 && g.idle != nil {
				close(g.idle)
				g.idle = nil
			}
		})
	}, nil
}

// CloseWrites refuses every new write but the update's own, then waits for
// the admitted ones to end. reopen admits writes again; a process that hands
// itself over to an update never calls it.
func (d *DB) CloseWrites(ctx context.Context) (reopen func(), err error) {
	g := d.writes
	g.mu.Lock()
	g.closed++
	var once sync.Once
	reopen = func() {
		once.Do(func() {
			g.mu.Lock()
			g.closed--
			g.mu.Unlock()
		})
	}
	for g.active > 0 {
		if g.idle == nil {
			g.idle = make(chan struct{})
		}
		idle := g.idle
		g.mu.Unlock()
		select {
		case <-idle:
		case <-ctx.Done():
			reopen()
			return nil, ctx.Err()
		}
		g.mu.Lock()
	}
	g.mu.Unlock()
	return reopen, nil
}

// writes reports whether a statement outside a transaction may write.
func writes(query string) bool {
	words := strings.Fields(query)
	if len(words) == 0 {
		return false
	}
	switch strings.ToUpper(words[0]) {
	case "SELECT", "WITH", "EXPLAIN":
		return false
	case "PRAGMA":
		return strings.Contains(query, "=")
	}
	return true
}
