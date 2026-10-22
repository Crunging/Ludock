package helper

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func runFile(t *testing.T, request FileRequest, stdin []byte) (string, int) {
	t.Helper()
	argument, _ := json.Marshal(request)
	var stdout, stderr bytes.Buffer
	code := RunFile(string(argument), bytes.NewReader(stdin), &stdout, &stderr)
	if code != 0 && stderr.String() != FileFailure {
		t.Fatalf("stderr %q", stderr.String())
	}
	if code != 0 && strings.Contains(stdout.String()+stderr.String(), request.Root) {
		t.Fatal("failure leaked a path")
	}
	return stdout.String(), code
}

func dataRoot(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "data")
	os.MkdirAll(filepath.Join(root, "world", "region"), 0o755)
	os.WriteFile(filepath.Join(root, "world", "level.dat"), []byte("level"), 0o644)
	os.WriteFile(filepath.Join(root, "server.properties"), []byte("motd=hi\n"), 0o640)
	os.MkdirAll(filepath.Join(root, "private"), 0o755)
	outside := filepath.Join(base, "outside")
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600)
	os.Symlink(outside, filepath.Join(root, "escape"))
	return root, outside
}

func TestListHidesBlockedAndReportsSymlinks(t *testing.T) {
	root, _ := dataRoot(t)
	out, code := runFile(t, FileRequest{Operation: "list", Root: root, Blocked: []string{root + "/private"}}, nil)
	if code != 0 {
		t.Fatal(code)
	}
	var entries []FileEntry
	json.Unmarshal([]byte(out), &entries)
	types := map[string]string{}
	for _, entry := range entries {
		types[entry.Name] = entry.Type
	}
	if types["private"] != "" || types["escape"] != "symlink" || types["world"] != "directory" || types["server.properties"] != "file" {
		t.Fatal(types)
	}
}

func TestPathsNeverLeaveTheRoot(t *testing.T) {
	root, _ := dataRoot(t)
	for _, request := range []FileRequest{
		{Operation: "list", Root: root, Path: "escape"},
		{Operation: "stat", Root: root, Path: "escape/secret"},
		{Operation: "download", Root: root, Path: "escape/secret"},
		{Operation: "list", Root: root, Path: "../outside"},
		{Operation: "delete", Root: root, Path: "private/x", Blocked: []string{root + "/private"}},
		{Operation: "delete", Root: root, Path: "", Blocked: nil},
		{Operation: "rename", Root: root, Path: "world", Destination: "private/world", Blocked: []string{root + "/private"}},
		{Operation: "check", Root: "/"},
		{Operation: "check", Root: root + "/escape"},
	} {
		if _, code := runFile(t, request, nil); code == 0 {
			t.Errorf("%+v succeeded", request)
		}
	}
	// A symlink can be deleted (the link itself), never followed.
	if _, code := runFile(t, FileRequest{Operation: "rename", Root: root, Path: "escape", Destination: "moved"}, nil); code == 0 {
		t.Fatal("renamed a symlink")
	}
}

func TestMutationsAndUploads(t *testing.T) {
	root, outside := dataRoot(t)
	if _, code := runFile(t, FileRequest{Operation: "mkdir", Root: root, Path: "world/new"}, nil); code != 0 {
		t.Fatal("mkdir")
	}
	if _, code := runFile(t, FileRequest{Operation: "rename", Root: root, Path: "world/new", Destination: "world/region"}, nil); code == 0 {
		t.Fatal("rename replaced an existing entry")
	}
	if _, code := runFile(t, FileRequest{Operation: "rename", Root: root, Path: "world/new", Destination: "renamed"}, nil); code != 0 {
		t.Fatal("rename")
	}
	if _, code := runFile(t, FileRequest{Operation: "delete", Root: root, Path: "escape"}, nil); code != 0 {
		t.Fatal("delete symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "secret")); err != nil {
		t.Fatal("symlink target deleted")
	}
	if _, code := runFile(t, FileRequest{Operation: "delete", Root: root, Path: "world"}, nil); code != 0 {
		t.Fatal("delete tree")
	}
	id := "6f9619ff-8b86-4011-b42d-00c04fc964ff"
	size := int64(5)
	upload := FileRequest{Operation: "upload", Root: root, Path: "server.properties", Size: &size, UploadID: id}
	if _, code := runFile(t, upload, []byte("hello"+id)); code != 0 {
		t.Fatal("upload")
	}
	if data, _ := os.ReadFile(filepath.Join(root, "server.properties")); string(data) != "hello" {
		t.Fatal(string(data))
	}
	if info, _ := os.Stat(filepath.Join(root, "server.properties")); info.Mode().Perm() != 0o640 {
		t.Fatal("upload changed the mode", info.Mode())
	}
	for _, body := range []string{"hell", "hello" + id[:10], "hello!" + id, "helloXX"} {
		if _, code := runFile(t, upload, []byte(body)); code == 0 {
			t.Errorf("accepted %q", body)
		}
		if data, _ := os.ReadFile(filepath.Join(root, "server.properties")); string(data) != "hello" {
			t.Fatal("a failed upload replaced the file")
		}
	}
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".ludock-upload-") {
			t.Fatal("staging file left behind")
		}
	}
	created := FileRequest{Operation: "upload", Root: root, Path: "new.txt", Size: &size, UploadID: id}
	if _, code := runFile(t, created, []byte("12345"+id)); code != 0 {
		t.Fatal("new upload")
	}
	// A hard link to outside data is refused.
	os.Link(filepath.Join(outside, "secret"), filepath.Join(root, "linked"))
	if _, code := runFile(t, FileRequest{Operation: "upload", Root: root, Path: "linked", Size: &size, UploadID: id}, []byte("12345"+id)); code == 0 {
		t.Fatal("wrote through a hard link")
	}
	if _, code := runFile(t, FileRequest{Operation: "download", Root: root, Path: "linked"}, nil); code == 0 {
		t.Fatal("downloaded a hard link")
	}
}

