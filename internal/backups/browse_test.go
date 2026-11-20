package backups

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/tarstream"
)

// writeArchive publishes an archive of entries (a trailing slash marks a
// folder) and returns its checksum.
func writeArchive(t *testing.T, directory, id string, entries map[string]string, order []string) string {
	t.Helper()
	var archive bytes.Buffer
	for _, name := range order {
		header := tarstream.Header{Name: name, Type: "file", Mode: 0o644, MtimeSeconds: 1700000000, Size: int64(len(entries[name]))}
		if strings.HasSuffix(name, "/") {
			header.Type, header.Mode, header.Size = "directory", 0o755, 0
		}
		encoded, err := tarstream.EncodeHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		archive.Write(encoded)
		archive.WriteString(entries[name])
		archive.Write(tarstream.Padding(header.Size))
	}
	archive.Write(tarstream.End())
	if err := os.WriteFile(filepath.Join(directory, id+".tar"), archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive.Bytes())
	return hex.EncodeToString(sum[:])
}

// A backup's folders list their direct items with folder totals, one file
// reads back on its own, and an item's staged size counts only what it holds.
func TestListFolderAndReadFile(t *testing.T) {
	directory := t.TempDir()
	storage := &Storage{Roots: directory}
	id := "6f9619ff-8b86-4011-b42d-00c04fc964ff"
	roots := []Root{{ID: "root-0", Path: "/data"}}
	big := strings.Repeat("x", 70000)
	entries := map[string]string{"snapshot/root-0/world/level.dat": "level", "snapshot/root-0/world/region/r.0.mca": big,
		"snapshot/root-0/server.properties": "motd=hi"}
	order := []string{"snapshot/", "snapshot/root-0/", "snapshot/root-0/world/", "snapshot/root-0/world/region/",
		"snapshot/root-0/world/region/r.0.mca", "snapshot/root-0/world/level.dat", "snapshot/root-0/server.properties"}
	checksum := writeArchive(t, directory, id, entries, order)

	top, err := storage.ListFolder(directory, id, roots, "root-0", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 || top[0].Name != "world" || top[0].Type != "directory" || top[0].Size != int64(len(big)+5) || top[0].Items != 3 ||
		top[1].Name != "server.properties" || top[1].Size != 7 || top[1].ModifiedAt != 1700000000 {
		t.Fatalf("%+v", top)
	}
	world, err := storage.ListFolder(directory, id, roots, "root-0", "world")
	if err != nil || len(world) != 2 || world[0].Name != "region" || world[1].Name != "level.dat" {
		t.Fatalf("%+v %v", world, err)
	}
	for _, folder := range []string{"world/level.dat", "missing", "../world", "world/", "/world", ".ludock-restore-x"} {
		if _, err := storage.ListFolder(directory, id, roots, "root-0", folder); err == nil {
			t.Fatal("listed", folder)
		}
	}
	if _, err := storage.ListFolder(directory, id, roots, "root-1", ""); !apperr.Is(err, "BACKUP_PATH_NOT_FOUND") {
		t.Fatal(err)
	}

	var read []byte
	err = storage.ReadFile(directory, id, roots, "root-0", "world/region/r.0.mca", func(_ int64, body io.Reader) error {
		read, err = io.ReadAll(body)
		return err
	})
	if err != nil || string(read) != big {
		t.Fatal(len(read), err)
	}
	if err := storage.ReadFile(directory, id, roots, "root-0", "world", func(int64, io.Reader) error { return nil }); !apperr.Is(err, "BACKUP_PATH_NOT_FOUND") {
		t.Fatal("read a folder as a file:", err)
	}

	staged, err := storage.ValidateItem(directory, id, roots, 1<<30, checksum, "root-0", "world/region", nil)
	if err != nil || staged != 4096+73728 {
		t.Fatal(staged, err)
	}
	if _, err := storage.ValidateItem(directory, id, roots, 1<<30, strings.Repeat("0", 64), "root-0", "world", nil); !apperr.Is(err, "BACKUP_CHECKSUM") {
		t.Fatal(err)
	}
	if _, err := storage.ValidateItem(directory, id, roots, 1<<30, checksum, "root-0", "", nil); !apperr.Is(err, "UNSAFE_PATH") {
		t.Fatal("accepted a whole root as an item:", err)
	}
}

// Truncating an archive inside a skipped file body still fails the listing.
func TestListFolderTruncatedArchive(t *testing.T) {
	directory := t.TempDir()
	storage := &Storage{Roots: directory}
	id := "6f9619ff-8b86-4011-b42d-00c04fc964ff"
	writeArchive(t, directory, id, map[string]string{"snapshot/root-0/big": strings.Repeat("x", 200000)},
		[]string{"snapshot/", "snapshot/root-0/", "snapshot/root-0/big"})
	path := filepath.Join(directory, id+".tar")
	data, _ := os.ReadFile(path)
	os.WriteFile(path, data[:100000], 0o600)
	if _, err := storage.ListFolder(directory, id, []Root{{ID: "root-0", Path: "/data"}}, "root-0", ""); !apperr.Is(err, "INVALID_BACKUP") {
		t.Fatal(err)
	}
}
