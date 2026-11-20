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

// A database with Discord-only notifications keeps its webhook and queue
// when notifications gain more destinations.
func TestNotificationDestinationsMigration(t *testing.T) {
	_, conn := memoryConn(t)
	if err := ApplyMigrations(bg, conn, Migrations[:3], testKey); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO settings VALUES ('notifications', '{"enabled":true,"webhookUrl":"https://discord.com/api/webhooks/1/a"}')`,
		`INSERT INTO notification_deliveries (id,event_key,payload_json,next_attempt_at,created_at,attempts,failure_code)
		  VALUES ('d1','outage:1','{"content":"Survival is unavailable.","allowed_mentions":{"parse":[]}}',5,4,2,'network')`,
	} {
		if _, err := conn.ExecContext(bg, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := ApplyMigrations(bg, conn, Migrations[:4], testKey); err != nil {
		t.Fatal(err)
	}
	var settings, row string
	conn.QueryRowContext(bg, "SELECT value_json FROM settings WHERE key='notifications'").Scan(&settings)
	conn.QueryRowContext(bg, `SELECT event_key||'|'||destination||'|'||message||'|'||attempts||'|'||failure_code
	  FROM notification_deliveries WHERE id='d1'`).Scan(&row)
	if settings != `{"discord":{"enabled":true,"url":"https://discord.com/api/webhooks/1/a"}}` ||
		row != "outage:1|discord|Survival is unavailable.|2|network" {
		t.Fatal(settings, row)
	}
	// Each destination gets its own delivery of an event.
	if _, err := conn.ExecContext(bg, `INSERT INTO notification_deliveries (id,event_key,destination,message,next_attempt_at,created_at)
	  VALUES ('d2','outage:1','ntfy','Survival is unavailable.',5,4)`); err != nil {
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

// Schedules saved before their save time was recorded get the upgrade time,
// so none runs late for a minute before the upgrade.
func TestScheduleSaveTimeMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.db")
	before, err := open(path, Migrations[:4])
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES('u','u','x','admin',1,1)`,
		`INSERT INTO docker_hosts(id,name,created_at) VALUES('h','local',1)`,
		`INSERT INTO logical_servers(id,host_id,external_identity,display_name,game_type,status,binding_revision,binding_fingerprint,first_seen_at,last_seen_at)
      VALUES('s','h','standalone:mc','mc','minecraft','active',1,'',1,1)`,
		`INSERT INTO schedules(id,server_id,owner_id,input_json,binding_revision,created_at) VALUES('a','s','u','{}',1,1)`,
	} {
		if _, err := before.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	before.Close()
	started := Now()
	after := mustOpen(t, path)
	var changed int64
	if err := after.QueryRow("SELECT changed_at FROM schedules WHERE id='a'").Scan(&changed); err != nil || changed < started-1000 {
		t.Fatal(changed, err)
	}
}

