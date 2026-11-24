package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/logging"
)

func pragma(t *testing.T, d *sql.DB, name string) string {
	t.Helper()
	var value string
	if err := d.QueryRow("PRAGMA " + name).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func mustOpen(t *testing.T, path string) *DB {
	t.Helper()
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestFreshDatabaseAndReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fresh.db")
	d := mustOpen(t, path)
	if got := pragma(t, d.DB, "application_id"); got != "1279544113" {
		t.Fatalf("application_id = %s", got)
	}
	if got := pragma(t, d.DB, "user_version"); got != strconv.Itoa(LatestVersion()) {
		t.Fatalf("user_version = %s", got)
	}
	if got := pragma(t, d.DB, "journal_mode"); got != "wal" {
		t.Fatalf("journal_mode = %s", got)
	}
	if got := pragma(t, d.DB, "synchronous"); got != "2" {
		t.Fatalf("synchronous = %s", got)
	}
	if got := pragma(t, d.DB, "foreign_keys"); got != "1" {
		t.Fatalf("foreign_keys = %s", got)
	}
	var check string
	d.QueryRow("SELECT value_json FROM settings WHERE key='identity.key-check'").Scan(&check)
	if !regexp.MustCompile(`^"[a-f0-9]{64}"$`).MatchString(check) {
		t.Fatalf("key check = %q", check)
	}
	// Every writer sets these, so a row missing one is a bug, not a default.
	for _, column := range []string{"backups.game_type", "schedules.changed_at", "notification_deliveries.kind"} {
		table, name, _ := strings.Cut(column, ".")
		var notNull int
		var fallback sql.NullString
		d.QueryRow("SELECT \"notnull\", dflt_value FROM pragma_table_info(?) WHERE name=?", table, name).Scan(&notNull, &fallback)
		if notNull != 1 || fallback.Valid {
			t.Fatal(column, "may be left out", notNull, fallback.String)
		}
	}
	if _, err := d.Exec("INSERT INTO docker_hosts (id, name, created_at) VALUES ('host', 'test', 1)"); err != nil {
		t.Fatal(err)
	}
	keyDir := path + ".identity-key"
	if mode(t, keyDir) != 0o700 || mode(t, filepath.Join(keyDir, "key")) != 0o600 {
		t.Fatal("unexpected key modes")
	}
	key, _ := os.ReadFile(filepath.Join(keyDir, "key"))
	if len(key) != 32 {
		t.Fatal("key length")
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".stage-") {
			t.Fatal("staging directory left behind")
		}
	}
	d.Close()
	d = mustOpen(t, path)
	defer d.Close()
	var count int
	d.QueryRow("SELECT COUNT(*) FROM docker_hosts").Scan(&count)
	if count != 1 {
		t.Fatal("row lost on reopen")
	}
	again, _ := os.ReadFile(filepath.Join(keyDir, "key"))
	if !bytes.Equal(key, again) || mode(t, path) != 0o600 {
		t.Fatal("key changed or database mode wrong")
	}
}

