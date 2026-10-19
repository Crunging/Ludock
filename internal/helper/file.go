package helper

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// FileRequest is the file helper protocol, shared with the backend.
type FileRequest struct {
	Operation   string   `json:"operation"`
	Root        string   `json:"root"`
	Path        string   `json:"path,omitempty"`
	Blocked     []string `json:"blocked,omitempty"`
	Destination string   `json:"destination,omitempty"`
	Size        *int64   `json:"size,omitempty"`
	UploadID    string   `json:"uploadId,omitempty"`
}

// FileEntry is one listed directory entry.
type FileEntry struct {
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	Size       int64   `json:"size"`
	ModifiedAt float64 `json:"modifiedAt"`
}

// FileFailure is the only error text the file helper prints. Paths and raw
// filesystem errors can contain private host information.
const FileFailure = "The file operation failed: the path changed, is unsafe, or cannot be accessed."

type fileHelper struct {
	request FileRequest
	t       tracker
	out     *bufio.Writer
	stdin   io.Reader
	entries int

	exit     func(int)
	mu       sync.Mutex
	aborted  bool
	abortErr error
}

func within(value, root string) bool { return value == root || strings.HasPrefix(value, root+"/") }

func components(value string) ([]string, error) {
	if strings.Contains(value, "\x00") || slices.Contains(strings.Split(value, "/"), "..") {
		return nil, errUnsafe
	}
	var parts []string
	for _, part := range strings.Split(value, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return parts, nil
}

func (h *fileHelper) blocked(value string, recursive bool) bool {
	for _, root := range h.request.Blocked {
		if within(value, root) || (recursive && within(root, value)) {
			return true
		}
	}
	return false
}

func (h *fileHelper) absolute(relative string) string { return path.Join(h.request.Root, relative) }

func (h *fileHelper) check(relative string, recursive bool) error {
	if h.blocked(h.absolute(relative), recursive) {
		return errors.New("this path overlaps an excluded mount")
	}
	return nil
}

func (h *fileHelper) pinRoot() (fd, error) {
	if !strings.HasPrefix(h.request.Root, "/") || h.request.Root == "/" {
		return -1, errUnsafe
	}
	parts, err := components(h.request.Root)
	if err != nil {
		return -1, err
	}
	return h.t.walk("/", parts)
}

func (h *fileHelper) pinDirectory(root fd, relative string) (fd, error) {
	parts, err := components(relative)
	if err != nil {
		return -1, err
	}
	current := root
	for _, part := range parts {
		if current, err = h.t.openAt(current, part, directoryFlags, 0); err != nil {
			return -1, err
		}
	}
	return current, nil
}

func (h *fileHelper) parentOf(root fd, relative string) (fd, string, error) {
	parts, err := components(relative)
	if err != nil {
		return -1, "", err
	}
	if len(parts) == 0 {
		return -1, "", errors.New("the root cannot be changed or downloaded")
	}
	parent, err := h.pinDirectory(root, strings.Join(parts[:len(parts)-1], "/"))
	return parent, parts[len(parts)-1], err
}

var uploadIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func uploadName(id string) (string, error) {
	if !uploadIDPattern.MatchString(id) {
		return "", errors.New("invalid upload identifier")
	}
	return ".ludock-upload-" + id + ".tmp", nil
}

func uploadTarget(parent fd, name string) (*unix.Stat_t, error) {
	info, err := lstatAt(parent, name)
	if isNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !isRegular(info) || info.Nlink != 1 {
		return nil, errors.New("only regular files without hard links are supported")
	}
	return info, nil
}

func (h *fileHelper) json(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = h.out.Write(encoded)
	return err
}

func (h *fileHelper) streamFile(file fd, size int64) error {
	buffer := make([]byte, 64*1024)
	var position int64
	for remaining := size; remaining > 0; {
		n, err := unix.Pread(int(file), buffer[:min(int64(len(buffer)), remaining)], position)
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("the file changed while it was read")
		}
		if _, err := h.out.Write(buffer[:n]); err != nil {
			return err
		}
		position += int64(n)
		remaining -= int64(n)
	}
	return nil
}