func TestDownloadsAndBackupsAreReadableTar(t *testing.T) {
	root, _ := dataRoot(t)
	os.Remove(filepath.Join(root, "escape"))
	long := strings.Repeat("ü", 80)
	os.WriteFile(filepath.Join(root, "world", long), []byte("unicode"), 0o644)
	for _, request := range []FileRequest{{Operation: "download", Root: root, Path: "world"}, {Operation: "backup", Root: root}} {
		out, code := runFile(t, request, nil)
		if code != 0 {
			t.Fatal(request.Operation)
		}
		reader := tar.NewReader(strings.NewReader(out))
		found := map[string]string{}
		for {
			header, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(request.Operation, err)
			}
			data, _ := io.ReadAll(reader)
			found[header.Name] = string(data)
			if request.Operation == "download" && (header.Uid != 0 || header.Gid != 0) {
				t.Fatal("download kept ownership")
			}
			if request.Operation == "backup" && header.Uid != os.Getuid() {
				t.Fatal("backup lost ownership")
			}
		}
		prefix := "world/"
		if request.Operation == "backup" {
			prefix = "source/world/"
			if _, ok := found["source/"]; !ok {
				t.Fatal("backup root entry missing")
			}
		}
		if found[prefix+"level.dat"] != "level" || found[prefix+long] != "unicode" {
			t.Fatalf("%s: %v", request.Operation, keys(found))
		}
	}
	out, _ := runFile(t, FileRequest{Operation: "download", Root: root, Path: "world/level.dat"}, nil)
	if out != "level" {
		t.Fatal(out)
	}
	stat, _ := runFile(t, FileRequest{Operation: "stat", Root: root, Path: "world"}, nil)
	if stat != `{"size":0,"type":"directory"}` {
		t.Fatal(stat)
	}
}

func keys(m map[string]string) []string {
	var result []string
	for key := range m {
		result = append(result, key)
	}
	return result
}

func TestTarHeaderLimits(t *testing.T) {
	if _, err := tarHeader(strings.Repeat("a", 4097), tarInfo{}, false, 0, false); err == nil {
		t.Fatal("accepted an overlong name")
	}
	// Sizes too large for octal use base-256.
	header, err := tarHeader("big", tarInfo{size: 1 << 40}, false, 0, false)
	if err != nil || header[124] != 128 {
		t.Fatal(err)
	}
	prefixed, _ := tarHeader(strings.Repeat("d", 120)+"/file", tarInfo{}, false, 0, false)
	if len(prefixed) != 512 || string(bytes.TrimRight(prefixed[345:500], "\x00")) != strings.Repeat("d", 120) {
		t.Fatal("ustar prefix not used")
	}
}

func restore(t *testing.T, root, stage, operation string) (string, int) {
	t.Helper()
	argument, _ := json.Marshal(RestoreRequest{Root: root, Stage: stage, Operation: operation})
	var stdout, stderr bytes.Buffer
	code := RunRestore(string(argument), &stdout, &stderr)
	if code != 0 && stderr.String() != RestoreFailure {
		t.Fatal(stderr.String())
	}
	return stdout.String(), code
}

