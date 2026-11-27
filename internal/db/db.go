// Package db owns Ludock's SQLite database, its schema, and the identity key
// that protects persisted fingerprints.
package db

import (
	"context"
	"database/sql"
	"encoding/binary"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"modernc.org/sqlite"
)

var bg = context.Background()

// Now returns the current Unix time in milliseconds. Tests may replace it.
var Now = func() int64 { return time.Now().UnixMilli() }

type DB struct {
	*sql.DB
	path string
	key  []byte
}

// FileURI is path as an SQLite file URI with query, escaped so a folder
// named with ? or # still names that file.
func FileURI(path string, query url.Values) string {
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + query.Encode()
}

func exists(path string) (os.FileInfo, bool) {
	info, err := os.Stat(path)
	return info, err == nil
}

// inspect validates an existing file before WAL, chmod, migrations, or any
// other writes.
func inspect(path string) error {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	canonical, _ = filepath.Abs(canonical)
	// A beta's marker is read from the file header: opening a database with a
	// WAL left behind creates a shared-memory file beside it.
	if header, err := readHeader(canonical); err == nil && string(header[:16]) == "SQLite format 3\x00" &&
		slices.Contains(betaApplicationIDs, int64(binary.BigEndian.Uint32(header[68:72]))) {
		return ErrBetaDatabase
	}
	hasJournal := false
	for _, suffix := range []string{"-wal", "-journal"} {
		if info, ok := exists(canonical + suffix); ok && info.Size() > 0 {
			hasJournal = true
		}
	}
	// SQLite cannot always open a checkpointed WAL-mode database read-only
	// when its shared-memory files are absent. Immutable inspection avoids
	// creating sidecars; use the normal reader if journaled changes exist.
	query := url.Values{"mode": {"ro"}}
	if !hasJournal {
		query.Set("immutable", "1")
	}
	existing, err := sql.Open("sqlite", FileURI(canonical, query))
	if err != nil {
		return err
	}
	defer existing.Close()
	return AssertCompatible(bg, existing, LatestVersion())
}

func readHeader(path string) (header [100]byte, err error) {
	file, err := os.Open(path)
	if err != nil {
		return header, err
	}
	defer file.Close()
	_, err = io.ReadFull(file, header[:])
	return header, err
}

// Open opens or initializes the database at path (":memory:" for tests).
func Open(path string) (result *DB, err error) {
	memory := path == ":memory:"
	if !memory {
		if info, ok := exists(path); ok && info.Size() > 0 {
			if err := inspect(path); err != nil {
				return nil, err
			}
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
	}
	query := url.Values{"_txlock": {"immediate"}}
	// Operations are persisted before they act on Docker, so a commit must
	// survive power loss, not only a restart. WAL defaults differ by platform.
	query["_pragma"] = []string{"foreign_keys(1)", "busy_timeout(5000)", "synchronous(FULL)"}
	dsn := ""
	if memory {
		dsn = "file::memory:?" + query.Encode()
	} else {
		dsn = FileURI(path, query)
	}
	pool := sql.OpenDB(statementConnector{driver: &sqlite.Driver{}, dsn: dsn})
	var key []byte
	defer func() {
		if err != nil {
			clear(key)
			pool.Close()
		}
	}()
	connections := 4
	if memory {
		// Every in-memory connection is a separate database.
		connections = 1
	}
	// Keep every connection open: a new one parses the schema again and
	// starts without its cached statements.
	pool.SetMaxOpenConns(connections)
	pool.SetMaxIdleConns(connections)
	conn, err := pool.Conn(bg)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := AssertCompatible(bg, conn, LatestVersion()); err != nil {
		return nil, err
	}
	version, err := pragmaInt(bg, conn, "user_version")
	if err != nil {
		return nil, err
	}
	keyed := version >= BaseSchemaVersion
	if key, err = LoadKey(path, keyed); err != nil {
		return nil, err
	}
	if keyed {
		if err := assertKey(conn, key); err != nil {
			return nil, err
		}
	}
	if err := ApplyMigrations(bg, conn, Migrations, key); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(bg, "PRAGMA journal_mode = WAL"); err != nil {
		return nil, err
	}
	if !memory {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, err
		}
	}
	return &DB{DB: pool, path: path, key: key}, nil
}

// Close closes the pool and clears key material.
func (d *DB) Close() error {
	err := d.DB.Close()
	clear(d.key)
	return err
}

func (d *DB) Check(ctx context.Context) error {
	var one int
	return d.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// BindingFingerprint protects a binding digest with the identity key.
func (d *DB) BindingFingerprint(digest string) (string, error) {
	return protectDigest(digest, d.key, "binding:v2")
}

// Tx runs fn in an immediate transaction.
func (d *DB) Tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func IsUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