// Any database other than an empty one or this version's is refused with its
// reason before anything writes to it: one unrelated to Ludock, a beta's (up
// to 0.5, 0.6.0, and 0.6's later development schemas, whose versions overlap
// 1.0's, and one whose WAL was left behind), a newer version's, and one
// holding only a view.
func TestRefusesOtherDatabasesUnchanged(t *testing.T) {
	for _, test := range []struct {
		name, setup string
		want        error
		wal         bool
	}{
		{"unrelated", "CREATE TABLE inventory_items (id TEXT); INSERT INTO inventory_items VALUES ('preserve-this')", ErrIncompatible, false},
		{"unmarked 0.1", "CREATE TABLE users (id TEXT PRIMARY KEY)", ErrIncompatible, false},
		{"0.5", "CREATE TABLE users (id TEXT PRIMARY KEY); PRAGMA application_id = 1280656459; PRAGMA user_version = 9", ErrBetaDatabase, false},
		{"0.6.0", "CREATE TABLE users (id TEXT PRIMARY KEY); PRAGMA application_id = 1279542091; PRAGMA user_version = 4", ErrBetaDatabase, false},
		{"0.6 development", "CREATE TABLE users (id TEXT PRIMARY KEY); PRAGMA application_id = 1279542091; PRAGMA user_version = 14", ErrBetaDatabase, false},
		{"beta at version 1", "CREATE TABLE users (id TEXT PRIMARY KEY); PRAGMA application_id = 1279542091; PRAGMA user_version = 1", ErrBetaDatabase, false},
		{"beta with a WAL", "CREATE TABLE users (id TEXT PRIMARY KEY); PRAGMA application_id = 1279542091; PRAGMA user_version = 4", ErrBetaDatabase, true},
		{"only a view", "CREATE VIEW inventory AS SELECT 1", ErrIncompatible, false},
		{"newer", "CREATE TABLE users (id TEXT PRIMARY KEY); PRAGMA application_id = 1279544113; PRAGMA user_version = 999", ErrNewerDatabase, false},
		{"foreign marker", "CREATE TABLE users (id TEXT PRIMARY KEY); PRAGMA application_id = 123; PRAGMA user_version = 1", ErrIncompatible, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "ludock.db")
			raw, _ := sql.Open("sqlite", path)
			if _, err := raw.Exec(test.setup); err != nil {
				t.Fatal(err)
			}
			raw.Close()
			if test.wal {
				os.WriteFile(path+"-wal", []byte("left by a crash"), 0o600)
			}
			os.Chmod(path, 0o640)
			original, _ := os.ReadFile(path)
			if _, err := Open(path); err != test.want {
				t.Fatalf("err = %v", err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(original, after) || mode(t, path) != 0o640 {
				t.Fatal("database changed")
			}
			for _, suffix := range []string{"-wal", "-journal", "-shm", ".identity-key"} {
				if _, err := os.Lstat(path + suffix); err == nil && !(test.wal && suffix == "-wal") {
					t.Fatalf("%s created", suffix)
				}
			}
		})
	}
}

// Interrupted key write never publishes partial key.
func TestInterruptedKeyWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "partial.db")
	writeKeyFile = func(*os.File, []byte) error { return errors.New("simulated interrupted key write") }
	_, err := Open(path)
	writeKeyFile = func(f *os.File, key []byte) error { _, err := f.Write(key); return err }
	if !errors.Is(err, ErrIdentityKey) {
		t.Fatalf("err = %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "identity-key") {
			t.Fatalf("left %s", entry.Name())
		}
	}

	// A failure after the rename but before the parent sync leaves a durable,
	// complete key that the next start uses.
	durable := filepath.Join(dir, "durable.db")
	calls := 0
	syncFile = func(f *os.File) error {
		calls++
		if calls == 3 {
			return errors.New("simulated interruption before parent sync")
		}
		return f.Sync()
	}
	_, err = Open(durable)
	syncFile = func(f *os.File) error { return f.Sync() }
	if !errors.Is(err, ErrIdentityKey) {
		t.Fatalf("err = %v", err)
	}
	published, err := os.ReadFile(filepath.Join(durable+".identity-key", "key"))
	if err != nil || len(published) != 32 {
		t.Fatal("key not published")
	}
	d := mustOpen(t, durable)
	defer d.Close()
	if pragma(t, d.DB, "user_version") != strconv.Itoa(LatestVersion()) {
		t.Fatal("not migrated")
	}
	again, _ := os.ReadFile(filepath.Join(durable+".identity-key", "key"))
	if !bytes.Equal(published, again) {
		t.Fatal("key replaced")
	}
}

