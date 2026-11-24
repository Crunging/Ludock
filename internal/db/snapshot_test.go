package db

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
)

func openTemp(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ludock.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, path
}

// saveSnapshot writes a snapshot to a file and checks its size and checksum.
func saveSnapshot(t *testing.T, d *DB) string {
	t.Helper()
	snapshot, err := d.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	path := filepath.Join(t.TempDir(), "ludock-data-20261120-120000.tar")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	size, err := io.Copy(io.MultiWriter(out, digest), snapshot)
	out.Close()
	if err != nil || size != snapshot.Size || hex.EncodeToString(digest.Sum(nil)) != snapshot.Checksum {
		t.Fatal("snapshot size or checksum differs from its contents", err)
	}
	return path
}

// Rows are written in pairs, one transaction each, while snapshots are
// taken. Every snapshot is an intact database holding whole pairs, and the
// writes carry on meanwhile.
func TestSnapshotConsistentWhileWriting(t *testing.T) {
	d, _ := openTemp(t)
	if _, err := d.Exec("CREATE TABLE pairs (n INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	var written atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := 0; !stop.Load(); n++ {
			err := d.Tx(context.Background(), func(tx *sql.Tx) error {
				if _, err := tx.Exec("INSERT INTO pairs VALUES (?)", n); err != nil {
					return err
				}
				_, err := tx.Exec("INSERT INTO pairs VALUES (?)", n)
				return err
			})
			if err != nil {
				t.Error(err)
				return
			}
			written.Add(1)
		}
	}()
	var counts []int
	for range 5 {
		path := saveSnapshot(t, d)
		folder := t.TempDir()
		if _, err := RestoreSnapshot(path, filepath.Join(folder, "ludock.db")); err != nil {
			t.Fatal(err)
		}
		restored, err := sql.Open("sqlite", fileURI(filepath.Join(folder, "ludock.db"), map[string][]string{"mode": {"ro"}}))
		if err != nil {
			t.Fatal(err)
		}
		var rows, unpaired int
		restored.QueryRow("SELECT COUNT(*) FROM pairs").Scan(&rows)
		restored.QueryRow("SELECT COUNT(*) FROM (SELECT n FROM pairs GROUP BY n HAVING COUNT(*)<>2)").Scan(&unpaired)
		restored.Close()
		if rows%2 != 0 || unpaired != 0 {
			t.Fatalf("snapshot holds %d rows, %d unpaired", rows, unpaired)
		}
		counts = append(counts, rows)
	}
	stop.Store(true)
	wg.Wait()
	if written.Load() == 0 || counts[len(counts)-1] == 0 {
		t.Fatal("writes made no progress while snapshots were taken", written.Load(), counts)
	}
}

// A saved copy is a plain tar of the database and its key folder, named as
// they are in the data folder and owner-only, with no work files left behind.
func TestSnapshotArchiveLayout(t *testing.T) {
	d, path := openTemp(t)
	archive := saveSnapshot(t, d)
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader := tar.NewReader(file)
	var names []string
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name+" "+header.FileInfo().Mode().Perm().String())
		if header.Name == "ludock.db.identity-key/key" {
			key, _ := io.ReadAll(reader)
			if string(key) != string(d.key) {
				t.Fatal("archived key differs from the database's key")
			}
		}
	}
	want := "ludock.db -rw------- ludock.db.identity-key/ -rwx------ ludock.db.identity-key/key -rw-------"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("archive holds %q, want %q", got, want)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), snapshotPrefix) {
			t.Fatal("snapshot left its work folder", entry.Name())
		}
	}
}

