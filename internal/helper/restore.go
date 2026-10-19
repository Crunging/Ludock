package helper

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// RestoreRequest is the restore helper protocol.
type RestoreRequest struct {
	Root      string `json:"root"`
	Stage     string `json:"stage"`
	Operation string `json:"operation"`
}

// RestoreFailure is the only error text the restore helper prints.
const RestoreFailure = "Restore data changed, is unsafe, or cannot be accessed. Keep the server stopped and review recovery."

var stagePattern = regexp.MustCompile(`^\.ludock-restore-[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type restoreHelper struct {
	request RestoreRequest
	t       tracker
	removed int
}

func (h *restoreHelper) pinRoot() (fd, error) {
	root := h.request.Root
	if !strings.HasPrefix(root, "/") || root == "/" || strings.Contains(root, "\x00") {
		return -1, errUnsafe
	}
	parts := strings.Split(root, "/")[1:]
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return -1, errUnsafe
		}
	}
	return h.t.walk("/", parts)
}

func syncAll(descriptors ...fd) error {
	seen := map[fd]bool{}
	for _, descriptor := range descriptors {
		if seen[descriptor] {
			continue
		}
		seen[descriptor] = true
		if err := unix.Fsync(int(descriptor)); err != nil {
			return err
		}
	}
	return nil
}

func (h *restoreHelper) unchanged(parent fd, name string, handle fd) error {
	current, err := lstatAt(parent, name)
	if err != nil {
		return err
	}
	pinned, err := fstat(handle)
	if err != nil {
		return err
	}
	if !isDir(current) || !sameInode(current, pinned) {
		return errUnsafe
	}
	return nil
}

// moveContents moves entries one at a time. rename never follows its final
// component: a racing symlink can be moved, but cannot turn this into a
// write through that symlink to outside data.
func (h *restoreHelper) moveContents(source, destination fd, omitted string) error {
	children, err := names(source)
	if err != nil {
		return err
	}
	if len(children) > 1000000 {
		return errUnsafe
	}
	for _, name := range children {
		if name == omitted {
			continue
		}
		if strings.HasPrefix(name, ".ludock-restore-") {
			return errUnsafe
		}
		sourceInfo, err := lstatAt(source, name)
		if err != nil {
			return err
		}
		if !isDir(sourceInfo) && (!isRegular(sourceInfo) || sourceInfo.Nlink != 1) {
			return errUnsafe
		}
		if _, err := lstatAt(destination, name); err == nil {
			return errUnsafe
		} else if !isNotExist(err) {
			return err
		}
		if err := unix.Renameat(int(source), name, int(destination), name); err != nil {
			return err
		}
		moved, err := lstatAt(destination, name)
		if err != nil {
			return err
		}
		if !sameInode(sourceInfo, moved) {
			return errUnsafe
		}
		if err := syncAll(source, destination); err != nil {
			return err
		}
	}
	return nil
}

func (h *restoreHelper) removeEntry(parent fd, name string, depth int) error {
	h.removed++
	if depth > 64 || h.removed > 1000000 {
		return errUnsafe
	}
	before, err := lstatAt(parent, name)
	if err != nil {
		return err
	}
	if isDir(before) {
		directory, err := h.t.openAt(parent, name, directoryFlags, 0)
		if err != nil {
			return err
		}
		pinned, err := fstat(directory)
		if err != nil {
			return err
		}
		if !sameInode(before, pinned) {
			return errUnsafe
		}
		children, err := names(directory)
		if err != nil {
			return err
		}
		for _, child := range children {
			if err := h.removeEntry(directory, child, depth+1); err != nil {
				return err
			}
		}
		if err := syncAll(directory); err != nil {
			return err
		}
		// Refuse to unlink a substituted directory after emptying the pinned one.
		if err := h.unchanged(parent, name, directory); err != nil {
			return err
		}
		if err := unix.Unlinkat(int(parent), name, unix.AT_REMOVEDIR); err != nil {
			return err
		}
		h.t.close(directory)
	} else {
		// Unlink removes a link itself, never its target. Special files are
		// rejected so cleanup never operates on devices or sockets.
		if !isRegular(before) && !isSymlink(before) {
			return errUnsafe
		}
		if err := unix.Unlinkat(int(parent), name, 0); err != nil {
			return err
		}
	}
	return syncAll(parent)
}

func (h *restoreHelper) pinStage(root fd, optional bool) (fd, error) {
	stage, err := h.t.openAt(root, h.request.Stage, directoryFlags, 0)
	if optional && isNotExist(err) {
		return -1, nil
	}
	return stage, err
}

func (h *restoreHelper) run() (any, error) {
	if !stagePattern.MatchString(h.request.Stage) {
		return nil, errUnsafe
	}
	root, err := h.pinRoot()
	if err != nil {
		return nil, err
	}
	ok := map[string]bool{"ok": true}
	switch h.request.Operation {
	case "space":
		var stat unix.Statfs_t
		if err := unix.Fstatfs(int(root), &stat); err != nil {
			return nil, err
		}
		bytes := uint64(stat.Bavail) * uint64(stat.Bsize)
		if bytes > 1<<53-1 {
			return nil, errUnsafe
		}
		return map[string]uint64{"availableBytes": bytes}, nil
	case "stage":
		if err := unix.Mkdirat(int(root), h.request.Stage, 0o700); err != nil {
			return nil, err
		}
		stage, err := h.pinStage(root, false)
		if err != nil {
			return nil, err
		}
		for _, name := range []string{"new", "old"} {
			if err := unix.Mkdirat(int(stage), name, 0o700); err != nil {
				return nil, err
			}
		}
		next, err := h.t.openAt(stage, "new", directoryFlags, 0)
		if err != nil {
			return nil, err
		}
		old, err := h.t.openAt(stage, "old", directoryFlags, 0)
		if err != nil {
			return nil, err
		}
		return ok, syncAll(next, old, stage, root)
	}
	stage, err := h.pinStage(root, h.request.Operation == "cleanup")
	if err != nil {
		return nil, err
	}
	if stage < 0 {
		return ok, nil
	}
	switch h.request.Operation {
	case "cleanup":
		if err := h.unchanged(root, h.request.Stage, stage); err != nil {
			return nil, err
		}
		children, err := names(stage)
		if err != nil {
			return nil, err
		}
		for _, name := range children {
			if err := h.removeEntry(stage, name, 0); err != nil {
				return nil, err
			}
		}
		if err := syncAll(stage); err != nil {
			return nil, err
		}
		if err := h.unchanged(root, h.request.Stage, stage); err != nil {
			return nil, err
		}
		if err := unix.Unlinkat(int(root), h.request.Stage, unix.AT_REMOVEDIR); err != nil {
			return nil, err
		}
		return ok, syncAll(root)
	case "moveOld":
		old, err := h.t.openAt(stage, "old", directoryFlags, 0)
		if err != nil {
			return nil, err
		}
		err = h.moveContents(root, old, h.request.Stage)
		if err != nil {
			return nil, err
		}
	case "moveNew":
		next, err := h.t.openAt(stage, "new", directoryFlags, 0)
		if err != nil {
			return nil, err
		}
		if err := h.moveContents(next, root, ""); err != nil {
			return nil, err
		}
	case "rollbackClean":
		// Prove the old data exists before deleting a failed replacement.
		if _, err := h.t.openAt(stage, "old", directoryFlags, 0); err != nil {
			return nil, err
		}
		children, err := names(root)
		if err != nil {
			return nil, err
		}
		for _, name := range children {
			if name == h.request.Stage {
				continue
			}
			if strings.HasPrefix(name, ".ludock-restore-") {
				return nil, errUnsafe
			}
			if err := h.removeEntry(root, name, 0); err != nil {
				return nil, err
			}
		}
	case "rollbackOld":
		old, err := h.t.openAt(stage, "old", directoryFlags, 0)
		if err != nil {
			return nil, err
		}
		if err := h.moveContents(old, root, ""); err != nil {
			return nil, err
		}
	default:
		return nil, errUnsafe
	}
	return ok, syncAll(stage, root)
}

// RunRestore runs one restore step for a JSON request.
func RunRestore(argument string, stdout, stderr io.Writer) int {
	h := &restoreHelper{}
	defer h.t.closeAll()
	var result any
	err := json.Unmarshal([]byte(argument), &h.request)
	if err == nil {
		result, err = h.run()
	}
	if err != nil {
		io.WriteString(stderr, RestoreFailure)
		return 1
	}
	encoded, _ := json.Marshal(result)
	stdout.Write(encoded)
	return 0
}

// RestoreRecord describes one extracted entry.
type RestoreRecord struct {
	Name  string   `json:"name"`
	Type  string   `json:"type"`
	Size  *float64 `json:"size"`
	UID   *float64 `json:"uid"`
	GID   *float64 `json:"gid"`
	Mode  *float64 `json:"mode"`
	Mtime *float64 `json:"mtime"`
}

// ExtractFailure is the only error text the extractor prints.
const ExtractFailure = "Restore extraction failed: the archive or destination changed or is unsafe."

var extractStage = regexp.MustCompile(`^\.ludock-restore-[0-9a-f-]{36}$`)

func safeInteger(value *float64, low, high float64) (int64, bool) {
	if value == nil || *value != math.Trunc(*value) || *value < low || *value > high || math.Abs(*value) > 1<<53-1 {
		return 0, false
	}
	return int64(*value), true
}

func recordParts(name string) ([]string, error) {
	if strings.Contains(name, "\x00") || strings.Contains(name, "\\") || len(name) > 4096 {
		return nil, errUnsafe
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errUnsafe
		}
	}
	return parts, nil
}

type extractor struct {
	t           tracker
	destination fd
}

// parent creates (0700) and pins the directories above an entry.
func (x *extractor) parent(names []string) (fd, error) {
	current := x.destination
	for _, name := range names {
		if err := unix.Mkdirat(int(current), name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, err
		}
		next, err := x.t.openAt(current, name, directoryFlags, 0)
		if err != nil {
			return -1, err
		}
		if err := unix.Fsync(int(current)); err != nil {
			return -1, err
		}
		if current != x.destination {
			x.t.close(current)
		}
		current = next
	}
	return current, nil
}

func metadata(descriptor fd, uid, gid, mode int64, mtime *float64) error {
	if err := unix.Fchown(int(descriptor), int(uid), int(gid)); err != nil {
		return err
	}
	if err := unix.Fchmod(int(descriptor), uint32(mode&0o777)); err != nil {
		return err
	}
	if mtime != nil && !math.IsInf(*mtime, 0) && !math.IsNaN(*mtime) {
		seconds := math.Floor(*mtime)
		nanos := int64((*mtime - seconds) * 1e9)
		times := [2]unix.Timespec{{Sec: int64(seconds), Nsec: nanos}, {Sec: int64(seconds), Nsec: nanos}}
		// utimensat with a NULL path changes the descriptor's own file.
		if _, _, errno := unix.Syscall6(unix.SYS_UTIMENSAT, uintptr(descriptor), 0,
			uintptr(unsafe.Pointer(&times[0])), 0, 0, 0); errno != 0 {
			return errno
		}
	}
	return nil
}

// RunRestoreExtract writes a staged restore from stdin: each entry is one
// JSON record line followed by exactly its file bytes. Directories get their
// metadata last so file writes cannot change their times or modes.
func RunRestoreExtract(argument string, stdin io.Reader, stdout, stderr io.Writer) int {
	x := &extractor{}
	defer x.t.closeAll()
	if err := x.run(argument, stdin); err != nil {
		io.WriteString(stderr, ExtractFailure)
		return 1
	}
	io.WriteString(stdout, "ok")
	return 0
}

func (x *extractor) run(argument string, stdin io.Reader) error {
	var request struct {
		Root     string  `json:"root"`
		Stage    string  `json:"stage"`
		MaxBytes float64 `json:"maxBytes"`
	}
	if json.Unmarshal([]byte(argument), &request) != nil || !strings.HasPrefix(request.Root, "/") || !extractStage.MatchString(request.Stage) {
		return errUnsafe
	}
	rootParts, err := recordParts(request.Root[1:])
	if err != nil {
		return err
	}
	root, err := x.t.walk("/", rootParts)
	if err != nil {
		return err
	}
	stage, err := x.t.openAt(root, request.Stage, directoryFlags, 0)
	if err != nil {
		return err
	}
	if x.destination, err = x.t.openAt(stage, "new", directoryFlags, 0); err != nil {
		return err
	}
	type directoryRecord struct {
		names          []string
		uid, gid, mode int64
		mtime          *float64
	}
	var directories []directoryRecord
	reader := bufio.NewReaderSize(stdin, 64*1024)
	count := 0
	var total float64
	for {
		line, err := reader.ReadSlice('\n')
		if err == bufio.ErrBufferFull || len(line) > 16385 {
			return errUnsafe
		}
		if err == io.EOF {
			if len(line) > 0 {
				return errUnsafe
			}
			break
		}
		if err != nil {
			return err
		}
		var record RestoreRecord
		if json.Unmarshal(bytes.TrimSuffix(line, []byte("\n")), &record) != nil {
			return errUnsafe
		}
		count++
		size, okSize := safeInteger(record.Size, 0, 1<<53-1)
		uid, okUID := safeInteger(record.UID, 0, 4294967294)
		gid, okGID := safeInteger(record.GID, 0, 4294967294)
		mode, okMode := safeInteger(record.Mode, -(1<<53 - 1), 1<<53-1)
		if count > 100000 || (record.Type != "file" && record.Type != "directory") || !okSize || !okUID || !okGID || !okMode {
			return errUnsafe
		}
		total += float64(size)
		if total > request.MaxBytes {
			return errUnsafe
		}
		names, err := recordParts(record.Name)
		if err != nil || len(names) > 64 {
			return errUnsafe
		}
		for _, name := range names {
			if strings.HasPrefix(name, ".ludock-restore-") {
				return errUnsafe
			}
		}
		owner, err := x.parent(names[:len(names)-1])
		if err != nil {
			return err
		}
		name := names[len(names)-1]
		if record.Type == "directory" {
			if size != 0 {
				return errUnsafe
			}
			if err := unix.Mkdirat(int(owner), name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				return err
			}
			directory, err := x.t.openAt(owner, name, directoryFlags, 0)
			if err != nil {
				return err
			}
			x.t.close(directory)
			directories = append(directories, directoryRecord{names, uid, gid, mode, record.Mtime})
		} else {
			file, err := x.t.openAt(owner, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.CopyN(fdWriter(file), reader, size); err != nil {
				return errUnsafe
			}
			if err := metadata(file, uid, gid, mode, record.Mtime); err != nil {
				return err
			}
			if err := unix.Fsync(int(file)); err != nil {
				return err
			}
			x.t.close(file)
		}
		if err := unix.Fsync(int(owner)); err != nil {
			return err
		}
		if owner != x.destination {
			x.t.close(owner)
		}
	}
	for index := len(directories) - 1; index >= 0; index-- {
		record := directories[index]
		owner, err := x.parent(record.names[:len(record.names)-1])
		if err != nil {
			return err
		}
		directory, err := x.t.openAt(owner, record.names[len(record.names)-1], directoryFlags, 0)
		if err != nil {
			return err
		}
		if err := metadata(directory, record.uid, record.gid, record.mode, record.mtime); err != nil {
			return err
		}
		if err := unix.Fsync(int(directory)); err != nil {
			return err
		}
		x.t.close(directory)
		if owner != x.destination {
			x.t.close(owner)
		}
	}
	return unix.Fsync(int(x.destination))
}

type fdWriter fd

func (w fdWriter) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		n, err := unix.Write(int(w), p[written:])
		if err != nil {
			return written, err
		}
		written += n
	}
	return written, nil
}