func infoOf(stat *unix.Stat_t) tarInfo {
	return tarInfo{size: stat.Size, uid: int64(stat.Uid), gid: int64(stat.Gid), mode: int64(stat.Mode), mtimeMs: mtimeMs(stat)}
}

func (h *fileHelper) header(name string, stat *unix.Stat_t, directory bool) error {
	header, err := tarHeader(name, infoOf(stat), directory, 0, h.request.Operation == "backup")
	if err != nil {
		return err
	}
	_, err = h.out.Write(header)
	return err
}

func (h *fileHelper) archiveEntry(parent fd, name, relative string, depth int) error {
	h.entries++
	if depth > 64 || h.entries > 100000 {
		return errors.New("the archive has too many entries or directories")
	}
	file, err := h.t.openAt(parent, name, fileFlags, 0)
	if err != nil {
		return err
	}
	info, err := fstat(file)
	if err != nil {
		return err
	}
	if isDir(info) {
		if err := h.header(relative+"/", info, true); err != nil {
			return err
		}
		children, err := names(file)
		if err != nil {
			return err
		}
		for _, child := range children {
			if err := h.archiveEntry(file, child, relative+"/"+child, depth+1); err != nil {
				return err
			}
		}
	} else {
		if !isRegular(info) || info.Nlink != 1 {
			return errors.New("archive cannot include symbolic links, hard links, or special files")
		}
		if err := h.header(relative, info, false); err != nil {
			return err
		}
		if err := h.streamFile(file, info.Size); err != nil {
			return err
		}
		if info.Size%512 != 0 {
			if _, err := h.out.Write(make([]byte, 512-info.Size%512)); err != nil {
				return err
			}
		}
	}
	h.t.close(file)
	return nil
}