// Countdown warnings claimed before deliveries were recorded upgrade as not
// heard, so no call-off follows one nobody may have heard; a delivery can
// then be recorded on them.
func TestHeardBindingMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "announcements.db")
	before, err := open(path, Migrations[:11])
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES('u','u','x','admin',1,1)`,
		`INSERT INTO docker_hosts(id,name,created_at) VALUES('h','local',1)`,
		`INSERT INTO logical_servers(id,host_id,external_identity,display_name,game_type,status,binding_revision,binding_fingerprint,first_seen_at,last_seen_at)
      VALUES('s','h','standalone:mc','mc','minecraft','active',1,'',1,1)`,
		`INSERT INTO schedules(id,server_id,owner_id,input_json,binding_revision,created_at) VALUES('a','s','u','{}',1,1)`,
		`INSERT INTO schedule_announcements(schedule_id,slot,lead_minutes) VALUES('a','2026-01-01T05:00:UTC',5)`,
	} {
		if _, err := before.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	before.Close()
	after := mustOpen(t, path)
	var heard sql.NullInt64
	if err := after.QueryRow("SELECT heard_binding FROM schedule_announcements WHERE schedule_id='a'").Scan(&heard); err != nil || heard.Valid {
		t.Fatal(heard, err)
	}
	if _, err := after.Exec("UPDATE schedule_announcements SET heard_binding=1 WHERE schedule_id='a'"); err != nil {
		t.Fatal(err)
	}
}

// Operators and viewers become members without losing their sessions or
// schedules; a viewer gains nothing their role withheld, even from a malformed
// grant, and an administrator's leftover grant is gone.
func TestMemberRoleMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roles.db")
	before, err := open(path, Migrations[:8])
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES('a','a','x','admin',1,1),
      ('o','o','x','operator',1,1),('v','v','x','viewer',1,1)`,
		`INSERT INTO sessions(token_hash,session_id,user_id,created_at,expires_at,last_seen_at) VALUES('t','s','v',1,9,1)`,
		`INSERT INTO docker_hosts(id,name,created_at) VALUES('h','local',1)`,
		`INSERT INTO logical_servers(id,host_id,external_identity,display_name,game_type,status,binding_revision,binding_fingerprint,first_seen_at,last_seen_at)
      VALUES('s','h','standalone:mc','mc','minecraft','active',1,'',1,1),('t','h','standalone:t','t','minecraft','active',1,'',1,1),
      ('u','h','standalone:u','u','minecraft','active',1,'',1,1)`,
		`INSERT INTO server_grants VALUES('v','t','{"entry":"server.view"}',1),('v','u','["server.view",1e400]',1),('a','s','["server.view"]',1),('o','s','["server.view","server.start"]',1),
      ('v','s','["server.view","server.start","files.read"]',1)`,
		`INSERT INTO schedules(id,server_id,owner_id,input_json,binding_revision,created_at) VALUES('x','s','o','{}',1,1)`,
	} {
		if _, err := before.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	before.Close()
	after := mustOpen(t, path)
	var roles, grants string
	var sessions, schedules int
	after.QueryRow("SELECT group_concat(id||'='||role, ',') FROM (SELECT * FROM users ORDER BY id)").Scan(&roles)
	after.QueryRow("SELECT group_concat(user_id||'='||capabilities_json, ' ') FROM (SELECT * FROM server_grants ORDER BY user_id, server_id)").Scan(&grants)
	after.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&sessions)
	after.QueryRow("SELECT COUNT(*) FROM schedules").Scan(&schedules)
	if roles != "a=admin,o=member,v=member" || sessions != 1 || schedules != 1 ||
		grants != `o=["server.view","server.start"] v=["server.view","files.read"] v=[] v=[]` {
		t.Fatal(roles, grants, sessions, schedules)
	}
	if got := pragma(t, after.DB, "foreign_keys"); got != "1" {
		t.Fatalf("foreign_keys = %s", got)
	}
	if _, err := after.Exec("INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES('z','z','x','viewer',1,1)"); err == nil {
		t.Fatal("old role accepted")
	}
}