func TestRejectsMissingReplacedAndUnsafeKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key-validation.db")
	mustOpen(t, path).Close()
	keyDir := path + ".identity-key"
	saved := keyDir + ".saved"
	savedKey := filepath.Join(saved, "key")
	databaseBytes, _ := os.ReadFile(path)
	os.Rename(keyDir, saved)
	keyBytes, _ := os.ReadFile(savedKey)

	reject := func(pattern string) {
		t.Helper()
		_, err := Open(path)
		if err == nil || !regexp.MustCompile(pattern).MatchString(err.Error()) {
			t.Fatalf("err = %v, want %s", err, pattern)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(databaseBytes, after) {
			t.Fatal("database changed")
		}
	}
	reject("identity key")
	if _, err := os.Lstat(keyDir); err == nil {
		t.Fatal("replacement key generated")
	}
	os.WriteFile(keyDir, []byte("not-a-directory"), 0o600)
	reject("identity key")
	os.Remove(keyDir)
	os.Mkdir(keyDir, 0o700)
	reject("identity key")
	os.Remove(keyDir)
	os.Symlink(saved, keyDir)
	reject("identity key")
	os.Remove(keyDir)

	os.Mkdir(keyDir, 0o755)
	os.Chmod(keyDir, 0o755)
	os.WriteFile(filepath.Join(keyDir, "key"), keyBytes, 0o600)
	reject(`directory key-validation\.db\.identity-key has unsafe permissions: expected 0700, found 0755\. To fix it, in the database's directory, run setfacl -b 'key-validation\.db\.identity-key' && chmod 700 'key-validation\.db\.identity-key'$`)
	os.RemoveAll(keyDir)

	for _, key := range [][]byte{make([]byte, 3), bytes.Repeat([]byte{1}, 32)} {
		os.Mkdir(keyDir, 0o700)
		os.WriteFile(filepath.Join(keyDir, "key"), key, 0o600)
		reject("identity key")
		os.RemoveAll(keyDir)
	}

	os.Mkdir(keyDir, 0o700)
	os.WriteFile(filepath.Join(keyDir, "key"), keyBytes, 0o644)
	os.Chmod(filepath.Join(keyDir, "key"), 0o644)
	reject(`expected 0600, found 0644\. To fix it, in the database's directory, run setfacl -b 'key-validation\.db\.identity-key/key' && chmod 600 `)
	os.RemoveAll(keyDir)

	os.Mkdir(keyDir, 0o700)
	os.Symlink(savedKey, filepath.Join(keyDir, "key"))
	reject("identity key")
	os.RemoveAll(keyDir)

	os.Mkdir(keyDir, 0o700)
	os.Link(savedKey, filepath.Join(keyDir, "key"))
	reject("identity key")
	os.RemoveAll(keyDir)

	os.Rename(saved, keyDir)
	mustOpen(t, path).Close()
}

func TestGroupOrACLAccessWarnsWithoutFailing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "group-key.db")
	mustOpen(t, path).Close()
	keyDir := path + ".identity-key"
	os.Chmod(keyDir, 0o770)
	os.Chmod(filepath.Join(keyDir, "key"), 0o660)
	var stdout, stderr bytes.Buffer
	logging.SetOutput(&stdout, &stderr)
	defer logging.SetOutput(os.Stdout, os.Stderr)
	mustOpen(t, path).Close()
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("warnings = %q", lines)
	}
	for _, line := range lines {
		if strings.Contains(line, dir) || !strings.Contains(line, "group bits or a POSIX ACL") || !strings.Contains(line, "setfacl -b") {
			t.Fatalf("warning = %q", line)
		}
	}
	for _, pair := range [][2]string{{"0700", "0770"}, {"0600", "0660"}} {
		if !strings.Contains(stderr.String(), "expected "+pair[0]+", found "+pair[1]) {
			t.Fatalf("missing %v in %q", pair, stderr.String())
		}
	}
	// World access still refuses to start.
	os.Chmod(filepath.Join(keyDir, "key"), 0o604)
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "unsafe permissions") {
		t.Fatalf("err = %v", err)
	}
}

func memoryConn(t *testing.T) (*sql.DB, *sql.Conn) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file::memory:?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	conn, err := raw.Conn(bg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); raw.Close() })
	return raw, conn
}

var testKey = bytes.Repeat([]byte{42}, 32)