// A restore puts the data back into an empty folder, where Ludock opens it
// with its own key, no saved work left to run, and no restore left to undo. It refuses a folder that already has data, an archive
// with anything else in it, a key from another installation, and a beta's copy.
func TestRestoreSnapshot(t *testing.T) {
	d, _ := openTemp(t)
	if err := d.SetSetting(context.Background(), nil, "restore-check", "kept"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO docker_hosts(id,name,created_at) VALUES('host','host',1);
    INSERT INTO logical_servers(id,host_id,external_identity,display_name,game_type,status,binding_revision,binding_fingerprint,first_seen_at,last_seen_at)
    VALUES('server','host','mc','Minecraft','minecraft','active',1,'',1,1)`); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"queued", "running", "succeeded"} {
		if _, err := d.Exec(`INSERT INTO operations(id,server_id,actor_id,kind,status,phase,input_json,binding_revision,created_at,updated_at)
      VALUES(?,'server','actor','restart',?,'queued','{}',1,1,1)`, status, status); err != nil {
			t.Fatal(err)
		}
	}
	// One restore was moving data, one had stopped partway, one was undone.
	for id, row := range map[string][2]string{"restore-running": {"running", `{"status":"replacing","roots":[]}`},
		"restore-failed": {"failed", `{"status":"rolling_back","roots":[]}`}, "restore-undone": {"failed", `{"status":"rolled_back"}`}} {
		if _, err := d.Exec(`INSERT INTO operations(id,server_id,actor_id,kind,status,phase,input_json,recovery_json,binding_revision,created_at,updated_at)
      VALUES(?,'server','actor','restore',?,'x','{}',json_object('restore',json(?),'dataUnsafe',json('true')),1,1,1)`,
			id, row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	archive := saveSnapshot(t, d)

	target := filepath.Join(t.TempDir(), "data", "ludock.db")
	if _, err := RestoreSnapshot(archive, target); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(target)
	if err != nil {
		t.Fatal("restored data doesn't open", err)
	}
	var value string
	if _, err := restored.Setting(context.Background(), "restore-check", &value); err != nil || value != "kept" {
		t.Fatal("restored data lost a setting", value, err)
	}
	// Work waiting or running when the data was saved isn't run again.
	var statuses string
	restored.QueryRow("SELECT group_concat(id||'='||status, ' ') FROM (SELECT id,status FROM operations ORDER BY id)").Scan(&statuses)
	if statuses != "queued=interrupted restore-failed=failed restore-running=interrupted restore-undone=failed running=interrupted succeeded=succeeded" {
		t.Fatal("restored operations", statuses)
	}
	// No restore's journal can be undone from the copy, and Activity says
	// where its data may be.
	var journals string
	restored.QueryRow(`SELECT group_concat(id||'='||COALESCE(json_extract(recovery_json,'$.restore.status'),'none')||
    CASE WHEN instr(COALESCE(result_json,''),'.ludock-restore-'||id) > 0 THEN ' guided' ELSE '' END, ' ')
    FROM (SELECT * FROM operations WHERE kind='restore' ORDER BY id)`).Scan(&journals)
	if journals != "restore-failed=unknown guided restore-running=unknown guided restore-undone=rolled_back" {
		t.Fatal("restored journals", journals)
	}
	restored.Close()
	if _, err := RestoreSnapshot(archive, target); !apperr.Is(err, "RESTORE_DATA_EXISTS") {
		t.Fatal("restore replaced existing data", err)
	}

	other, _ := openTemp(t)
	otherArchive := saveSnapshot(t, other)
	mixed := filepath.Join(t.TempDir(), "mixed.tar")
	writeArchive(t, mixed, map[string][]byte{"ludock.db": entryOf(t, archive, "ludock.db"),
		"ludock.db.identity-key/key": entryOf(t, otherArchive, "ludock.db.identity-key/key")})
	extra := filepath.Join(t.TempDir(), "extra.tar")
	writeArchive(t, extra, map[string][]byte{"ludock.db": entryOf(t, archive, "ludock.db"),
		"ludock.db.identity-key/key": entryOf(t, archive, "ludock.db.identity-key/key"), "../escape": []byte("x")})
	// A beta's copy has the same layout but its database carries the beta's marker.
	betaDB := filepath.Join(t.TempDir(), "beta.db")
	os.WriteFile(betaDB, entryOf(t, archive, "ludock.db"), 0o600)
	raw, _ := sql.Open("sqlite", betaDB)
	if _, err := raw.Exec("PRAGMA application_id = 1279542091"); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	betaDBBytes, _ := os.ReadFile(betaDB)
	beta := filepath.Join(t.TempDir(), "beta.tar")
	writeArchive(t, beta, map[string][]byte{"ludock.db": betaDBBytes, "ludock.db.identity-key/key": entryOf(t, archive, "ludock.db.identity-key/key")})
	for name, test := range map[string]struct{ archive, code string }{
		"another key": {mixed, "RESTORE_ARCHIVE"}, "an extra entry": {extra, "RESTORE_ARCHIVE"}, "a beta's copy": {beta, "RESTORE_BETA"},
	} {
		folder := t.TempDir()
		if _, err := RestoreSnapshot(test.archive, filepath.Join(folder, "ludock.db")); !apperr.Is(err, test.code) {
			t.Fatalf("restore accepted %s: %v", name, err)
		}
		if entries, _ := os.ReadDir(folder); len(entries) != 0 {
			t.Fatalf("refused restore of %s left %d files", name, len(entries))
		}
	}
}

func entryOf(t *testing.T, archive, name string) []byte {
	t.Helper()
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err != nil {
			t.Fatal(name, err)
		}
		if header.Name == name {
			data, _ := io.ReadAll(reader)
			return data
		}
	}
}

func writeArchive(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer := tar.NewWriter(file)
	for _, name := range []string{"ludock.db", "ludock.db.identity-key/key", "../escape"} {
		if data, ok := files[name]; ok {
			writer.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Size: int64(len(data))})
			writer.Write(data)
		}
	}
	writer.Close()
}
