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

func maintenance(ctx context.Context) bool { return ctx.Value(maintenanceKey{}) != nil }

func (g *writeGate) enter(ctx context.Context) (leave func(), err error) {
	if g == nil || maintenance(ctx) {
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

// readPragmas are the PRAGMAs Ludock reads; any other may write, as
// optimize does.
var readPragmas = map[string]bool{"application_id": true, "user_version": true, "database_list": true, "foreign_key_check": true,
	"foreign_keys": true, "journal_mode": true, "quick_check": true, "integrity_check": true}

// checkPragmas read whatever argument they are given.
var checkPragmas = map[string]bool{"foreign_key_check": true, "quick_check": true, "integrity_check": true}

// changes reports whether a statement changes rows and can run inside a
// transaction.
func changes(query string) bool {
	words := strings.Fields(query)
	if len(words) == 0 || strings.Contains(strings.TrimSuffix(strings.TrimSpace(query), ";"), ";") {
		return false
	}
	switch strings.ToUpper(words[0]) {
	case "INSERT", "UPDATE", "DELETE", "REPLACE", "WITH":
		return true
	}
	return false
}

// writes reports whether a statement may write: anything but a single
// SELECT or EXPLAIN, or reading one of readPragmas.
func writes(query string) bool {
	query = strings.TrimSuffix(strings.TrimSpace(query), ";")
	words := strings.Fields(query)
	if len(words) == 0 {
		return false
	}
	if strings.Contains(query, ";") {
		return true
	}
	switch strings.ToUpper(words[0]) {
	case "SELECT", "EXPLAIN":
		return false
	case "PRAGMA":
		// PRAGMA name(value) sets as PRAGMA name = value does, but for the
		// checks, whose argument is what to check.
		rest := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(query, words[0])))
		name, argument, called := strings.Cut(rest, "(")
		name = strings.TrimPrefix(strings.TrimSpace(name), "main.")
		if strings.Contains(name, "=") || !readPragmas[name] {
			return true
		}
		return called && !checkPragmas[name] || strings.Contains(argument, "=")
	}
	return true
}