func (h *fileHelper) removeEntry(parent fd, name string, depth int) error {
	if depth > 64 {
		return errors.New("the directory is too deeply nested")
	}
	info, err := lstatAt(parent, name)
	if err != nil {
		return err
	}
	if !isDir(info) {
		return unix.Unlinkat(int(parent), name, 0)
	}
	directory, err := h.t.openAt(parent, name, directoryFlags, 0)
	if err != nil {
		return err
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
	h.t.close(directory)
	return unix.Unlinkat(int(parent), name, unix.AT_REMOVEDIR)
}

func (h *fileHelper) abortUpload(err error) {
	h.mu.Lock()
	h.aborted = true
	if h.abortErr == nil {
		h.abortErr = err
	}
	h.mu.Unlock()
	if closer, ok := h.stdin.(io.Closer); ok {
		closer.Close()
	}
	// Closing a blocking pipe may not interrupt its read. The backend's
	// cleanup request removes the staging file after the helper exits.
	if h.exit != nil {
		time.AfterFunc(5*time.Second, func() { h.exit(1) })
	}
}

func (h *fileHelper) uploadAborted() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.aborted
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

func (h *fileHelper) upload(parent fd, name string) error {
	if h.request.Size == nil || *h.request.Size < 0 || *h.request.Size > 1<<53-1 {
		return errors.New("invalid upload size")
	}
	size := *h.request.Size
	id := h.request.UploadID
	if id == "" {
		id = newUUID()
	}
	temporary, err := uploadName(id)
	if err != nil {
		return err
	}
	trailer := []byte(h.request.UploadID)
	original, err := uploadTarget(parent, name)
	if err != nil {
		return err
	}
	owner := original
	if owner == nil {
		if owner, err = fstat(parent); err != nil {
			return err
		}
	}
	file, err := h.t.openAt(parent, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		h.t.close(file)
		if err := unix.Unlinkat(int(parent), temporary, 0); err != nil && !isNotExist(err) {
			// The staging name is fixed; leaving it is reported by the backend's
			// cleanup request.
			_ = err
		}
	}()
	idle := time.AfterFunc(60*time.Second, func() { h.abortUpload(errors.New("upload interrupted")) })
	defer idle.Stop()
	var received, committed int64
	buffer := make([]byte, 64*1024)
	for {
		n, readErr := h.stdin.Read(buffer)
		if n > 0 {
			idle.Reset(60 * time.Second)
			chunk := buffer[:n]
			payload := min(int64(len(chunk)), size-received)
			for offset := int64(0); offset < payload; {
				written, err := unix.Write(int(file), chunk[offset:payload])
				if err != nil {
					return err
				}
				if written == 0 {
					return errors.New("upload write made no progress")
				}
				offset += int64(written)
			}
			received += payload
			tail := chunk[payload:]
			if committed+int64(len(tail)) > int64(len(trailer)) ||
				subtle.ConstantTimeCompare(tail, trailer[committed:committed+int64(len(tail))]) != 1 {
				return errors.New("upload exceeded its declared size or was interrupted")
			}
			committed += int64(len(tail))
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if h.uploadAborted() || received != size || committed != int64(len(trailer)) {
		return errors.New("upload did not match its declared size or was interrupted")
	}
	// Set permissions while the helper still owns the new inode; CHOWN is
	// enough to restore game ownership without granting FOWNER.
	mode := uint32(0o666 &^ currentUmask())
	if original != nil {
		mode = original.Mode & 0o777
	}
	if err := unix.Fchmod(int(file), mode); err != nil {
		return err
	}
	if err := unix.Fchown(int(file), int(owner.Uid), int(owner.Gid)); err != nil {
		return err
	}
	if err := unix.Fsync(int(file)); err != nil {
		return err
	}
	current, err := uploadTarget(parent, name)
	if err != nil {
		return err
	}
	if (current == nil) != (original == nil) || (original != nil && (original.Dev != current.Dev || original.Ino != current.Ino ||
		original.Mode != current.Mode || original.Uid != current.Uid || original.Gid != current.Gid ||
		original.Size != current.Size || mtimeMs(original) != mtimeMs(current) || ctimeMs(original) != ctimeMs(current))) {
		return errors.New("the upload target changed")
	}
	if h.uploadAborted() {
		return errors.New("upload interrupted")
	}
	if original != nil {
		if err := unix.Renameat(int(parent), temporary, int(parent), name); err != nil {
			return err
		}
	} else {
		// link never replaces an existing name, unlike rename.
		if err := unix.Linkat(int(parent), temporary, int(parent), name, 0); err != nil {
			return err
		}
		if err := unix.Unlinkat(int(parent), temporary, 0); err != nil {
			return err
		}
	}
	if err := unix.Fsync(int(parent)); err != nil {
		return err
	}
	return h.json(map[string]bool{"ok": true})
}

func currentUmask() int {
	old := syscall.Umask(0)
	syscall.Umask(old)
	return old
}

func (h *fileHelper) run() error {
	root, err := h.pinRoot()
	if err != nil {
		return err
	}
	parts, err := components(h.request.Path)
	if err != nil {
		return err
	}
	relative := strings.Join(parts, "/")
	operation := h.request.Operation
	if err := h.check(relative, operation == "download" || operation == "delete" || operation == "rename"); err != nil {
		return err
	}
	switch operation {
	case "check":
		return h.json(map[string]bool{"safe": true})
	case "backup":
		if err := h.check(relative, true); err != nil {
			return err
		}
		info, err := fstat(root)
		if err != nil {
			return err
		}
		if err := h.header("source/", info, true); err != nil {
			return err
		}
		children, err := names(root)
		if err != nil {
			return err
		}
		for _, name := range children {
			if err := h.archiveEntry(root, name, "source/"+name, 0); err != nil {
				return err
			}
		}
		_, err = h.out.Write(make([]byte, 1024))
		return err
	case "list":
		directory, err := h.pinDirectory(root, relative)
		if err != nil {
			return err
		}
		children, err := names(directory)
		if err != nil {
			return err
		}
		if len(children) > 10000 {
			return errors.New("this directory has too many entries to list")
		}
		entries := []FileEntry{}
		for _, name := range children {
			if h.blocked(h.absolute(path.Join(relative, name)), false) {
				continue
			}
			info, err := lstatAt(directory, name)
			if err != nil {
				return err
			}
			entry := FileEntry{Name: name, ModifiedAt: mtimeMs(info)}
			switch {
			case isSymlink(info):
				entry.Type = "symlink"
			case isDir(info):
				entry.Type = "directory"
			case isRegular(info):
				entry.Type, entry.Size = "file", info.Size
			default:
				continue
			}
			entries = append(entries, entry)
		}
		return h.json(entries)
	}
	parent, name, err := h.parentOf(root, relative)
	if err != nil {
		return err
	}
	switch operation {
	case "mkdir":
		owner, err := fstat(parent)
		if err != nil {
			return err
		}
		if err := unix.Mkdirat(int(parent), name, 0o777); err != nil {
			return err
		}
		directory, err := h.t.openAt(parent, name, directoryFlags, 0)
		if err != nil {
			return err
		}
		if err := unix.Fchown(int(directory), int(owner.Uid), int(owner.Gid)); err != nil {
			return err
		}
		return h.json(map[string]bool{"ok": true})
	case "delete":
		if err := h.removeEntry(parent, name, 0); err != nil {
			return err
		}
		return h.json(map[string]bool{"ok": true})
	case "rename":
		destinationParts, err := components(h.request.Destination)
		if err != nil {
			return err
		}
		destination := strings.Join(destinationParts, "/")
		if err := h.check(destination, true); err != nil {
			return err
		}
		targetParent, targetName, err := h.parentOf(root, destination)
		if err != nil {
			return err
		}
		info, err := lstatAt(parent, name)
		if err != nil {
			return err
		}
		if isSymlink(info) {
			return errors.New("symbolic links cannot be renamed")
		}
		if _, err := lstatAt(targetParent, targetName); err == nil {
			return errors.New("the destination already exists")
		} else if !isNotExist(err) {
			return err
		}
		if err := unix.Renameat2(int(parent), name, int(targetParent), targetName, unix.RENAME_NOREPLACE); err != nil {
			if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EINVAL) {
				return err
			}
			if err := unix.Renameat(int(parent), name, int(targetParent), targetName); err != nil {
				return err
			}
		}
		return h.json(map[string]bool{"ok": true})
	case "upload":
		return h.upload(parent, name)
	case "upload-cleanup":
		temporary, err := uploadName(h.request.UploadID)
		if err != nil {
			return err
		}
		if err := unix.Unlinkat(int(parent), temporary, 0); err != nil && !isNotExist(err) {
			return err
		}
		return h.json(map[string]bool{"ok": true})
	}
	file, err := h.t.openAt(parent, name, fileFlags, 0)
	if err != nil {
		return err
	}
	info, err := fstat(file)
	if err != nil {
		return err
	}
	if !isDir(info) && (!isRegular(info) || info.Nlink != 1) {
		return errors.New("only regular files and directories can be downloaded")
	}
	switch operation {
	case "stat":
		kind, size := "file", info.Size
		if isDir(info) {
			kind, size = "directory", 0
		}
		return h.json(map[string]any{"type": kind, "size": size})
	case "download":
		if isDir(info) {
			h.t.close(file)
			if err := h.archiveEntry(parent, name, name, 0); err != nil {
				return err
			}
			_, err := h.out.Write(make([]byte, 1024))
			return err
		}
		return h.streamFile(file, info.Size)
	}
	return errors.New("unknown file operation")
}

// RunFile runs the file helper for a JSON request.
func RunFile(argument string, stdin io.Reader, stdout, stderr io.Writer) int {
	var request FileRequest
	if err := json.Unmarshal([]byte(argument), &request); err != nil {
		io.WriteString(stderr, FileFailure)
		return 1
	}
	h := &fileHelper{request: request, out: bufio.NewWriterSize(stdout, 64*1024), stdin: stdin}
	if stdin == os.Stdin {
		h.exit = os.Exit
	}
	// The deadline belongs to the command as well as its Docker connection:
	// losing the connection must not leave a writer running.
	limit := 55 * time.Second
	if slices.Contains([]string{"backup", "download", "upload"}, request.Operation) {
		limit = 30 * time.Minute
	}
	deadline := time.AfterFunc(limit, func() {
		if request.Operation == "upload" {
			h.abortUpload(errors.New("upload deadline"))
			return
		}
		os.Exit(1)
	})
	defer deadline.Stop()
	defer h.t.closeAll()
	err := h.run()
	if flushErr := h.out.Flush(); err == nil {
		err = flushErr
	}
	if err != nil {
		io.WriteString(stderr, FileFailure)
		return 1
	}
	return 0
}
