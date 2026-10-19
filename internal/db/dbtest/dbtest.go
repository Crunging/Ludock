// Package dbtest opens databases for tests.
package dbtest

import (
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
