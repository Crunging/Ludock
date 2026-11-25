package helper

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
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
	// A mode whose group grants more than its owner is kept when the owner
	// stays the helper, and narrowed to the owner's when it changes, since
	// the future owner holds the group's rights until it is set.
	fchown, fchmod = unix.Fchown, unix.Fchmod
	for _, test := range []struct {
		helper func() int
		want   fs.FileMode
	}{{os.Geteuid, 0o460}, {func() int { return -2 }, 0o440}} {
		helperUID = test.helper
		os.Chmod(filepath.Join(root, "new.txt"), 0o460)
		if _, code := runFile(t, created, []byte("12345"+id)); code != 0 {
			t.Fatal("replace", code)
		}
		if info, _ := os.Stat(filepath.Join(root, "new.txt")); info.Mode().Perm() != test.want {
			t.Fatal("replaced 0460 as", info.Mode())
		}
	}
	helperUID = os.Geteuid
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

func mustACL(t *testing.T, text string) []byte {
	t.Helper()
	value, err := ParseACLText(text)
	if err != nil {
		t.Fatal(text, err)
	}
	return value
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
	// setgid and sticky, and a forged file in it setuid and setgid. The
	// backup's root also had an ACL the live one hasn't, whose mask must not
	// become the live root's group bits. world's ACL had no default ACL, so
	// it keeps none the stage gave it.
	os.Chmod(root, 0o750)
	inherited := mustACL(t, "user::rwx,user:4242:rwx,group::rwx,mask::rwx,other::rwx")
	err := unix.Setxattr(filepath.Join(root, stage, "new"), defaultACL, inherited, 0)
	if err != nil && !errors.Is(err, unix.ENOTSUP) {
		t.Fatal(err)
	}
	acls := err == nil
	records := []string{
		`{"acl":"user::rwx,user:4242:rwx,group::r-x,mask::rwx,other::r-x","name":"","type":"directory","size":0,"uid":` + strconv.Itoa(os.Getuid()) + `,"gid":` + strconv.Itoa(os.Getgid()) + `,"mode":493}`,
		`{"acl":"user::rwx,user:4242:r-x,group::r-x,mask::r-x,other::r-x","name":"world","type":"directory","size":0,"uid":` + strconv.Itoa(os.Getuid()) + `,"gid":` + strconv.Itoa(os.Getgid()) + `,"mode":2029,"mtime":1700000000}`,
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
	if _, err := unix.Getxattr(filepath.Join(root, stage, "new", "world"), defaultACL, nil); acls && !errors.Is(err, unix.ENODATA) {
		t.Fatal("world kept the stage's default ACL", err)
	}
	if info, _ := os.Stat(filepath.Join(root, stage, "new", "world", "level.dat")); info.Mode() != 0o644 {
		t.Fatal("restored a file as", info.Mode())
	}
	// Storage that refuses a file's ACL leaves it the archive's mode, whose
	// group bits are the owning group's own, without an ACL inherited from
	// its folder, and the extraction says so.
	unix.Setxattr(filepath.Join(root, stage, "new"), defaultACL, inherited, 0)
	fsetxattr = func(int, string, []byte, int) error { return unix.EOPNOTSUPP }
	refused := `{"acl":"user::rw-,user:4242:rw-,group::r--,mask::rw-,other::---","name":"whitelist.json","type":"file","size":2,"uid":` +
		strconv.Itoa(os.Getuid()) + `,"gid":` + strconv.Itoa(os.Getgid()) + `,"mode":416}` + "\n[]"
	stdout.Reset()
	code := RunRestoreExtract(string(argument), strings.NewReader(refused), &stdout, &stderr)
	fsetxattr = unix.Fsetxattr
	if code != 0 || stdout.String() != "ok 1 "+ExtractACLsLost {
		t.Fatal(stdout.String(), stderr.String())
	}
	whitelist := filepath.Join(root, stage, "new", "whitelist.json")
	if _, err := unix.Getxattr(whitelist, accessACL, nil); !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP) {
		t.Fatal("kept an inherited ACL", err)
	}
	if info, _ := os.Stat(whitelist); info.Mode() != 0o640 {
		t.Fatal("restored a refused ACL's file as", info.Mode())
	}
	for _, unsafe := range []string{
		`{"acl":"user::rw-,user:steve:rw-,group::r--,mask::rw-,other::---","name":"named","type":"file","size":0,"uid":0,"gid":0,"mode":420}`,
		`{"acl":"user::rw-,user:4242:rw-,group::r--,other::---","name":"unmasked","type":"file","size":0,"uid":0,"gid":0,"mode":420}`,
		`{"defaultAcl":"user::rwx,group::r-x,other::r-x","name":"inherits","type":"file","size":0,"uid":0,"gid":0,"mode":420}`,
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
	if info, _ := os.Stat(root); info.Mode().Perm() != 0o755 {
		t.Fatal("restored the root as", info.Mode())
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

func TestRenamesNeverClobber(t *testing.T) {
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
	// A whole folder's undo sets aside as whole restores always moved there,
	// but a numbered name, new with this, needs RENAME_NOREPLACE.
	os.Mkdir(filepath.Join(dir, "aside"), 0o700)
	aside, err := unix.Open(filepath.Join(dir, "aside"), unix.O_DIRECTORY|unix.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(aside)
	os.Mkdir(filepath.Join(dir, "aside", "folder"), 0o700)
	if err := setAsideEntry(parent, "folder", fd(aside), stat("folder"), false); err == nil {
		t.Fatal("set aside under a numbered name without RENAME_NOREPLACE")
	}
	os.Remove(filepath.Join(dir, "aside", "folder"))
	if err := setAsideEntry(parent, "folder", fd(aside), stat("folder"), false); err != nil {
		t.Fatal(err)
	}
	renameat2 = original
	os.Mkdir(filepath.Join(dir, "folder"), 0o755)
	if err := setAsideEntry(parent, "folder", fd(aside), stat("folder"), false); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, "aside", "folder (2)")); err != nil || !info.IsDir() {
		t.Fatal("not set aside under a free name", err)
	}
	// With RENAME_NOREPLACE, a file made at the name between the check and
	// the move is kept.
	renameat2 = func(fromFD int, from string, toFD int, to string, flags uint) error {
		os.WriteFile(filepath.Join(dir, "aside", to), []byte("made meanwhile"), 0o644)
		return original(fromFD, from, toFD, to, flags)
	}
	if err := setAsideEntry(parent, "b.txt", fd(aside), stat("b.txt"), false); err == nil {
		t.Fatal("replaced a file made meanwhile")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "aside", "b.txt")); string(data) != "made meanwhile" {
		t.Fatal("destination changed:", string(data))
	}
	renameat2 = original
	// A file with another name is never moved, even to keep it.
	root := filepath.Join(t.TempDir(), "data")
	stage := StageName("6f9619ff-8b86-4011-b42d-00c04fc964ff")
	os.MkdirAll(filepath.Join(root, stage, "old"), 0o700)
	os.WriteFile(filepath.Join(root, stage, "old", "ops.json"), []byte("original"), 0o644)
	os.WriteFile(filepath.Join(root, "ops.json"), []byte("game made"), 0o644)
	os.Link(filepath.Join(root, "ops.json"), filepath.Join(root, "linked"))
	argument, _ := json.Marshal(RestoreRequest{Root: root, Stage: stage, Operation: RestoreRollbackOld,
		Aside: SetAsideName("6f9619ff-8b86-4011-b42d-00c04fc964ff"), Base: root})
	if RunRestore(string(argument), io.Discard, io.Discard) == 0 {
		t.Fatal("moved a hard-linked file")
	}
	if data, _ := os.ReadFile(filepath.Join(root, "ops.json")); string(data) != "game made" {
		t.Fatal("live copy changed:", string(data))
	}
	os.Remove(filepath.Join(root, "linked"))
	// Nor is an original with another name, and what is in its way stays put.
	os.Link(filepath.Join(root, stage, "old", "ops.json"), filepath.Join(root, stage, "snapshot"))
	if RunRestore(string(argument), io.Discard, io.Discard) == 0 {
		t.Fatal("returned a hard-linked original")
	}
	if data, _ := os.ReadFile(filepath.Join(root, "ops.json")); string(data) != "game made" {
		t.Fatal("set aside before refusing:", string(data))
	}
	os.Remove(filepath.Join(root, stage, "snapshot"))
	// With something in the way, a whole folder's return needs
	// RENAME_NOREPLACE too, so without it nothing moves.
	renameat2 = func(int, string, int, string, uint) error { return unix.EINVAL }
	if RunRestore(string(argument), io.Discard, io.Discard) == 0 {
		t.Fatal("set aside without RENAME_NOREPLACE")
	}
	renameat2 = original
	if data, _ := os.ReadFile(filepath.Join(root, "ops.json")); string(data) != "game made" {
		t.Fatal("moved the live copy:", string(data))
	}
	if data, _ := os.ReadFile(filepath.Join(root, stage, "old", "ops.json")); string(data) != "original" {
		t.Fatal("moved the original:", string(data))
	}
	// Something else at the set-aside folder's name, which nothing needed,
	// doesn't keep the originals from returning.
	other := filepath.Join(t.TempDir(), "data")
	os.MkdirAll(filepath.Join(other, stage, "old"), 0o700)
	os.WriteFile(filepath.Join(other, stage, "old", "ops.json"), []byte("original"), 0o644)
	os.WriteFile(filepath.Join(other, SetAsideName("6f9619ff-8b86-4011-b42d-00c04fc964ff")), nil, 0o644)
	plain, _ := json.Marshal(RestoreRequest{Root: other, Stage: stage, Operation: RestoreRollbackOld,
		Aside: SetAsideName("6f9619ff-8b86-4011-b42d-00c04fc964ff"), Base: other})
	var said bytes.Buffer
	if RunRestore(string(plain), &said, io.Discard) != 0 || !strings.Contains(said.String(), `"kept":false`) {
		t.Fatal("returned nothing past a file at the set-aside name:", said.String())
	}
	// One item's return needs RENAME_NOREPLACE, so without it nothing moves.
	os.MkdirAll(filepath.Join(root, "world"), 0o755)
	os.WriteFile(filepath.Join(root, "world", "level.dat"), []byte("game made"), 0o644)
	os.WriteFile(filepath.Join(root, stage, "old", "item"), []byte("original"), 0o644)
	item, _ := json.Marshal(RestoreRequest{Root: root, Stage: stage, Operation: RestoreRollbackOld, Item: "world/level.dat",
		Aside: SetAsideName("6f9619ff-8b86-4011-b42d-00c04fc964ff"), Base: root})
	renameat2 = func(int, string, int, string, uint) error { return unix.EINVAL }
	if RunRestore(string(item), io.Discard, io.Discard) == 0 {
		t.Fatal("returned one item without RENAME_NOREPLACE")
	}
	renameat2 = original
	if data, _ := os.ReadFile(filepath.Join(root, "world", "level.dat")); string(data) != "game made" {
		t.Fatal("moved the live copy:", string(data))
	}
	// A retry makes the set-aside folder durable before saying it holds a
	// copy, though nothing is left in the way.
	if RunRestore(string(item), io.Discard, io.Discard) != 0 {
		t.Fatal("returned nothing")
	}
	var synced []string
	fsync = func(descriptor int) error {
		path, _ := os.Readlink("/proc/self/fd/" + strconv.Itoa(descriptor))
		synced = append(synced, path)
		return unix.Fsync(descriptor)
	}
	t.Cleanup(func() { fsync = unix.Fsync })
	var out bytes.Buffer
	if RunRestore(string(item), &out, io.Discard) != 0 || !strings.Contains(out.String(), `"kept":true`) ||
		!slices.Contains(synced, filepath.Join(root, SetAsideName("6f9619ff-8b86-4011-b42d-00c04fc964ff"))) {
		t.Fatalf("retry said %s, synced %q", out.String(), synced)
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

// An excluded mount's entry stays out of reach under any spelling. A
// case-insensitive share can't be mounted here, so the test makes the other
// spelling the same way that storage would, with a rename right after the
// helper resolves its exclusions: it proves that once resolved, every
// operation refuses the entry by identity, whatever spelling reaches it; it
// doesn't prove how a particular filesystem folds the destination's own
// spelling when it is resolved.
func TestExclusionsHoldByIdentity(t *testing.T) {
	root, _ := dataRoot(t)
	for _, folder := range []string{"mods", "a/deep"} {
		os.MkdirAll(filepath.Join(root, folder), 0o755)
		os.WriteFile(filepath.Join(root, folder, "hidden.txt"), []byte("hidden"), 0o644)
	}
	blocked := []string{root + "/mods", root + "/a/deep"}
	swap := func(from, to string) {
		os.Rename(filepath.Join(root, from[:4]), filepath.Join(root, to[:4]))
		os.Rename(filepath.Join(root, from[5:]), filepath.Join(root, to[5:]))
	}
	exclusionsResolved = func() { swap("mods a", "MODS A") }
	defer func() { exclusionsResolved = func() {} }()
	id := "6f9619ff-8b86-4011-b42d-00c04fc964ff"
	size := int64(5)
	for _, request := range []FileRequest{
		{Operation: "list", Path: "MODS"},
		{Operation: "download", Path: "MODS/hidden.txt"},
		{Operation: "upload", Path: "MODS/new.txt", Size: &size, UploadID: id},
		{Operation: "upload-prepare", Path: "MODS"},
		{Operation: "mkdir", Path: "MODS/new"},
		{Operation: "delete", Path: "MODS"},
		{Operation: "rename", Path: "MODS", Destination: "other"},
		// A folder that holds one can't be taken as a whole either.
		{Operation: "delete", Path: "A"},
		{Operation: "download", Path: "A"},
		{Operation: "move", Paths: []string{"A"}, Destination: "world"},
		{Operation: "delete-paths", Paths: []string{"A"}},
	} {
		swap("MODS A", "mods a")
		request.Root, request.Blocked = root, blocked
		if _, code := runFile(t, request, []byte("12345"+id)); code == 0 {
			t.Error(request.Operation, request.Path, request.Paths, "reached an excluded entry")
		}
	}
	swap("MODS A", "mods a")
	listing, code := runFile(t, FileRequest{Operation: "list", Root: root, Blocked: blocked}, nil)
	if code != 0 || strings.Contains(listing, "MODS") || !strings.Contains(listing, `"A"`) {
		t.Fatal("listing", code, listing)
	}
	if got := listTree(t, filepath.Join(root, "MODS")); !slices.Equal(got, []string{"hidden.txt"}) {
		t.Fatal(got)
	}
	if got := listTree(t, filepath.Join(root, "A")); !slices.Equal(got, []string{"deep", "deep/hidden.txt"}) {
		t.Fatal(got)
	}
	// Everything else still works, with the exclusions where they were.
	exclusionsResolved = func() {}
	swap("MODS A", "mods a")
	if _, code := runFile(t, FileRequest{Operation: "upload", Root: root, Path: "fine.txt", Size: &size, UploadID: id, Blocked: blocked},
		[]byte("12345"+id)); code != 0 {
		t.Fatal("refused an upload away from the exclusions", code)
	}
	// An exclusion that can't be resolved stops writes into the deepest
	// folder found on its way, under any spelling, where an entry could
	// become it; not reads, and not writes elsewhere.
	exclusionsResolved = func() {}
	missing := []string{root + "/missing"}
	for _, request := range []FileRequest{
		{Operation: "mkdir", Path: "MISSING"},
		{Operation: "move", Path: "world", Paths: []string{"level.dat"}},
	} {
		request.Root, request.Blocked = root, missing
		if _, code := runFile(t, request, nil); code == 0 {
			t.Error(request.Operation, "wrote where an exclusion it couldn't find could appear")
		}
	}
	for _, request := range []FileRequest{{Operation: "list"}, {Operation: "mkdir", Path: "world/new"}, {Operation: "upload-prepare", Path: "world"},
		{Operation: "move", Path: "world", Paths: []string{"level.dat"}, Destination: "private"}} {
		request.Root, request.Blocked = root, missing
		if _, code := runFile(t, request, nil); code != 0 {
			t.Error(request.Operation, "refused away from an exclusion it couldn't find", code)
		}
	}
	// One that appears after the start, in a folder reached under another
	// spelling, is refused like a resolved one.
	os.MkdirAll(filepath.Join(root, "b"), 0o755)
	os.MkdirAll(filepath.Join(root, "parked"), 0o755)
	os.WriteFile(filepath.Join(root, "parked", "hidden.txt"), []byte("hidden"), 0o644)
	exclusionsResolved = func() {
		os.Rename(filepath.Join(root, "b"), filepath.Join(root, "B"))
		os.Rename(filepath.Join(root, "parked"), filepath.Join(root, "B", "missing"))
	}
	late := []string{root + "/b/missing"}
	for _, request := range []FileRequest{
		{Operation: "list", Path: "B"},
		{Operation: "download", Path: "B/missing/hidden.txt"},
		{Operation: "upload", Path: "B/missing/new.txt", Size: &size, UploadID: id},
		{Operation: "mkdir", Path: "B/missing/new"},
		{Operation: "delete", Path: "B/missing"},
		{Operation: "rename", Path: "B/missing", Destination: "B/other"},
	} {
		os.Rename(filepath.Join(root, "B", "missing"), filepath.Join(root, "parked"))
		os.Rename(filepath.Join(root, "B"), filepath.Join(root, "b"))
		request.Root, request.Blocked = root, late
		out, code := runFile(t, request, []byte("12345"+id))
		if request.Operation == "list" && (code != 0 || strings.Contains(out, "missing")) {
			t.Error("listed an exclusion that appeared later", code, out)
		} else if request.Operation != "list" && code == 0 {
			t.Error(request.Operation, "reached an exclusion that appeared later")
		}
	}
	if got := listTree(t, filepath.Join(root, "B", "missing")); !slices.Equal(got, []string{"hidden.txt"}) {
		t.Fatal(got)
	}
	// So is one that appears in a folder that replaced the one it stopped at.
	// A second spelling of a folder can't be made here, so a second name for
	// the same file, a hard link, stands in for it.
	os.MkdirAll(filepath.Join(root, "c"), 0o755)
	exclusionsResolved = func() {
		os.Rename(filepath.Join(root, "c"), filepath.Join(root, "c-old"))
		os.Mkdir(filepath.Join(root, "c"), 0o755)
		os.WriteFile(filepath.Join(root, "c", "missing"), []byte("hidden"), 0o644)
		os.Link(filepath.Join(root, "c", "missing"), filepath.Join(root, "alias"))
	}
	listing, code = runFile(t, FileRequest{Operation: "list", Root: root, Blocked: []string{root + "/c/missing"}}, nil)
	if code != 0 || strings.Contains(listing, `"alias"`) {
		t.Fatal("listed an exclusion that appeared in a replaced folder", code, listing)
	}
	// A folder that comes to be on the way to one, in a replaced tree, can't
	// be renamed or deleted as a whole. Spelled with a doubled slash, the
	// exclusion's path differs from the request's for the byte comparison
	// while the walk opens the same folders, as a loosely matching name would.
	os.MkdirAll(filepath.Join(root, "e"), 0o755)
	exclusionsResolved = func() {
		os.Rename(filepath.Join(root, "e"), filepath.Join(root, "e-old"))
		os.MkdirAll(filepath.Join(root, "e", "f"), 0o755)
	}
	for _, request := range []FileRequest{{Operation: "rename", Path: "e", Destination: "x"}, {Operation: "delete", Path: "e"}} {
		request.Root, request.Blocked = root, []string{root + "//e/f/missing"}
		if _, code := runFile(t, request, nil); code == 0 {
			t.Error(request.Operation, "took a folder now on the way to an exclusion")
		}
		os.Rename(filepath.Join(root, "e-old"), filepath.Join(root, "e-older"))
	}
	if _, err := os.Stat(filepath.Join(root, "e", "f")); err != nil {
		t.Fatal(err)
	}
	// One present at the start and replaced after it by another folder is
	// refused both as the folder its path names now and as the one it named.
	for _, folder := range []string{"g", "spare"} {
		os.MkdirAll(filepath.Join(root, folder), 0o755)
		os.WriteFile(filepath.Join(root, folder, "hidden.txt"), []byte("hidden"), 0o644)
	}
	exclusionsResolved = func() {
		os.Rename(filepath.Join(root, "g"), filepath.Join(root, "g-old"))
		os.Rename(filepath.Join(root, "spare"), filepath.Join(root, "g"))
	}
	for _, request := range []FileRequest{
		{Operation: "list", Path: "g"},
		{Operation: "download", Path: "g/hidden.txt"},
		{Operation: "upload", Path: "g/new.txt", Size: &size, UploadID: id},
		{Operation: "download", Path: "g-old/hidden.txt"},
		{Operation: "list"},
	} {
		os.Rename(filepath.Join(root, "g"), filepath.Join(root, "spare"))
		os.Rename(filepath.Join(root, "g-old"), filepath.Join(root, "g"))
		request.Root, request.Blocked = root, []string{root + "//g"}
		out, code := runFile(t, request, []byte("12345"+id))
		if request.Path == "" && (code != 0 || strings.Contains(out, `"g"`) || strings.Contains(out, `"g-old"`)) {
			t.Error("listed a replaced exclusion", code, out)
		} else if request.Path != "" && code == 0 {
			t.Error(request.Operation, request.Path, "reached a replaced exclusion")
		}
	}
	// So is one replaced after the current snapshot was taken, part-way
	// through a listing.
	exclusionsResolved = func() {}
	for _, folder := range []string{"h", "spare"} {
		os.MkdirAll(filepath.Join(root, folder), 0o755)
	}
	replaced := false
	rootInfo, _ := os.Stat(root)
	currentTaken = func(folder *unix.Stat_t) {
		if !replaced && folder.Ino == rootInfo.Sys().(*syscall.Stat_t).Ino {
			replaced = true
			os.Rename(filepath.Join(root, "h"), filepath.Join(root, "h-old"))
			os.Rename(filepath.Join(root, "spare"), filepath.Join(root, "h"))
		}
	}
	listing, code = runFile(t, FileRequest{Operation: "list", Root: root, Blocked: []string{root + "//h"}}, nil)
	currentTaken = func(*unix.Stat_t) {}
	if !replaced || code != 0 || strings.Contains(listing, `"h"`) || strings.Contains(listing, `"h-old"`) {
		t.Fatal("listed an exclusion replaced after its snapshot", replaced, code, listing)
	}
	// A file where a folder on the way should be can't hold the exclusion,
	// so it stays listed.
	exclusionsResolved = func() {}
	os.WriteFile(filepath.Join(root, "plain"), []byte("plain"), 0o644)
	listing, code = runFile(t, FileRequest{Operation: "list", Root: root, Blocked: []string{root + "/plain/missing"}}, nil)
	if code != 0 || !strings.Contains(listing, `"plain"`) {
		t.Fatal("hid a file on the way to an exclusion", code, listing)
	}
}

// A replaced file keeps its access ACL, whose mask the mode bits only show,
// set as the ACL rather than as mode bits; if the ACL can't be kept the file
// isn't replaced; and a replacement whose original had none takes none from
// its folder. A filesystem without ACLs skips the test.
func TestUploadKeepsAccessACL(t *testing.T) {
	root, _ := dataRoot(t)
	target := filepath.Join(root, "server.properties")
	// Owner rw-, user 4242 rw-, owning group r--, mask rw-, others ---: the
	// mode shows 0660, but the owning group may only read.
	entry := func(tag, perm uint16, id uint32) []byte {
		return binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint16(binary.LittleEndian.AppendUint16(nil, tag), perm), id)
	}
	acl := binary.LittleEndian.AppendUint32(nil, 2)
	for _, part := range [][]byte{entry(0x01, 6, 0xffffffff), entry(0x02, 6, 4242), entry(0x04, 4, 0xffffffff), entry(0x10, 6, 0xffffffff),
		entry(0x20, 0, 0xffffffff)} {
		acl = append(acl, part...)
	}
	if err := unix.Setxattr(target, accessACL, acl, 0); err != nil {
		t.Skip("no ACLs here:", err)
	}
	id := "6f9619ff-8b86-4011-b42d-00c04fc964ff"
	size := int64(5)
	upload := FileRequest{Operation: "upload", Root: root, Path: "server.properties", Size: &size, UploadID: id}
	if _, code := runFile(t, upload, []byte("hello"+id)); code != 0 {
		t.Fatal("replace", code)
	}
	kept := make([]byte, 64)
	if n, err := unix.Getxattr(target, accessACL, kept); err != nil || !bytes.Equal(kept[:n], acl) {
		t.Fatal("ACL not kept", err)
	}
	fsetxattr = func(int, string, []byte, int) error { return unix.EIO }
	defer func() { fsetxattr = unix.Fsetxattr }()
	if _, code := runFile(t, upload, []byte("other"+id)); code != FileACLNotKept {
		t.Fatal("replaced a file whose ACL couldn't be kept", code)
	}
	if data, _ := os.ReadFile(target); string(data) != "hello" {
		t.Fatal(string(data))
	}
	fsetxattr = unix.Fsetxattr
	// The ACL sets the mode itself: the mask is never the group's on its own.
	fchmod = func(int, uint32) error { return errors.New("set the mode bits of a file with an ACL") }
	defer func() { fchmod = unix.Fchmod }()
	if _, code := runFile(t, upload, []byte("third"+id)); code != 0 {
		t.Fatal("replace", code)
	}
	fchmod = unix.Fchmod
	// A replacement whose original had no ACL keeps none from its folder's
	// default ACL.
	plain := filepath.Join(root, "plain.txt")
	os.WriteFile(plain, []byte("plain"), 0o640)
	unix.Setxattr(root, "system.posix_acl_default", acl, 0)
	replace := FileRequest{Operation: "upload", Root: root, Path: "plain.txt", Size: &size, UploadID: id}
	if _, code := runFile(t, replace, []byte("hello"+id)); code != 0 {
		t.Fatal("replace", code)
	}
	if _, err := unix.Getxattr(plain, accessACL, kept); !errors.Is(err, unix.ENODATA) {
		t.Fatal("a replacement took its folder's default ACL", err)
	}
	// A new file gets what creating it with mode 0666 in its folder gives:
	// the default ACL with its owner, mask, and other entries cut down by
	// that mode, not by the staged 0600 or the umask. On ext4 and tmpfs that
	// is mode 0440 with this ACL.
	unix.Setxattr(root, defaultACL, mustACL(t, "user::r-x,user:4242:rwx,group::rwx,group:4343:rw-,mask::r-x,other::---"), 0)
	created := FileRequest{Operation: "upload", Root: root, Path: "new.txt", Size: &size, UploadID: id}
	if _, code := runFile(t, created, []byte("hello"+id)); code != 0 {
		t.Fatal("create", code)
	}
	reference, _ := os.OpenFile(filepath.Join(root, "reference.txt"), os.O_CREATE|os.O_WRONLY, 0o666)
	reference.Close()
	sameAsCreated(t, filepath.Join(root, "new.txt"), filepath.Join(root, "reference.txt"),
		"user::r--,user:4242:rwx,group::rwx,group:4343:rw-,mask::r--,other::---", 0o440)
	// When the owner changes, what the future owner may hold until then, its
	// own named entry or else what the owning group, a named group, or
	// others get, is capped at the owner's rights; other named users keep
	// what the folder's ACL gives them.
	future := strconv.Itoa(os.Getuid())
	helperUID = func() int { return -2 }
	defer func() { helperUID = os.Geteuid }()
	for index, test := range []struct {
		folder, want string
		mode         os.FileMode
	}{
		{"user::r-x,user:4242:rwx,group::rw-,group:4343:rw-,mask::rwx,other::rw-", "user::r--,user:4242:rwx,group::r--,group:4343:r--,mask::rw-,other::r--", 0o464},
		{"user::r-x,group::rwx,other::---", "", 0o440},
		{"user::r-x,user:" + future + ":rwx,group::rw-,mask::rwx,other::rw-", "user::r--,user:" + future + ":r-x,group::rw-,mask::rw-,other::rw-", 0o466},
	} {
		unix.Setxattr(root, defaultACL, mustACL(t, test.folder), 0)
		name := "handed-" + strconv.Itoa(index)
		if _, code := runFile(t, FileRequest{Operation: "upload", Root: root, Path: name, Size: &size, UploadID: id}, []byte("hello"+id)); code != 0 {
			t.Fatal("create", code)
		}
		value := make([]byte, 256)
		n, _ := unix.Getxattr(filepath.Join(root, name), accessACL, value)
		text, _ := ACLText(value[:max(n, 0)])
		if info, _ := os.Stat(filepath.Join(root, name)); text != test.want || info.Mode() != test.mode {
			t.Errorf("%s: %s %v", test.folder, text, info.Mode())
		}
	}
}

// sameAsCreated fails unless got has the mode and ACLs of reference, which
// the kernel created, and its access ACL is want.
func sameAsCreated(t *testing.T, got, reference, want string, mode os.FileMode) {
	t.Helper()
	for _, attribute := range []string{accessACL, defaultACL} {
		values := [2][]byte{}
		for index, path := range []string{got, reference} {
			value := make([]byte, 256)
			n, err := unix.Getxattr(path, attribute, value)
			if err != nil && !errors.Is(err, unix.ENODATA) {
				t.Fatal(path, attribute, err)
			}
			values[index] = value[:max(n, 0)]
		}
		if !bytes.Equal(values[0], values[1]) {
			t.Errorf("%s: %s %x, created %x", got, attribute, values[0], values[1])
		}
		if text, _ := ACLText(values[0]); attribute == accessACL && text != want {
			t.Errorf("%s: %s", got, text)
		}
	}
	for _, path := range []string{got, reference} {
		if info, _ := os.Stat(path); info == nil || info.Mode()&^os.ModeType != mode {
			t.Errorf("%s is %v", path, info)
		}
	}
}
