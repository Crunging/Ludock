package helper

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const stageID = "0f8fad5b-d9cb-469f-a165-70867728950e"

type archived struct {
	name, body string
	flag       byte
	link       string
}

func tarArchive(t *testing.T, entries []archived) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Typeflag: entry.flag, Mode: 0o4777, Size: int64(len(entry.body)), Linkname: entry.link,
			Uid: 12345, Gid: 12345}
		if header.Typeflag == 0 {
			header.Typeflag = tar.TypeReg
		}
		if header.Typeflag != tar.TypeReg {
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		writer.Write([]byte(entry.body))
	}
	writer.Close()
	return buffer.Bytes()
}

func gzipped(data []byte) []byte {
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	writer.Write(data)
	writer.Close()
	return buffer.Bytes()
}

func zipArchive(t *testing.T, entries []archived) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.flag == tar.TypeSymlink {
			header.SetMode(os.ModeSymlink | 0o777)
		}
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		file.Write([]byte(entry.body + entry.link))
	}
	writer.Close()
	return buffer.Bytes()
}

func unpackFile(t *testing.T, root, name string, data []byte) (UnpackResult, int) {
	t.Helper()
	os.WriteFile(filepath.Join(root, name), data, 0o644)
	reserve := int64(0)
	out, code := runFile(t, FileRequest{Operation: "unpack", Root: root, Path: name, UploadID: stageID, Reserve: &reserve}, nil)
	var result UnpackResult
	if code == 0 {
		json.Unmarshal([]byte(out), &result)
	}
	return result, code
}

// listTree lists every path under root, so a test can see what an unpack
// left behind.
func listTree(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if path != root {
			relative, _ := filepath.Rel(root, path)
			paths = append(paths, relative)
		}
		return nil
	})
	return paths
}

// Entry names map to components under the new folder; anything absolute,
// climbing, reserved, or that a backup would refuse is unsafe.
func TestArchiveEntryPaths(t *testing.T) {
	for name, want := range map[string][]string{"world/level.dat": {"world", "level.dat"}, "./world//region/": {"world", "region"},
		"a/./b": {"a", "b"}, "ünïcode name.txt": {"ünïcode name.txt"}} {
		if got, err := archiveParts(name, 64); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %v %v", name, got, err)
		}
	}
	for _, name := range []string{"", "/etc/passwd", "../escape", "world/../../escape", "a\\..\\b", "a\x00b", "line\nbreak", "caf\xe9.txt",
		".ludock-restore-1/x", strings.Repeat("a", 256), strings.Repeat("a/", 2100)} {
		if _, err := archiveParts(name, 64); !errors.Is(err, errArchiveUnsafe) {
			t.Errorf("%q: %v", name, err)
		}
	}
	if _, err := archiveParts(strings.Repeat("a/", 65)+"b", 64); !errors.Is(err, errArchiveLimit) {
		t.Fatal("depth", err)
	}
	for archive, want := range map[string]string{"World.ZIP": "World", "pack.tar.gz": "pack", "pack.tgz": "pack", "x.mcworld": "x",
		"data": "data files", ".zip": ".zip files"} {
		if got := unpackBase(archive); got != want {
			t.Errorf("%q: %q", archive, got)
		}
	}
}