func TestFutureMigrationKeepsRecords(t *testing.T) {
	_, conn := memoryConn(t)
	if err := ApplyMigrations(bg, conn, Migrations, testKey); err != nil {
		t.Fatal(err)
	}
	conn.ExecContext(bg, "INSERT INTO users (id, username, password_hash, role, disabled, created_at, updated_at) VALUES ('u','player','hash','member',0,1,1)")
	next := append(append([]Migration(nil), Migrations...), Migration{Version: LatestVersion() + 1,
		SQL: "CREATE TABLE test_preferences (user_id TEXT PRIMARY KEY REFERENCES users(id), note TEXT NOT NULL) STRICT;"})
	if err := ApplyMigrations(bg, conn, next, testKey); err != nil {
		t.Fatal(err)
	}
	var name string
	conn.QueryRowContext(bg, "SELECT username FROM users WHERE id='u'").Scan(&name)
	if name != "player" {
		t.Fatal("record lost")
	}
	if err := ApplyMigrations(bg, conn, next, testKey); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptedMigrationRollsBack(t *testing.T) {
	_, conn := memoryConn(t)
	if err := ApplyMigrations(bg, conn, Migrations, testKey); err != nil {
		t.Fatal(err)
	}
	broken := append(append([]Migration(nil), Migrations...), Migration{Version: LatestVersion() + 1,
		SQL: "CREATE TABLE should_rollback (id TEXT); INSERT INTO nonexistent VALUES (1);"})
	if err := ApplyMigrations(bg, conn, broken, testKey); err == nil {
		t.Fatal("expected failure")
	}
	var version, tables int
	conn.QueryRowContext(bg, "PRAGMA user_version").Scan(&version)
	conn.QueryRowContext(bg, "SELECT COUNT(*) FROM sqlite_schema WHERE name='should_rollback'").Scan(&tables)
	if version != LatestVersion() || tables != 0 {
		t.Fatalf("version=%d tables=%d", version, tables)
	}
	if err := ApplyMigrations(bg, conn, Migrations, testKey); err != nil {
		t.Fatal(err)
	}
}

// Reading a throttle reports an active window and removes an expired one.
func TestGetLoginThrottleExpiry(t *testing.T) {
	d := mustOpen(t, filepath.Join(t.TempDir(), "throttle.db"))
	defer d.Close()
	if _, err := d.RecordLoginFailure(bg, "key", 1000, 60_000, 5, 0); err != nil {
		t.Fatal(err)
	}
	if throttle, err := d.GetLoginThrottle(bg, "key", 30_000, 60_000); err != nil || throttle.Failures != 1 {
		t.Fatal(throttle, err)
	}
	if throttle, err := d.GetLoginThrottle(bg, "key", 61_000, 60_000); err != nil || throttle != (LoginThrottle{}) {
		t.Fatal(throttle, err)
	}
	var rows int
	d.QueryRow("SELECT COUNT(*) FROM login_attempts").Scan(&rows)
	if rows != 0 {
		t.Fatal("expired window kept")
	}
}

// Pruning keeps a restore whose journal isn't final, which its undo needs,
// and removes old restores without one like other work.
func TestPruneKeepsUnfinishedRestores(t *testing.T) {
	d := mustOpen(t, filepath.Join(t.TempDir(), "prune.db"))
	for _, statement := range []string{
		`INSERT INTO docker_hosts(id,name,created_at) VALUES('h','local',1)`,
		`INSERT INTO logical_servers(id,host_id,external_identity,display_name,game_type,status,binding_revision,binding_fingerprint,first_seen_at,last_seen_at)
      VALUES('s','h','standalone:mc','mc','minecraft','active',1,'',1,1)`,
		`INSERT INTO operations(id,server_id,actor_id,kind,status,phase,input_json,recovery_json,binding_revision,created_at,updated_at)
      VALUES('pending','s','a','restore','failed','failed','{}','{"restore":{"status":"rolling_back","roots":[]}}',1,1,1),
        ('plain','s','a','restore','failed','failed','{}','{}',1,1,1), ('done','s','a','restore','failed','failed','{}','{"restore":{"status":"rolled_back"}}',1,1,1)`,
	} {
		if _, err := d.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for i := range operationsKeptPerServer {
		if _, err := d.Exec(`INSERT INTO operations(id,server_id,actor_id,kind,status,phase,input_json,binding_revision,created_at,updated_at)
      VALUES(?,'s','a','start','succeeded','succeeded','{}',1,?,?)`, fmt.Sprint("newer-", i), 10+i, Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.PruneOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	var left string
	d.QueryRow("SELECT group_concat(id) FROM operations WHERE kind='restore'").Scan(&left)
	if left != "pending" {
		t.Fatal(left)
	}
}
