package helper

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Crunging/Ludock/internal/mountinfo"
	"github.com/Crunging/Ludock/internal/tarstream"
)

func runFile(t *testing.T, request FileRequest, stdin []byte) (string, int) {
	t.Helper()
	return runFileFrom(t, request, bytes.NewReader(stdin))
}

func runFileFrom(t *testing.T, request FileRequest, stdin io.Reader) (string, int) {
	t.Helper()
	argument, _ := json.Marshal(request)
	var stdout, stderr bytes.Buffer
	code := RunFile(string(argument), stdin, &stdout, &stderr)
	if code != 0 && stderr.String() != FileFailure {
		t.Fatalf("stderr %q", stderr.String())
	}
	if code != 0 && strings.Contains(stdout.String()+stderr.String(), request.Root) {
		t.Fatal("failure leaked a path")
	}
	return stdout.String(), code
}

// trailerHook is an upload's stdin that runs before once the helper has the
// body and reads on for the trailer.
type trailerHook struct {
	body, trailer []byte
	before        func() error
	err           error
}

func (h *trailerHook) Read(p []byte) (int, error) {
	if len(h.body) > 0 {
		n := copy(p, h.body)
		h.body = h.body[n:]
		return n, nil
	}
	if h.before != nil {
		h.err, h.before = h.before(), nil
	}
	if len(h.trailer) == 0 {
		return 0, io.EOF
	}
	n := copy(p, h.trailer)
	h.trailer = h.trailer[n:]
	return n, nil
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
	// What happens once the body is in, when the helper asks for the trailer:
	// the staged file is still root's alone, and a folder moved out of the
	// root or a staging name swapped meanwhile publishes nothing.
	os.Mkdir(filepath.Join(root, "moving"), 0o755)
	os.WriteFile(filepath.Join(root, "other"), []byte("other"), 0o644)
	staging := ".ludock-upload-" + id + ".tmp"
	for _, test := range []struct {
		path   string
		before func() error
		ok     bool
	}{
		{"staged.txt", func() error {
			if info, err := os.Lstat(filepath.Join(root, staging)); err != nil || info.Mode().Perm() != 0o600 {
				return fmt.Errorf("staged file readable: %v %v", info.Mode(), err)
			}
			return nil
		}, true},
		{"moving/moved.txt", func() error { return os.Rename(filepath.Join(root, "moving"), filepath.Join(outside, "moving")) }, false},
		{"swapped.txt", func() error { return os.Rename(filepath.Join(root, "other"), filepath.Join(root, staging)) }, false},
	} {
		request := FileRequest{Operation: "upload", Root: root, Path: test.path, Size: &size, UploadID: id}
		stdin := &trailerHook{body: []byte("12345"), trailer: []byte(id), before: test.before}
		if _, code := runFileFrom(t, request, stdin); (code == 0) != test.ok || stdin.err != nil {
			t.Fatal(test.path, code, stdin.err)
		}
		published := filepath.Join(root, test.path)
		if test.path == "moving/moved.txt" {
			published = filepath.Join(outside, test.path)
		}
		if data, err := os.ReadFile(published); test.ok != (err == nil) || (test.ok && string(data) != "12345") {
			t.Fatal(test.path, "published", string(data), err)
		}
	}
	if info, _ := os.Stat(filepath.Join(root, "staged.txt")); info.Mode().Perm() != 0o666&^fs.FileMode(currentUmask()) {
		t.Fatal("published with mode", info.Mode())
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

	// The file gets its group before its final mode, and its owner last, so
	// the mode's group bits never apply to the helper's own group.
	var steps []string
	parent, _ := os.Stat(root)
	want := parent.Sys().(*syscall.Stat_t)
	fchown = func(descriptor, uid, gid int) error {
		steps = append(steps, fmt.Sprintf("owner %d group %d", uid, gid))
		return unix.Fchown(descriptor, uid, gid)
	}
	fchmod = func(descriptor int, mode uint32) error {
		var stat unix.Stat_t
		unix.Fstat(descriptor, &stat)
		steps = append(steps, fmt.Sprintf("mode with group %d", stat.Gid))
		return unix.Fchmod(descriptor, mode)
	}
	defer func() { fchown, fchmod = unix.Fchown, unix.Fchmod }()
	if _, code := runFile(t, created, []byte("12345"+id)); code != 0 || !slices.Equal(steps, []string{fmt.Sprintf("owner -1 group %d", want.Gid),
		fmt.Sprintf("mode with group %d", want.Gid), fmt.Sprintf("owner %d group -1", want.Uid)}) {
		t.Fatal(code, steps)
	}
	// A mode whose group grants more than its owner is narrowed to the
	// owner's, since the future owner holds the group's rights until it is
	// set.
	fchown, fchmod = unix.Fchown, unix.Fchmod
	os.Chmod(filepath.Join(root, "new.txt"), 0o460)
	if _, code := runFile(t, created, []byte("12345"+id)); code != 0 {
		t.Fatal("replace", code)
	}
	if info, _ := os.Stat(filepath.Join(root, "new.txt")); info.Mode().Perm() != 0o440 {
		t.Fatal("kept a mode wider than its owner's", info.Mode())
	}
	// A publish that took effect and then failed, as on NFS, is a success.
	renameat = func(fromDirectory int, from string, toDirectory int, to string) error {
		unix.Renameat(fromDirectory, from, toDirectory, to)
		return unix.EIO
	}
	defer func() { renameat = unix.Renameat }()
	if _, code := runFile(t, upload, []byte("again"+id)); code != 0 {
		t.Fatal("reported a published file as failed")
	}
	if data, _ := os.ReadFile(filepath.Join(root, "server.properties")); string(data) != "again" {
		t.Fatal(string(data))
	}
	// One that failed with nothing to show it did nothing is unconfirmed.
	renameat = func(int, string, int, string) error { return unix.EIO }
	if _, code := runFile(t, upload, []byte("third"+id)); code != FileUncertain {
		t.Fatal("reported an unknown publish as", code)
	}
}

// An upload first removes the staging files that cut-off uploads left in its
// folder, reading the folder a few names at a time: only regular files with
// a staging name untouched for an hour, since a younger one may belong to an
// upload still running elsewhere, and never one an excluded mount hides from
// the game, which the targeted cleanups leave too.
func TestUploadSweepsStaleStaging(t *testing.T) {
	root, _ := dataRoot(t)
	largest := 0
	sweepChunk = 3
	readNames = func(directory *os.File, count int) ([]string, error) {
		names, err := directory.Readdirnames(count)
		largest = max(largest, len(names))
		return names, err
	}
	defer func() { sweepChunk, sweepTime, readNames = 1024, 2*time.Second, (*os.File).Readdirnames }()
	stage := func(index int) string {
		return fmt.Sprintf(".ludock-upload-6f9619ff-8b86-4011-b42d-00c04fc964%02d.tmp", index)
	}
	old := time.Now().Add(-2 * time.Hour)
	for index := range 20 {
		os.WriteFile(filepath.Join(root, fmt.Sprintf("game-%d.dat", index)), nil, 0o644)
		os.WriteFile(filepath.Join(root, stage(index)), []byte("cut off"), 0o600)
		if index != 7 {
			os.Chtimes(filepath.Join(root, stage(index)), old, old)
		}
	}
	hidden := filepath.Join(root, stage(3))
	if _, code := runFile(t, FileRequest{Operation: "upload-prepare", Root: root, Blocked: []string{hidden}}, nil); code != 0 || largest != 3 {
		t.Fatal("prepare", code, "read", largest, "names at once")
	}
	for index := range 20 {
		_, err := os.Lstat(filepath.Join(root, stage(index)))
		if kept := index == 3 || index == 7; kept != (err == nil) {
			t.Fatal(stage(index), "kept", err == nil)
		}
	}
	// A folder past the sweep's budget is swept only so far, and the upload
	// goes ahead.
	seen := 0
	sweepEntries = 5
	readNames = func(directory *os.File, count int) ([]string, error) {
		names, err := directory.Readdirnames(count)
		seen += len(names)
		return names, err
	}
	if _, code := runFile(t, FileRequest{Operation: "upload-prepare", Root: root, Blocked: []string{hidden}}, nil); code != 0 || seen > 5 {
		t.Fatal("prepare past the budget", code, seen)
	}
	sweepEntries = 10000
	// Time runs out mid-chunk, as on slow storage: no candidate after that is
	// touched, and the upload still goes ahead.
	old2 := filepath.Join(root, stage(30))
	os.WriteFile(old2, nil, 0o600)
	os.Chtimes(old2, old, old)
	sweepTime = 20 * time.Millisecond
	readNames = func(directory *os.File, count int) ([]string, error) {
		names, err := directory.Readdirnames(count)
		time.Sleep(30 * time.Millisecond)
		return names, err
	}
	if _, code := runFile(t, FileRequest{Operation: "upload-prepare", Root: root, Blocked: []string{hidden}}, nil); code != 0 {
		t.Fatal("prepare past its time", code)
	}
	if _, err := os.Lstat(old2); err != nil {
		t.Fatal("swept on past its time", err)
	}
	sweepTime = 2 * time.Second
	// A folder that can't be read is left unswept, and the upload goes ahead.
	readNames = func(*os.File, int) ([]string, error) { return nil, unix.EIO }
	if _, code := runFile(t, FileRequest{Operation: "upload-prepare", Root: root}, nil); code != 0 {
		t.Fatal("a sweep that couldn't read the folder stopped the upload", code)
	}
	for _, request := range []FileRequest{
		{Operation: "upload-files-cleanup", Root: root, Files: []UploadFile{{UploadID: stage(3)[15:51]}}, Blocked: []string{hidden}},
		{Operation: "upload-cleanup", Root: root, Path: "server.properties", UploadID: stage(3)[15:51], Blocked: []string{hidden}},
	} {
		if _, code := runFile(t, request, nil); code == 0 {
			t.Fatal(request.Operation, "removed a staging file over an excluded mount")
		}
		if _, err := os.Lstat(hidden); err != nil {
			t.Fatal(request.Operation, err)
		}
	}
}

func TestDownloadsAndBackupsAreReadableTar(t *testing.T) {
	root, _ := dataRoot(t)
	os.Remove(filepath.Join(root, "escape"))
	long := strings.Repeat("ü", 80)
	os.WriteFile(filepath.Join(root, "world", long), []byte("unicode"), 0o644)
	// A backup keeps a folder's setgid but never a file's setuid or setgid.
	os.Chmod(filepath.Join(root, "world"), 0o755|os.ModeSetgid)
	os.Chmod(filepath.Join(root, "world", "level.dat"), 0o644|os.ModeSetuid|os.ModeSetgid)
	for _, request := range []FileRequest{{Operation: "download", Root: root, Path: "world"}, {Operation: "backup", Root: root, Base: root}} {
		out, code := runFile(t, request, nil)
		if code != 0 {
			t.Fatal(request.Operation)
		}
		if request.Operation == "download" {
			header, rest, _ := strings.Cut(out, "\n")
			if header != `{"size":0,"type":"directory"}` {
				t.Fatal(header)
			}
			out = rest
		}
		// Ludock's own reader is stricter than archive/tar.
		if err := tarstream.Walk(strings.NewReader(out), func(*tarstream.Header, io.Reader) error { return nil }, nil); err != nil {
			t.Fatal(request.Operation, err)
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
			if want := map[string]int64{"source/world/": 0o2755, "source/world/level.dat": 0o644}[header.Name]; want != 0 && header.Mode != want {
				t.Fatalf("%s is archived as %o", header.Name, header.Mode)
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
	// A file's type and size come from the file being sent, ahead of it.
	out, _ := runFile(t, FileRequest{Operation: "download", Root: root, Path: "world/level.dat"}, nil)
	if out != `{"size":5,"type":"file"}`+"\n"+"level" {
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

func restore(t *testing.T, root, stage, operation string) (string, int) {
	t.Helper()
	argument, _ := json.Marshal(RestoreRequest{Root: root, Stage: stage, Operation: operation, Base: root})
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
	if out, code := restore(t, root, stage, RestoreSpace); code != 0 || !strings.Contains(out, "availableBytes") {
		t.Fatal(out)
	}
	if _, code := restore(t, root, ".ludock-restore-bad", RestoreStage); code == 0 {
		t.Fatal("accepted a bad stage")
	}
	// Other names Ludock reserves, such as another restore's stage or one a
	// person made, stay where they are, untouched.
	other := ".ludock-restore-1f9619ff-8b86-4011-b42d-00c04fc964ff"
	for _, folder := range []string{other + "/old", ".ludock-restore-anything"} {
		os.MkdirAll(filepath.Join(root, folder), 0o700)
		os.WriteFile(filepath.Join(root, folder, "kept"), []byte("kept"), 0o644)
	}
	if _, code := restore(t, root, stage, RestoreStage); code != 0 {
		t.Fatal("stage")
	}
	// The backup's root is 0755 and the live one 0750. Its world folder is
	// setgid and sticky, and a forged file in it setuid and setgid.
	os.Chmod(root, 0o750)
	records := []string{
		`{"name":"","type":"directory","size":0,"uid":` + strconv.Itoa(os.Getuid()) + `,"gid":` + strconv.Itoa(os.Getgid()) + `,"mode":493}`,
		`{"name":"world","type":"directory","size":0,"uid":` + strconv.Itoa(os.Getuid()) + `,"gid":` + strconv.Itoa(os.Getgid()) + `,"mode":2029,"mtime":1700000000}`,
		`{"name":"world/level.dat","type":"file","size":8,"uid":` + strconv.Itoa(os.Getuid()) + `,"gid":` + strconv.Itoa(os.Getgid()) + `,"mode":3492,"mtime":1700000000.5}`,
	}
	input := strings.Join(records, "\n") + "\nrestored"
	argument, _ := json.Marshal(map[string]any{"root": root, "stage": stage, "maxBytes": 100, "reserveBytes": 0})
	var stdout, stderr bytes.Buffer
	if code := RunRestoreExtract(string(argument), strings.NewReader(input), &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "ok ") {
		t.Fatal(stderr.String())
	}
	info, _ := os.Stat(filepath.Join(root, stage, "new", "world"))
	if info.Mode()&^os.ModeType != 0o755|os.ModeSetgid|os.ModeSticky || info.ModTime().Unix() != 1700000000 {
		t.Fatal(info.Mode(), info.ModTime())
	}
	if info, _ := os.Stat(filepath.Join(root, stage, "new", "world", "level.dat")); info.Mode() != 0o644 {
		t.Fatal("restored a file as", info.Mode())
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
	for _, step := range []string{RestoreMoveOld, RestoreMoveNew} {
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
	// Undo as if a server started elsewhere wrote the replacement: it is set
	// aside in a folder its owner can open, owned like the data folder it
	// sits in once that has its own back.
	aside := SetAsideName("6f9619ff-8b86-4011-b42d-00c04fc964ff")
	argument, _ = json.Marshal(RestoreRequest{Root: root, Stage: stage, Operation: RestoreSetAside, Aside: aside, Base: root})
	if code := RunRestore(string(argument), io.Discard, io.Discard); code != 0 {
		t.Fatal("set aside")
	}
	if _, code := restore(t, root, stage, RestoreRollbackOld); code != 0 {
		t.Fatal("rollback")
	}
	if data, _ := os.ReadFile(filepath.Join(root, "world", "level.dat")); string(data) != "level" {
		t.Fatal(string(data))
	}
	if data, _ := os.ReadFile(filepath.Join(root, aside, "world", "level.dat")); string(data) != "restored" {
		t.Fatal(string(data))
	}
	if info, _ := os.Stat(filepath.Join(root, aside)); info.Mode().Perm() != 0o750 {
		t.Fatal("set-aside folder is", info.Mode())
	}
	if _, code := restore(t, root, stage, RestoreCleanup); code != 0 {
		t.Fatal("cleanup")
	}
	if _, err := os.Stat(filepath.Join(root, stage)); !os.IsNotExist(err) {
		t.Fatal("stage left behind")
	}
	for _, folder := range []string{other + "/old", ".ludock-restore-anything"} {
		if data, _ := os.ReadFile(filepath.Join(root, folder, "kept")); string(data) != "kept" {
			t.Fatal("a restore moved", folder)
		}
	}
	if _, code := restore(t, root, stage, RestoreCleanup); code != 0 {
		t.Fatal("cleanup is idempotent")
	}
	// Special files and hard links are never moved.
	restore(t, root, stage, RestoreStage)
	os.Link(filepath.Join(root, "server.properties"), filepath.Join(root, "hardlink"))
	if _, code := restore(t, root, stage, RestoreMoveOld); code == 0 {
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

// Extraction stops when other writers take the reserve.
// Entries alternate between directories, return to an earlier one, and
// name parents the archive never lists; each lands in its own directory.
func TestRestoreExtractInterleavedDirectories(t *testing.T) {
	root, _ := dataRoot(t)
	stage := ".ludock-restore-7f9619ff-8b86-4011-b42d-00c04fc964ff"
	if _, code := restore(t, root, stage, RestoreStage); code != 0 {
		t.Fatal("stage")
	}
	owner := `"uid":` + strconv.Itoa(os.Getuid()) + `,"gid":` + strconv.Itoa(os.Getgid())
	files := []string{"a/one", "b/c/two", "a/three", "b/c/d/four", "five", "b/c/six"}
	var input strings.Builder
	for _, name := range files {
		input.WriteString(`{"name":"` + name + `","type":"file","size":` + strconv.Itoa(len(name)) + `,` + owner + `,"mode":420}` + "\n" + name)
	}
	argument, _ := json.Marshal(map[string]any{"root": root, "stage": stage, "maxBytes": 1000, "reserveBytes": 0})
	var stderr bytes.Buffer
	if code := RunRestoreExtract(string(argument), strings.NewReader(input.String()), io.Discard, &stderr); code != 0 {
		t.Fatal(stderr.String())
	}
	for _, name := range files {
		if data, err := os.ReadFile(filepath.Join(root, stage, "new", name)); err != nil || string(data) != name {
			t.Fatal(name, string(data), err)
		}
	}
}

func TestExtractionKeepsReserve(t *testing.T) {
	root := t.TempDir()
	stage := ".ludock-restore-0f9619ff-8b86-4011-b42d-00c04fc964ff"
	if _, code := restore(t, root, stage, RestoreStage); code != 0 {
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

// A full disk during a write is reported as full.
func TestExtractExitOnFullDisk(t *testing.T) {
	for _, err := range []error{errFull, unix.ENOSPC, fmt.Errorf("sync: %w", unix.EDQUOT)} {
		if status, message := extractExit(err); status != ExtractFullStatus || message != ExtractFull {
			t.Error(err, status)
		}
	}
	if status, _ := extractExit(errUnsafe); status != 1 {
		t.Error("an unsafe archive reported as a full disk")
	}
}

// Rolling back a one-item restore whose replacement is still staged keeps a
// live entry something else created after the old copy moved out, and keeps
// the old copy; once the replacement is installed, rollback takes it back
// and returns the old copy.
func TestRestoreItemRollbackOwnership(t *testing.T) {
	root := t.TempDir()
	stage := ".ludock-restore-0f9619ff-8b86-4011-b42d-00c04fc964ff"
	for _, dir := range []string{"world", stage + "/old", stage + "/new/world"} {
		os.MkdirAll(filepath.Join(root, dir), 0o755)
	}
	os.WriteFile(filepath.Join(root, stage, "old", "item"), []byte("current"), 0o644)
	os.WriteFile(filepath.Join(root, stage, "new", "world", "level.dat"), []byte("backup"), 0o644)
	os.WriteFile(filepath.Join(root, "world", "level.dat"), []byte("someone else"), 0o644)
	step := func(operation string) int {
		request, _ := json.Marshal(RestoreRequest{Root: root, Stage: stage, Operation: operation, Item: "world/level.dat"})
		return RunRestore(string(request), io.Discard, io.Discard)
	}
	if step(RestoreRollbackClean) == 0 {
		t.Fatal("took back an entry the swap never installed")
	}
	if data, _ := os.ReadFile(filepath.Join(root, "world", "level.dat")); string(data) != "someone else" {
		t.Fatal(string(data))
	}
	os.Remove(filepath.Join(root, "world", "level.dat"))
	if step(RestoreMoveNew) != 0 || step(RestoreRollbackClean) != 0 || step(RestoreRollbackOld) != 0 {
		t.Fatal("rollback after the swap failed")
	}
	if data, _ := os.ReadFile(filepath.Join(root, "world", "level.dat")); string(data) != "current" {
		t.Fatal(string(data))
	}
	if data, _ := os.ReadFile(filepath.Join(root, stage, "returned")); string(data) != "backup" {
		t.Fatal(string(data))
	}
}

// The data a restore replaces, one item or a whole folder, holds something
// cleanup could never remove, first a named pipe and then another filesystem
// mounted inside it, such as a disk the host mounted under the data: the
// restore is refused before the server stops and never moves anything
// aside. The mount is found by the helper's mount table before the stop, and
// by mount IDs as the stopped data is moved; tests can't mount, so both are
// faked, the table from a captured line with an escaped space.
func TestRestoreRefusesUnremovableData(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "game data")
	stage := ".ludock-restore-0f9619ff-8b86-4011-b42d-00c04fc964ff"
	for _, dir := range []string{"world/region", "world/mods/cache", stage + "/old"} {
		os.MkdirAll(filepath.Join(root, dir), 0o755)
	}
	stepIn := func(operation, item string) int {
		request, _ := json.Marshal(RestoreRequest{Root: root, Stage: stage, Operation: operation, Item: item, Base: base})
		return RunRestore(string(request), io.Discard, io.Discard)
	}
	step := func(operation string) int { return stepIn(operation, "world") }
	unmoved := func(how string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(root, "world/mods/cache")); err != nil {
			t.Fatal(how, "moved the folder", err)
		}
	}
	if step(RestoreCheck) != 0 {
		t.Fatal("refused a folder on one mount")
	}
	pipe := filepath.Join(root, "world/region/console.pipe")
	if err := unix.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	if step(RestoreCheck) == 0 || step(RestoreMoveOld) == 0 {
		t.Fatal("accepted a folder with a named pipe inside it")
	}
	os.Remove(pipe)
	var mounted unix.Stat_t
	if err := unix.Stat(filepath.Join(root, "world/mods/cache"), &mounted); err != nil {
		t.Fatal(err)
	}
	mountOf = func(descriptor fd) (uint64, error) {
		var info unix.Stat_t
		if err := unix.Fstat(int(descriptor), &info); err != nil {
			return 0, err
		}
		if info.Ino == mounted.Ino {
			return 2, nil
		}
		return 1, nil
	}
	t.Cleanup(func() { mountOf = mountID })
	if step(RestoreCheck) == 0 || step(RestoreMoveOld) != RestoreMounted || stepIn(RestoreMoveOld, "") != RestoreMounted {
		t.Fatal("accepted a folder with a mount inside it")
	}
	unmoved("found")
	mountOf = mountID
	// Another restore's stage below the top is never moved or removed with
	// what holds it, whether this restore replaces it or is being undone.
	other := filepath.Join(root, "world/region", ".ludock-restore-1f9619ff-8b86-4011-b42d-00c04fc964ff")
	os.MkdirAll(filepath.Join(other, "old"), 0o700)
	os.MkdirAll(filepath.Join(root, stage, "new"), 0o700)
	for _, request := range []RestoreRequest{
		{Operation: RestoreCheck, Item: "world"},
		{Operation: RestoreMoveOld, Item: "world"},
		{Operation: RestoreMoveOld},
		{Operation: RestoreRollbackClean, Item: "world"},
		{Operation: RestoreRollbackClean},
		{Operation: RestoreSetAside, Item: "world", Aside: SetAsideName("0f9619ff-8b86-4011-b42d-00c04fc964ff")},
		{Operation: RestoreSetAside, Aside: SetAsideName("0f9619ff-8b86-4011-b42d-00c04fc964ff")},
	} {
		request.Root, request.Stage, request.Base = root, stage, base
		argument, _ := json.Marshal(request)
		if code := RunRestore(string(argument), io.Discard, io.Discard); code != RestoreHoldsStage {
			t.Errorf("%s %q: %d", request.Operation, request.Item, code)
		}
		if _, err := os.Stat(filepath.Join(other, "old")); err != nil {
			t.Fatal(request.Operation, request.Item, "moved the other stage")
		}
	}
	os.RemoveAll(other)
	table := func(point string) {
		escaped := strings.ReplaceAll(point, " ", `\040`)
		mountinfo.Table = func() ([]byte, error) {
			return []byte("22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n" +
				"36 22 98:0 /mnt1 " + escaped + " rw,noatime master:1 - ext3 /dev/root rw,errors=continue\n"), nil
		}
	}
	previous := mountinfo.Table
	t.Cleanup(func() { mountinfo.Table = previous })
	for point, refused := range map[string]bool{
		filepath.Join(root, "world/mods/cache"): true,
		// On the path down to the data folder, as when ludock.files picks a
		// folder the host mounted.
		root: true,
		// The helper's own mount, and a sibling the item doesn't reach.
		base:                          false,
		filepath.Join(root, "config"): false,
		// The kernel leaves a non-breaking space unescaped.
		root + "\u00a0copy": false,
	} {
		table(point)
		if got := step(RestoreCheck) == RestoreMounted; got != refused {
			t.Errorf("table %s: item check refused %v", point, got)
		}
	}
	table(filepath.Join(root, "world/mods/cache"))
	if step(RestoreMoveOld) != RestoreMounted || stepIn(RestoreMoveOld, "") != RestoreMounted {
		t.Fatal("moved data holding a mount from the table")
	}
	unmoved("table")
}

// Input that ends partway through a record or a file's bytes, as a cut
// stream does, exits with its own status; a record it refuses still exits
// as a refusal.
func TestExtractionCutInput(t *testing.T) {
	root := t.TempDir()
	stage := ".ludock-restore-0f9619ff-8b86-4011-b42d-00c04fc964ff"
	if _, code := restore(t, root, stage, RestoreStage); code != 0 {
		t.Fatal("stage")
	}
	owner := `,"uid":` + strconv.Itoa(os.Getuid()) + `,"gid":` + strconv.Itoa(os.Getgid()) + `,"mode":420}`
	argument, _ := json.Marshal(map[string]any{"root": root, "stage": stage, "maxBytes": 1000, "reserveBytes": 0})
	for _, c := range []struct {
		name, input string
		status      int
	}{
		{"mid-file", `{"name":"a.dat","type":"file","size":10` + owner + "\nabc", ExtractCutStatus},
		{"mid-record", `{"name":"b.dat","type":"fi`, ExtractCutStatus},
		{"refused", `{"name":"../c.dat","type":"file","size":1` + owner + "\nx", 1},
	} {
		var stderr bytes.Buffer
		if code := RunRestoreExtract(string(argument), strings.NewReader(c.input), io.Discard, &stderr); code != c.status {
			t.Error(c.name, code, stderr.String())
		}
	}
}

// A restore's stage folder at the top of the data and an unpack's staging
// folder beside its archive, in any letter case, are Ludock's own: forged
// file requests never list, read, create, rename, move, upload into, or
// delete them, and downloads and backups of the folders around them leave
// them out.
func TestReservedNamesStayOutOfFiles(t *testing.T) {
	root, _ := dataRoot(t)
	os.Remove(filepath.Join(root, "escape"))
	stage := ".ludock-restore-6f9619ff-8b86-4011-b42d-00c04fc964ff"
	os.MkdirAll(filepath.Join(root, stage, "old"), 0o700)
	os.WriteFile(filepath.Join(root, stage, "old", "level.dat"), []byte("previous"), 0o644)
	os.MkdirAll(filepath.Join(root, "world", ".LUDOCK-unpack-x"), 0o755)
	os.WriteFile(filepath.Join(root, "world", ".LUDOCK-unpack-x", "part"), []byte("part"), 0o644)
	id := "6f9619ff-8b86-4011-b42d-00c04fc964ff"
	size, reserve := int64(1), int64(0)
	for _, request := range []FileRequest{
		{Operation: "list", Root: root, Path: stage},
		{Operation: "stat", Root: root, Path: stage + "/old/level.dat"},
		{Operation: "space", Root: root, Path: stage},
		{Operation: "download", Root: root, Path: stage + "/old"},
		{Operation: "delete", Root: root, Path: stage},
		{Operation: "delete", Root: root, Path: "world/.LUDOCK-unpack-x/part"},
		{Operation: "rename", Root: root, Path: stage, Destination: "kept"},
		{Operation: "rename", Root: root, Path: "server.properties", Destination: ".ludock-restore-x"},
		{Operation: "mkdir", Root: root, Path: ".Ludock-restore-x"},
		{Operation: "upload", Root: root, Path: stage + "/old/level.dat", Size: &size, UploadID: id},
		{Operation: "upload", Root: root, Path: ".ludock-x", Size: &size, UploadID: id},
		{Operation: "unpack", Root: root, Path: stage + "/old/level.dat", UploadID: id, Reserve: &reserve},
		{Operation: "delete-paths", Root: root, Paths: []string{"server.properties", stage}},
		{Operation: "download-paths", Root: root, Paths: []string{"server.properties", stage}},
		{Operation: "move", Root: root, Paths: []string{stage}, Destination: "world"},
		{Operation: "move", Root: root, Paths: []string{"server.properties"}, Destination: stage + "/old"},
	} {
		if _, code := runFile(t, request, []byte("x"+id)); code == 0 {
			t.Errorf("%s %s %s %v succeeded", request.Operation, request.Path, request.Destination, request.Paths)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(root, stage, "old", "level.dat")); string(data) != "previous" {
		t.Fatal("the stage folder changed")
	}
	// A stage below the top, where a wider ludock.files or another server's
	// folder shows it, is never deleted, renamed, or moved with what holds
	// it; an unpack's leftover folder is taken along.
	os.MkdirAll(filepath.Join(root, "world", "region", stage, "old"), 0o700)
	os.WriteFile(filepath.Join(root, "world", "region", stage, "old", "level.dat"), []byte("previous"), 0o644)
	for _, request := range []FileRequest{
		{Operation: "delete", Root: root, Path: "world"},
		{Operation: "delete-paths", Root: root, Paths: []string{"server.properties", "world"}},
		{Operation: "rename", Root: root, Path: "world", Destination: "renamed"},
		{Operation: "move", Root: root, Paths: []string{"world"}, Destination: "private"},
	} {
		if _, code := runFile(t, request, nil); code != FileHoldsRestore {
			t.Errorf("%s %v: %d", request.Operation, request.Paths, code)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(root, "world", "region", stage, "old", "level.dat")); string(data) != "previous" {
		t.Fatal("a stage below the top changed")
	}
	os.MkdirAll(filepath.Join(root, "private", ".ludock-unpack-y"), 0o700)
	if _, code := runFile(t, FileRequest{Operation: "delete", Root: root, Path: "private"}, nil); code != 0 {
		t.Fatal("refused a folder holding an unpack's leftovers", code)
	}
	os.MkdirAll(filepath.Join(root, "private"), 0o755)
	if _, err := os.Stat(filepath.Join(root, "server.properties")); err != nil {
		t.Fatal("a refused selection changed something")
	}
	for _, folder := range []string{"", "world"} {
		out, _ := runFile(t, FileRequest{Operation: "list", Root: root, Path: folder}, nil)
		if strings.Contains(strings.ToLower(out), ".ludock-") {
			t.Fatal("listed", out)
		}
	}
	for _, request := range []FileRequest{
		{Operation: "download", Root: root, Path: "world"},
		{Operation: "download-paths", Root: root, Paths: []string{"world"}},
		{Operation: "backup", Root: root, Base: root},
	} {
		out, code := runFile(t, request, nil)
		if code != 0 {
			t.Fatal(request.Operation, code)
		}
		if request.Operation != "backup" {
			out = out[strings.IndexByte(out, '\n')+1:]
		}
		reader := tar.NewReader(strings.NewReader(out))
		for header, err := reader.Next(); err == nil; header, err = reader.Next() {
			if strings.Contains(strings.ToLower(header.Name), ".ludock-") {
				t.Fatal(request.Operation, "archived", header.Name)
			}
		}
	}
}

// A folder or file mounted separately inside a data folder, whether the
// backend declared it or only the file system shows it, is never entered or
// moved: deleting, renaming, or moving it or a folder holding it is refused
// before anything changes, and a backup of the data refuses to read it.
// Tests can't mount, so the file system's answer is faked by inode, and the
// helper's mount table by a line.
func TestChangesNeverCrossAMount(t *testing.T) {
	root, _ := dataRoot(t)
	os.Remove(filepath.Join(root, "escape"))
	mods := filepath.Join(root, "world", "mods")
	os.MkdirAll(mods, 0o755)
	os.WriteFile(filepath.Join(mods, "a.jar"), []byte("jar"), 0o644)
	var mounted, mountedFile unix.Stat_t
	if err := unix.Stat(mods, &mounted); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(filepath.Join(root, "world", "level.dat"), &mountedFile); err != nil {
		t.Fatal(err)
	}
	requests := map[string]FileRequest{
		"delete the folder":  {Operation: "delete", Root: root, Path: "world"},
		"delete the mount":   {Operation: "delete", Root: root, Path: "world/mods"},
		"delete a selection": {Operation: "delete-paths", Root: root, Paths: []string{"server.properties", "world"}},
		"rename the folder":  {Operation: "rename", Root: root, Path: "world", Destination: "renamed"},
		"rename the mount":   {Operation: "rename", Root: root, Path: "world/mods", Destination: "world/plugins"},
		"move the folder":    {Operation: "move", Root: root, Paths: []string{"world"}, Destination: "private"},
	}
	intact := func(how string) {
		t.Helper()
		for _, name := range []string{"world/mods/a.jar", "world/level.dat", "server.properties"} {
			if _, err := os.Stat(filepath.Join(root, name)); err != nil {
				t.Fatal(how, "changed", name)
			}
		}
	}
	for name, request := range requests {
		request.Mounts = []string{mods}
		if _, code := runFile(t, request, nil); code != FileMounted {
			t.Errorf("declared: %s: %d", name, code)
		}
	}
	intact("declared")
	mountOf = func(descriptor fd) (uint64, error) {
		var info unix.Stat_t
		if err := unix.Fstat(int(descriptor), &info); err != nil {
			return 0, err
		}
		if info.Ino == mounted.Ino {
			return 2, nil
		}
		return 1, nil
	}
	t.Cleanup(func() { mountOf = mountID })
	requests["back up"] = FileRequest{Operation: "backup", Root: root, Base: root}
	for name, request := range requests {
		if _, code := runFile(t, request, nil); code != FileMounted {
			t.Errorf("found: %s: %d", name, code)
		}
	}
	intact("found")
	mounted.Ino = mountedFile.Ino
	for _, name := range []string{"delete the folder", "delete a selection", "back up"} {
		if _, code := runFile(t, requests[name], nil); code != FileMounted {
			t.Errorf("file: %s: %d", name, code)
		}
	}
	intact("file")
	mountOf = mountID
	previous := mountinfo.Table
	t.Cleanup(func() { mountinfo.Table = previous })
	mountinfo.Table = func() ([]byte, error) {
		return []byte("40 22 0:45 / " + mods + " rw - nfs4 server:/share rw\n"), nil
	}
	for _, request := range []FileRequest{requests["back up"], {Operation: "mounts", Root: root, Base: root},
		{Operation: "mounts", Root: root, Path: "world", Base: root}} {
		if _, code := runFile(t, request, nil); code != FileMounted {
			t.Errorf("table: %s %s: %d", request.Operation, request.Path, code)
		}
	}
	if _, code := runFile(t, FileRequest{Operation: "mounts", Root: root, Path: "private", Base: root}, nil); code != 0 {
		t.Error("refused a path the mount isn't on")
	}
	mountinfo.Table = func() ([]byte, error) { return nil, nil }
	if _, code := runFile(t, requests["delete the folder"], nil); code != 0 {
		t.Fatal("refused a folder on one mount", code)
	}
	if _, code := runFile(t, requests["back up"], nil); code != 0 {
		t.Fatal("refused a backup on one mount", code)
	}
}
