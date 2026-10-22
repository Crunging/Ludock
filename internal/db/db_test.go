package db

import (
	"bytes"
	"database/sql"
	"errors"
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
	if got := pragma(t, d.DB, "application_id"); got != "1279542091" {
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

// Rejects unrelated database without writing.
func TestRejectsUnrelatedDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unrelated.db")
	raw, _ := sql.Open("sqlite", path)
	raw.Exec("CREATE TABLE inventory_items (id TEXT, description TEXT); INSERT INTO inventory_items VALUES ('asset-1', 'preserve-this');")
	raw.Close()
	os.Chmod(path, 0o640)
	original, _ := os.ReadFile(path)
	if _, err := Open(path); !errors.Is(err, ErrIncompatible) || !strings.Contains(err.Error(), "new application data volume") {
		t.Fatalf("err = %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(original, after) || mode(t, path) != 0o640 {
		t.Fatal("database changed")
	}
	for _, suffix := range []string{"-wal", ".identity-key"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			t.Fatalf("%s created", suffix)
		}
	}
}

// A database from releases up to 0.5 carries their marker and schema; it is
// refused before anything writes to it.
func TestRejectsOldReleaseDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	raw, _ := sql.Open("sqlite", path)
	raw.Exec("CREATE TABLE users (id TEXT PRIMARY KEY); PRAGMA application_id = 1280656459; PRAGMA user_version = 9")
	raw.Close()
	os.Chmod(path, 0o640)
	original, _ := os.ReadFile(path)
	if _, err := Open(path); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("err = %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(original, after) || mode(t, path) != 0o640 {
		t.Fatal("database changed")
	}
	for _, suffix := range []string{"-wal", "-journal", ".identity-key"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			t.Fatalf("%s created", suffix)
		}
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
	conn.ExecContext(bg, "INSERT INTO users VALUES ('u','player','hash','viewer',0,1,1)")
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

func TestRejectsNewerSchemaAndForeignMarker(t *testing.T) {
	_, conn := memoryConn(t)
	ApplyMigrations(bg, conn, Migrations, testKey)
	conn.ExecContext(bg, "PRAGMA user_version = 999")
	if err := AssertCompatible(bg, conn, BaseSchemaVersion, LatestVersion()); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("err = %v", err)
	}
	conn.ExecContext(bg, "PRAGMA user_version = 1; PRAGMA application_id = 123")
	if err := ApplyMigrations(bg, conn, Migrations, testKey); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("err = %v", err)
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