// Servers keep their bindings, paused sharing, and everything that refers to
// them when the review fields and binding history go; one 0.6.0 left waiting
// for review stays unusable until discovery settles it, and the schema ends
// as a fresh database's does.
func TestSharingPausedMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.db")
	before, err := open(path, Migrations[:12])
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES('a','a','x','admin',1,1),('m','m','x','member',1,1)`,
		`INSERT INTO docker_hosts(id,name,created_at) VALUES('h','local',1)`,
		`INSERT INTO logical_servers(id,host_id,external_identity,container_id,display_name,game_type,status,binding_revision,
        binding_fingerprint,pending_fingerprint,pending_game_type,review_required,first_seen_at,last_seen_at)
      VALUES('s','h','standalone:mc','c1','mc','minecraft','active',3,'f3',NULL,NULL,0,1,2),
        ('p','h','standalone:p','c2','p','valheim','active',5,'f5',NULL,NULL,1,1,2),
        ('r','h','standalone:r','c3','r','minecraft','review_required',2,'f1','f2','terraria',1,1,2),
        ('g','h','standalone:g',NULL,'g','unknown','ambiguous',0,'',NULL,NULL,0,1,2)`,
		`INSERT INTO server_bindings(server_id,binding_revision,container_id,binding_fingerprint,observed_at,accepted)
      VALUES('s',3,'c1','f3',2,1),('r',2,'c3','f2',2,0)`,
		`INSERT INTO server_grants VALUES('m','p','["server.view"]',1),('m','g','["server.view"]',1)`,
		`INSERT INTO operations(id,server_id,actor_id,kind,status,phase,input_json,binding_revision,created_at,updated_at)
      VALUES('o','s','a','start','succeeded','done','{}',3,1,1)`,
		`INSERT INTO backups(id,server_id,binding_fingerprint,destination,roots_json,size,checksum,created_at,state)
      VALUES('b','p','f5','/b','[]',1,'x',1,'complete')`,
		`INSERT INTO schedules(id,server_id,owner_id,input_json,binding_revision,created_at) VALUES('x','s','a','{}',3,1)`,
		`INSERT INTO availability(server_id,policy_json,updated_at) VALUES('s','{}',1)`,
		`INSERT INTO resource_samples VALUES('p',1,1,1)`,
	} {
		if _, err := before.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	before.Close()
	after := mustOpen(t, path)
	var servers string
	after.QueryRow(`SELECT group_concat(row, ' ') FROM (SELECT id||'='||COALESCE(container_id,'-')||','||game_type||','||status||','||
      binding_revision||','||binding_fingerprint||','||sharing_paused||','||first_seen_at||','||last_seen_at AS row
      FROM logical_servers ORDER BY id)`).Scan(&servers)
	if servers != "g=-,unknown,ambiguous,0,,0,1,2 p=c2,valheim,active,5,f5,1,1,2 r=c3,minecraft,missing,2,f1,1,1,2 s=c1,minecraft,active,3,f3,0,1,2" {
		t.Fatal(servers)
	}
	var references int
	after.QueryRow(`SELECT (SELECT COUNT(*) FROM server_grants) + (SELECT COUNT(*) FROM operations) + (SELECT COUNT(*) FROM backups) +
      (SELECT COUNT(*) FROM schedules) + (SELECT COUNT(*) FROM availability) + (SELECT COUNT(*) FROM resource_samples)`).Scan(&references)
	if references != 7 {
		t.Fatal(references)
	}
	if _, err := after.Exec("UPDATE logical_servers SET status='review_required' WHERE id='s'"); err == nil {
		t.Fatal("review status accepted")
	}
	if _, err := after.Exec(`INSERT INTO operations(id,server_id,actor_id,kind,status,phase,input_json,binding_revision,created_at,updated_at)
      VALUES('o2','nobody','a','start','queued','queued','{}',1,1,1)`); err == nil {
		t.Fatal("operation for an unknown server accepted")
	}
	// Removing a server still removes its grants.
	if _, err := after.Exec("DELETE FROM logical_servers WHERE id='g'"); err != nil {
		t.Fatal(err)
	}
	var grants int
	if err := after.QueryRow("SELECT COUNT(*) FROM server_grants WHERE server_id='g'").Scan(&grants); err != nil || grants != 0 {
		t.Fatal(grants, err)
	}
	fresh := mustOpen(t, filepath.Join(t.TempDir(), "fresh.db"))
	if got, want := schema(t, after.DB), schema(t, fresh.DB); got != want {
		t.Fatalf("migrated schema differs from a fresh one:\n%s\n---\n%s", got, want)
	}
	for _, retired := range []string{"server_bindings", "review_required", "pending_"} {
		if strings.Contains(schema(t, after.DB), retired) {
			t.Fatal(retired)
		}
	}
}

func schema(t *testing.T, d *sql.DB) string {
	t.Helper()
	var text string
	if err := d.QueryRow("SELECT group_concat(type||' '||name||' '||COALESCE(sql,''), char(10)) FROM (SELECT * FROM sqlite_schema ORDER BY name)").Scan(&text); err != nil {
		t.Fatal(err)
	}
	return text
}