func TestRestoreSteps(t *testing.T) {
	root, _ := dataRoot(t)
	os.Remove(filepath.Join(root, "escape"))
	stage := ".ludock-restore-6f9619ff-8b86-4011-b42d-00c04fc964ff"
	if out, code := restore(t, root, stage, "space"); code != 0 || !strings.Contains(out, "availableBytes") {
		t.Fatal(out)
	}
	if _, code := restore(t, root, ".ludock-restore-bad", "stage"); code == 0 {
		t.Fatal("accepted a bad stage")
	}
	if _, code := restore(t, root, stage, "stage"); code != 0 {
		t.Fatal("stage")
	}
	records := []string{
		`{"name":"world","type":"directory","size":0,"uid":` + strconv.Itoa(os.Getuid()) + `,"gid":` + strconv.Itoa(os.Getgid()) + `,"mode":493,"mtime":1700000000}`,
		`{"name":"world/level.dat","type":"file","size":8,"uid":` + strconv.Itoa(os.Getuid()) + `,"gid":` + strconv.Itoa(os.Getgid()) + `,"mode":420,"mtime":1700000000.5}`,
	}
	input := records[0] + "\n" + records[1] + "\nrestored"
	argument, _ := json.Marshal(map[string]any{"root": root, "stage": stage, "maxBytes": 100, "reserveBytes": 0})
	var stdout, stderr bytes.Buffer
	if code := RunRestoreExtract(string(argument), strings.NewReader(input), &stdout, &stderr); code != 0 || stdout.String() != "ok" {
		t.Fatal(stderr.String())
	}
	info, _ := os.Stat(filepath.Join(root, stage, "new", "world"))
	if info.Mode().Perm() != 0o755 || info.ModTime().Unix() != 1700000000 {
		t.Fatal(info.Mode(), info.ModTime())
	}
	for _, unsafe := range []string{
		`{"name":"../x","type":"file","size":0,"uid":0,"gid":0,"mode":420}`,
		`{"name":".ludock-restore-x/y","type":"file","size":0,"uid":0,"gid":0,"mode":420}`,
		`{"name":"big","type":"file","size":1000,"uid":0,"gid":0,"mode":420}`,
		`{"name":"world/level.dat","type":"file","size":1,"uid":0,"gid":0,"mode":420}` + "\nx",
		`{"name":"short","type":"file","size":10,"uid":0,"gid":0,"mode":420}` + "\nabc",
	} {
		var stderr bytes.Buffer
		if code := RunRestoreExtract(string(argument), strings.NewReader(unsafe+"\n"), io.Discard, &stderr); code == 0 {
			t.Errorf("accepted %s", unsafe)
		}
	}
	for _, step := range []string{"moveOld", "moveNew"} {
		if _, code := restore(t, root, stage, step); code != 0 {
			t.Fatal(step)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(root, "world", "level.dat")); string(data) != "restored" {
		t.Fatal(string(data))
	}
	if _, err := os.Stat(filepath.Join(root, stage, "old", "server.properties")); err != nil {
		t.Fatal("old data not kept")
	}
	// Roll back: remove the replacement and return the old data.
	for _, step := range []string{"rollbackClean", "rollbackOld"} {
		if _, code := restore(t, root, stage, step); code != 0 {
			t.Fatal(step)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(root, "world", "level.dat")); string(data) != "level" {
		t.Fatal(string(data))
	}
	if _, code := restore(t, root, stage, "cleanup"); code != 0 {
		t.Fatal("cleanup")
	}
	if _, err := os.Stat(filepath.Join(root, stage)); !os.IsNotExist(err) {
		t.Fatal("stage left behind")
	}
	if _, code := restore(t, root, stage, "cleanup"); code != 0 {
		t.Fatal("cleanup is idempotent")
	}
	// Special files and hard links are never moved.
	restore(t, root, stage, "stage")
	os.Link(filepath.Join(root, "server.properties"), filepath.Join(root, "hardlink"))
	if _, code := restore(t, root, stage, "moveOld"); code == 0 {
		t.Fatal("moved a hard link")
	}
}

func TestMountProofAndIdentities(t *testing.T) {
	host := t.TempDir()
	os.MkdirAll(filepath.Join(host, "srv", "game"), 0o755)
	os.Symlink(filepath.Join(host, "srv", "game"), filepath.Join(host, "srv", "link"))
	HostRoot = host
	defer func() { HostRoot = "/host" }()
	var out bytes.Buffer
	if code := RunMountProof(`[{"source":"/srv/game","destination":"/data"}]`, "1", &out); code != 0 {
		t.Fatal(out.String())
	}
	var result struct{ Identities map[string]MountIdentity }
	json.Unmarshal(out.Bytes(), &result)
	if result.Identities["/data"].Ino == "" {
		t.Fatal(out.String())
	}
	out.Reset()
	if code := RunMountProof(`[{"source":"/srv/link","destination":"/data"}]`, "1", &out); code == 0 || out.String() != `{"error":"unsafe-source"}`+"\n" {
		t.Fatal("followed a symlinked source")
	}
	// Identity checks walk from /, so check a real directory's identity.
	actual := filepath.Join(host, "srv", "game")
	ok, _ := json.Marshal(map[string]MountIdentity{actual: result.Identities["/data"]})
	if RunMountIdentities(string(ok)) != 0 {
		t.Fatal("matching identity rejected")
	}
	wrong, _ := json.Marshal(map[string]MountIdentity{filepath.Join(host, "srv"): result.Identities["/data"]})
	if RunMountIdentities(string(wrong)) == 0 {
		t.Fatal("mismatched identity accepted")
	}
}

func TestRenameWithoutNoReplaceNeverClobbers(t *testing.T) {
	// Act like a kernel or filesystem without RENAME_NOREPLACE.
	original := renameat2
	renameat2 = func(int, string, int, string, uint) error { return unix.ENOSYS }
	t.Cleanup(func() { renameat2 = original })
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(dir, "taken.txt"), []byte("game data"), 0o644)
	os.Mkdir(filepath.Join(dir, "folder"), 0o755)
	parentFD, err := unix.Open(dir, unix.O_DIRECTORY|unix.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(parentFD)
	parent := fd(parentFD)
	stat := func(name string) *unix.Stat_t {
		info, err := lstatAt(parent, name)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	// A destination created after the existence check is kept.
	if err := renameNoReplace(parent, "a.txt", parent, "taken.txt", stat("a.txt")); err == nil {
		t.Fatal("replaced an existing file")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "taken.txt")); string(data) != "game data" {
		t.Fatal("destination changed:", string(data))
	}
	if err := renameNoReplace(parent, "a.txt", parent, "b.txt", stat("a.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("source still present")
	}
	if err := renameNoReplace(parent, "folder", parent, "moved", stat("folder")); err == nil {
		t.Fatal("renamed a folder without an atomic no-replace rename")
	}
}

func TestFreeBytesCountsFundamentalBlocks(t *testing.T) {
	// SSHFS reports FreeBSD storage with a transfer size far larger than
	// the blocks Bavail counts.
	if got := FreeBytes(&unix.Statfs_t{Bsize: 131072, Frsize: 512, Bavail: 131072}); got != 64<<20 {
		t.Fatal(got)
	}
	if got := FreeBytes(&unix.Statfs_t{Bsize: 4096, Bavail: 10}); got != 40960 {
		t.Fatal("without Frsize:", got)
	}
	if got := FreeBytes(&unix.Statfs_t{Frsize: 4096, Bavail: 1 << 62}); got != 1<<62 {
		t.Fatal("not capped:", got)
	}
}

func TestExtractionStopsWhenOtherWritersTakeTheReserve(t *testing.T) {
	root := t.TempDir()
	stage := ".ludock-restore-0f9619ff-8b86-4011-b42d-00c04fc964ff"
	if _, code := restore(t, root, stage, "stage"); code != 0 {
		t.Fatal("stage")
	}
	// The disk has room when the file starts, then another container fills
	// it while the file is copied.
	checks := 0
	statfs = func(fd int, stat *unix.Statfs_t) error {
		checks++
		*stat = unix.Statfs_t{Bsize: 4096, Frsize: 4096, Bavail: 100 << 8}
		if checks > 2 {
			stat.Bavail = 1
		}
		return nil
	}
	t.Cleanup(func() { statfs = unix.Fstatfs })
	size := 3 * copyChunk
	record := `{"name":"world.dat","type":"file","size":` + strconv.Itoa(size) + `,"uid":` + strconv.Itoa(os.Getuid()) +
		`,"gid":` + strconv.Itoa(os.Getgid()) + `,"mode":420}`
	argument, _ := json.Marshal(map[string]any{"root": root, "stage": stage, "maxBytes": size, "reserveBytes": 1 << 20})
	var stderr bytes.Buffer
	input := strings.NewReader(record + "\n" + strings.Repeat("x", size))
	if code := RunRestoreExtract(string(argument), input, io.Discard, &stderr); code != ExtractFullStatus || stderr.String() != ExtractFull {
		t.Fatal(code, stderr.String())
	}
	if info, err := os.Stat(filepath.Join(root, stage, "new", "world.dat")); err != nil || info.Size() > copyChunk {
		t.Fatal("kept writing into the reserve", info, err)
	}
}

func TestAFullDiskDuringAWriteIsReportedAsFull(t *testing.T) {
	for _, err := range []error{errFull, unix.ENOSPC, fmt.Errorf("sync: %w", unix.EDQUOT)} {
		if status, message := extractExit(err); status != ExtractFullStatus || message != ExtractFull {
			t.Error(err, status)
		}
	}
	if status, _ := extractExit(errUnsafe); status != 1 {
		t.Error("an unsafe archive reported as a full disk")
	}
}
