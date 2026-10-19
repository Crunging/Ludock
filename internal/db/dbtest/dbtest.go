// Package dbtest opens databases for tests, including a copy of a database
// written by Ludock v0.5.0.
package dbtest

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Crunging/Ludock/internal/db"
)

func testdata(name string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "testdata", "v050", name)
}

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

// CopyV050 copies the v0.5.0 database and its identity key into dir and
// returns the database path.
func CopyV050(t testing.TB, dir string) string {
	t.Helper()
	data, err := os.ReadFile(testdata("ludock.db"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(testdata("identity-key"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ludock.db")
	keyDir := path + ".identity-key"
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(keyDir, 0o700)
	os.Chmod(keyDir, 0o700)
	if err := os.WriteFile(filepath.Join(keyDir, "key"), key, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// OpenV050 opens a copy of the v0.5.0 database.
func OpenV050(t testing.TB) *db.DB {
	t.Helper()
	d, err := db.Open(CopyV050(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// V050Vectors returns the raw vectors.json recorded with the fixture.
func V050Vectors(t testing.TB) []byte {
	t.Helper()
	data, err := os.ReadFile(testdata("vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
