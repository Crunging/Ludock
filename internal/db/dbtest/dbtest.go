// Package dbtest opens databases for tests.
package dbtest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Crunging/Ludock/internal/db"
)

// Open opens a fresh database in a temporary directory.
func Open(t testing.TB) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "ludock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// Session signs an existing user in with a session that does not expire and
// returns its token hash.
func Session(t testing.TB, d *db.DB, userID string) string {
	t.Helper()
	hash := "test-session-" + userID
	if err := d.CreateSession(context.Background(), db.SessionInput{SessionID: hash, TokenHash: hash, UserID: userID,
		CreatedAt: 1, ExpiresAt: 1 << 62}); err != nil {
		t.Fatal(err)
	}
	return hash
}