// An archive with one top folder becomes that folder; others get a folder
// named after the archive. Nothing existing is replaced, links and special
// entries are left out, and new files are owned and permitted like uploads.
func TestUnpackFormatsAndNames(t *testing.T) {
	root, _ := dataRoot(t)
	world := []archived{{name: "world/", flag: tar.TypeDir}, {name: "world/level.dat", body: "new level"},
		{name: "world/region/r.0.0.mca", body: "region"}}
	result, code := unpackFile(t, root, "world.zip", zipArchive(t, world))
	if code != 0 || result.Name != "world (2)" || result.Files != 2 {
		t.Fatal(result, code)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "world", "level.dat")); string(data) != "level" {
		t.Fatal("replaced an existing world")
	}
	if data, _ := os.ReadFile(filepath.Join(root, "world (2)", "region", "r.0.0.mca")); string(data) != "region" {
		t.Fatal("not unpacked")
	}
	pack := []archived{{name: "a.jar", body: "a"}, {name: "configs/b.yml", body: "b"}, {name: "link", flag: tar.TypeSymlink, link: "/etc"},
		{name: "hard", flag: tar.TypeLink, link: "a.jar"}, {name: "fifo", flag: tar.TypeFifo}}
	for name, data := range map[string][]byte{"plugins.tar.gz": gzipped(tarArchive(t, pack)), "plugins.tar": tarArchive(t, pack),
		"bundle": zipArchive(t, pack[:3])} {
		result, code := unpackFile(t, root, name, data)
		if code != 0 || result.Files != 2 || (result.Skipped != 3 && name != "bundle") {
			t.Fatal(name, result, code)
		}
		folder := filepath.Join(root, result.Name)
		if got := listTree(t, folder); !reflect.DeepEqual(got, []string{"a.jar", "configs", "configs/b.yml"}) {
			t.Fatal(name, got)
		}
		info, _ := os.Stat(filepath.Join(folder, "a.jar"))
		parent, _ := os.Stat(root)
		stat := info.Sys().(*syscall.Stat_t)
		if info.Mode().Perm()&0o111 != 0 || info.Mode()&os.ModeSetuid != 0 || stat.Uid != parent.Sys().(*syscall.Stat_t).Uid {
			t.Fatal(name, info.Mode())
		}
	}
	if _, err := os.Stat(filepath.Join(root, "bundle files")); err != nil {
		t.Fatal("a zip without an extension unpacks by content", listTree(t, root))
	}
	// Deep enough that the archive's own folder name would put a file past
	// MaxPath, the folder is named after the archive instead.
	deep := strings.Repeat(strings.Repeat("d", 255)+"/", 15) + strings.Repeat("d", 160)
	os.MkdirAll(filepath.Join(root, deep), 0o755)
	long := strings.Repeat("m", 250)
	if result, code := unpackFile(t, root, deep+"/w.tar", tarArchive(t, []archived{{name: long + "/", flag: tar.TypeDir}, {name: long + "/a", body: "a"}})); code != 0 || result.Name != "w" {
		t.Fatal(result, code)
	}
	// One whose files would pass it even so is refused, and leaves nothing.
	deeper := long + "/" + strings.Repeat("f", 100)
	if _, code := unpackFile(t, root, deep+"/x.tar", tarArchive(t, []archived{{name: long + "/", flag: tar.TypeDir}, {name: deeper, body: "f"}})); code != FileArchiveDeep {
		t.Fatal("unpacked files Files can't open", code)
	}
	if got := listTree(t, filepath.Join(root, deep)); !reflect.DeepEqual(got, []string{"w", "w/a", "w.tar", "x.tar"}) {
		t.Fatal(got)
	}
}

