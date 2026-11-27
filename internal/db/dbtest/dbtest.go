// Package dbtest opens databases for tests.
package dbtest

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Crunging/Ludock/internal/db"
)

// Migrating a new database takes about 150ms under the race detector, so
// each test writes out a copy of one migrated once per test binary.
var template = sync.OnceValues(func() ([]file, error) {
	dir, err := os.MkdirTemp("", "ludock-dbtest-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	d, err := db.Open(filepath.Join(dir, "ludock.db"))
	if err != nil {
		return nil, err
	}
	// Closing the last connection checkpoints the WAL into the file.
	if err := d.Close(); err != nil {
		return nil, err
	}
	var files []file
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		f := file{mode: info.Mode()}
		f.path, _ = filepath.Rel(dir, path)
		if !entry.IsDir() {
			if f.data, err = os.ReadFile(path); err != nil {
				return err
			}
		}
		files = append(files, f)
		return nil
	})
	return files, err
})

type file struct {
	path string
	mode fs.FileMode
	data []byte
}

// Open opens a fresh database in a temporary directory.
func Open(t testing.TB) *db.DB {
	t.Helper()
	files, err := template()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// Parents come before their files, and the identity key needs its modes.
	for _, f := range files {
		path := filepath.Join(dir, f.path)
		if f.mode.IsDir() {
			err = os.Mkdir(path, f.mode.Perm())
		} else {
			err = os.WriteFile(path, f.data, f.mode.Perm())
		}
		if err == nil {
			err = os.Chmod(path, f.mode.Perm())
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	d, err := db.Open(filepath.Join(dir, "ludock.db"))
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

// DeleteUser removes an account, without the administrator recheck a real
// change has; sessions and grants cascade.
func DeleteUser(ctx context.Context, d *db.DB, id string) error {
	_, err := d.ExecContext(ctx, "DELETE FROM users WHERE id = ?", id)
	return err
}

// DeleteUserSessions signs an account out everywhere, without the
// administrator recheck a real change has.
func DeleteUserSessions(ctx context.Context, d *db.DB, userID string) error {
	_, err := d.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", userID)
	return err
}

// UpdateUserAccess changes a role and disabled flag, without the
// administrator recheck a real change has. Disabling signs the account out
// everywhere.
func UpdateUserAccess(ctx context.Context, d *db.DB, id string, role db.Role, disabled bool) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		value := 0
		if disabled {
			value = 1
		}
		if _, err := tx.ExecContext(ctx, "UPDATE users SET role = ?, disabled = ?, updated_at = ? WHERE id = ?",
			string(role), value, db.Now(), id); err != nil {
			return err
		}
		if disabled {
			_, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", id)
			return err
		}
		return nil
	})
}