// Hostile archives write nothing outside, or at all, and leave no staging
// folder behind.
func TestUnpackRefusesHostileArchives(t *testing.T) {
	root, outside := dataRoot(t)
	before := listTree(t, filepath.Dir(root))
	cases := map[string]struct {
		data []byte
		code int
	}{
		"climb.tar":     {tarArchive(t, []archived{{name: "fine.txt", body: "x"}, {name: "../../escape.txt", body: "x"}}), FileArchiveUnsafe},
		"absolute.zip":  {zipArchive(t, []archived{{name: "/tmp/escape.txt", body: "x"}}), FileArchiveUnsafe},
		"twice.tar":     {tarArchive(t, []archived{{name: "a", body: "1"}, {name: "a", body: "2"}}), FileArchiveUnsafe},
		"file-dir.tar":  {tarArchive(t, []archived{{name: "a", body: "1"}, {name: "a/b", body: "2"}}), FileArchiveUnsafe},
		"text.zip":      {[]byte("not an archive"), FileNotArchive},
		"truncated.tgz": {gzipped(tarArchive(t, []archived{{name: "a", body: strings.Repeat("x", 4096)}}))[:60], FileNotArchive},
		// The tar ends before the gzip trailer, which is cut off.
		"trailer.tgz": {func() []byte {
			data := gzipped(tarArchive(t, []archived{{name: "a", body: "x"}}))
			return data[:len(data)-4]
		}(), FileNotArchive},
	}
	for name, test := range cases {
		if _, code := unpackFile(t, root, name, test.data); code != test.code {
			t.Errorf("%s: %d", name, code)
		}
		os.Remove(filepath.Join(root, name))
	}
	if after := listTree(t, filepath.Dir(root)); !reflect.DeepEqual(after, before) {
		t.Fatal(after)
	}
	// A link entry is left out, so a later entry under its name is an
	// ordinary folder rather than a path through the link.
	through := tarArchive(t, []archived{{name: "out", flag: tar.TypeSymlink, link: outside}, {name: "out/planted", body: "x"}})
	result, code := unpackFile(t, root, "through.tar", through)
	if code != 0 || result.Skipped != 1 || result.Name != "out" {
		t.Fatal(result, code)
	}
	if info, err := os.Lstat(filepath.Join(root, "out", "planted")); err != nil || !info.Mode().IsRegular() {
		t.Fatal(info, err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Fatal("wrote outside")
	}
}

// Each limit is checked while unpacking and removes the partial result.
func TestUnpackLimits(t *testing.T) {
	root, _ := dataRoot(t)
	saved := Unpack
	t.Cleanup(func() { Unpack = saved })
	Unpack = UnpackLimits{Bytes: 1 << 20, Entries: 5, Depth: 3, Ratio: 100, Floor: 64 << 10}
	zeros := strings.Repeat("\x00", 512<<10)
	cases := map[string][]byte{
		"ratio.tar.gz": gzipped(tarArchive(t, []archived{{name: "zeros", body: zeros}})),
		"ratio.zip":    zipArchive(t, []archived{{name: "zeros", body: zeros}}),
		"total.tar":    tarArchive(t, []archived{{name: "a", body: zeros}, {name: "b", body: zeros}, {name: "c", body: "x"}}),
		"entries.zip":  zipArchive(t, []archived{{name: "1"}, {name: "2"}, {name: "3"}, {name: "4"}, {name: "5"}, {name: "6"}}),
		"entries.tar":  tarArchive(t, []archived{{name: "1"}, {name: "2"}, {name: "3"}, {name: "4"}, {name: "5"}, {name: "6"}}),
		"depth.tar":    tarArchive(t, []archived{{name: "a/b/c/d", body: "x"}}),
		"implied.tar":  tarArchive(t, []archived{{name: "a/b/x", body: "x"}, {name: "c/d/y", body: "y"}}),
	}
	before := listTree(t, root)
	for name, data := range cases {
		if _, code := unpackFile(t, root, name, data); code != FileArchiveLimit {
			t.Errorf("%s: %d", name, code)
		}
		os.Remove(filepath.Join(root, name))
	}
	if after := listTree(t, root); !reflect.DeepEqual(after, before) {
		t.Fatal(after)
	}
	// The reserve stops an unpack that would leave less free space.
	statfs = func(fd int, stat *unix.Statfs_t) error {
		*stat = unix.Statfs_t{Bsize: 4096, Frsize: 4096, Bavail: 100}
		return nil
	}
	t.Cleanup(func() { statfs = unix.Fstatfs })
	if _, code := unpackFile(t, root, "small.tar", tarArchive(t, []archived{{name: "a", body: "x"}})); code != FileArchiveFull {
		t.Fatal(code)
	}
	if after := listTree(t, root); !slices.Contains(after, "small.tar") || len(after) != len(before)+1 {
		t.Fatal(after)
	}
}

// Cleanup removes only the staging folder it is named, so a look-alike Ludock
// never recorded is left even when it looks like an old one of Ludock's.
func TestUnpackStagingCleanup(t *testing.T) {
	root, _ := dataRoot(t)
	fresh := filepath.Join(root, ".ludock-unpack-"+stageID)
	os.MkdirAll(filepath.Join(fresh, "half"), 0o755)
	os.Chmod(fresh, 0o700)
	planted := filepath.Join(root, ".ludock-unpack-7c9e6679-7425-40de-944b-e07fc1f90ae7")
	os.MkdirAll(filepath.Join(planted, "game data"), 0o755)
	os.Chmod(planted, 0o700)
	old := unix.NsecToTimespec(0)
	unix.UtimesNano(planted, []unix.Timespec{old, old})
	if _, code := unpackFile(t, root, "other.tar", tarArchive(t, []archived{{name: "x", body: "x"}})); code == 0 {
		t.Fatal("reused a staging name in use")
	}
	if _, err := os.Stat(filepath.Join(planted, "game data")); err != nil {
		t.Fatal("an unpack removed a folder Ludock didn't record")
	}
	if _, code := runFile(t, FileRequest{Operation: "unpack-cleanup", Root: root, Path: "other.tar", UploadID: stageID}, nil); code != 0 {
		t.Fatal(code)
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatal("cleanup kept the staging folder")
	}
	if _, err := os.Stat(filepath.Join(planted, "game data")); err != nil {
		t.Fatal("cleanup removed a folder it wasn't named")
	}
	// A folder that is gone took its staging folder with it.
	if _, code := runFile(t, FileRequest{Operation: "unpack-cleanup", Root: root, Path: "gone/other.tar", UploadID: stageID}, nil); code != 0 {
		t.Fatal(code)
	}
	// Files an excluded mount hides from the game are never removed.
	os.MkdirAll(filepath.Join(fresh, "hidden"), 0o755)
	os.Chmod(fresh, 0o700)
	if _, code := runFile(t, FileRequest{Operation: "unpack-cleanup", Root: root, Path: "other.tar", UploadID: stageID,
		Blocked: []string{fresh + "/hidden"}}, nil); code == 0 {
		t.Fatal("removed a staging folder over an excluded mount")
	}
	if _, err := os.Stat(filepath.Join(fresh, "hidden")); err != nil {
		t.Fatal(err)
	}
}

// Selections act on entries of one folder; a move never replaces a name,
// moves a link or a mount, or puts a folder inside itself.
func TestSelectionDeleteAndMove(t *testing.T) {
	root, _ := dataRoot(t)
	os.MkdirAll(filepath.Join(root, "logs", "old"), 0o755)
	os.MkdirAll(filepath.Join(root, "mounted"), 0o755)
	for _, name := range []string{"a.log", "b.log", "c.log"} {
		os.WriteFile(filepath.Join(root, "logs", name), []byte(name), 0o644)
	}
	move := func(folder string, names []string, destination string, mounts ...string) int {
		_, code := runFile(t, FileRequest{Operation: "move", Root: root, Path: folder, Paths: names, Destination: destination, Mounts: mounts}, nil)
		return code
	}
	for name, code := range map[string]int{
		"another folder": move("logs", []string{"a.log", "../server.properties"}, "world"),
		"a path":         move("", []string{"logs/a.log"}, "world"),
		"into itself":    move("logs", []string{"old"}, "logs/old"),
		"a link":         move("", []string{"escape"}, "world"),
		"a mount":        move("", []string{"mounted"}, "world", root+"/mounted"),
		"into a mount":   move("logs", []string{"a.log"}, "mounted", root+"/mounted"),
		"twice":          move("logs", []string{"a.log", "a.log"}, "world"),
	} {
		if code == 0 {
			t.Errorf("%s moved", name)
		}
	}
	os.WriteFile(filepath.Join(root, "logs", "old", "b.log"), nil, 0o644)
	if code := move("logs", []string{"a.log", "b.log"}, "logs/old"); code != FileNameTaken {
		t.Fatal(code)
	}
	if _, err := os.Stat(filepath.Join(root, "logs", "a.log")); err != nil {
		t.Fatal("a refused move changed something")
	}
	os.Remove(filepath.Join(root, "logs", "old", "b.log"))
	if code := move("logs", []string{"a.log", "b.log"}, "logs/old"); code != 0 {
		t.Fatal(code)
	}
	if got := listTree(t, filepath.Join(root, "logs")); !reflect.DeepEqual(got, []string{"c.log", "old", "old/a.log", "old/b.log"}) {
		t.Fatal(got)
	}
	if _, code := runFile(t, FileRequest{Operation: "delete-paths", Root: root, Path: "logs", Paths: []string{"c.log", "missing"}}, nil); code == 0 {
		t.Fatal("deleted with a missing entry")
	}
	if _, code := runFile(t, FileRequest{Operation: "delete-paths", Root: root, Path: "logs", Paths: []string{"c.log", "old"}}, nil); code != 0 {
		t.Fatal(code)
	}
	if got := listTree(t, filepath.Join(root, "logs")); len(got) != 0 {
		t.Fatal(got)
	}
	out, code := runFile(t, FileRequest{Operation: "download-paths", Root: root, Paths: []string{"world", "server.properties"}}, nil)
	if code != 0 || !strings.HasPrefix(out, `{"size":0,"type":"directory"}`+"\n") {
		t.Fatal(code, out[:min(len(out), 80)])
	}
	reader := tar.NewReader(strings.NewReader(out[strings.IndexByte(out, '\n')+1:]))
	var names []string
	for header, err := reader.Next(); err == nil; header, err = reader.Next() {
		names = append(names, header.Name)
	}
	if !reflect.DeepEqual(names, []string{"world/", "world/level.dat", "world/region/", "server.properties"}) &&
		!reflect.DeepEqual(names, []string{"world/", "world/region/", "world/level.dat", "server.properties"}) {
		t.Fatal(names)
	}
}
