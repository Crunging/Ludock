package helper

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
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
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Crunging/Ludock/internal/tarstream"
)

type FileRequest struct {
	Operation   string   `json:"operation"`
	Root        string   `json:"root"`
	Path        string   `json:"path,omitempty"`
	Blocked     []string `json:"blocked,omitempty"`
	Destination string   `json:"destination,omitempty"`
	Size        *int64   `json:"size,omitempty"`
	UploadID    string   `json:"uploadId,omitempty"`
	// Expected makes an upload replace only this exact existing file.
	Expected *FileVersion `json:"expected,omitempty"`
	// Paths are the entries of one folder that a selection acts on.
	Paths []string `json:"paths,omitempty"`
	// Mounts are the nested mounts the helper has, which a move can't cross.
	Mounts []string `json:"mounts,omitempty"`
	// Reserve is the free space an unpack leaves.
	Reserve *int64 `json:"reserve,omitempty"`
	// Files are the files of a batch upload's cleanup, all in the folder
	// Path. The upload itself reads them from stdin.
	Files []UploadFile `json:"files,omitempty"`
	// Base is the helper's mount that holds Root. A backup, and the mounts
	// check, refuse any other mount at or below Root or between them.
	Base string `json:"base,omitempty"`
}

type UploadFile struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	UploadID string `json:"uploadId"`
}

// FileVersion identifies a file's content by size and SHA-256.
type FileVersion struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// FileSpace is the filesystem under a root. Used and Available leave out
// blocks reserved for root, as df does.
type FileSpace struct {
	Total     int64 `json:"total"`
	Used      int64 `json:"used"`
	Available int64 `json:"available"`
}

type FileEntry struct {
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	Size       int64   `json:"size"`
	ModifiedAt float64 `json:"modifiedAt"`
}

// FileFailure is the only error text the file helper prints. Paths and raw
// filesystem errors can contain private host information.
const FileFailure = "The file change failed: the path changed, is unsafe, or can’t be reached."

// BackupLimit bounds reading one data root for a backup, while the server is
// stopped.
const BackupLimit = 4 * time.Hour

// FileNameTaken is the file helper's exit code when a new name is in use.
const FileNameTaken = 3

// FileChanged is the file helper's exit code when an upload's expected
// version is no longer the file's content.
const FileChanged = 4

// Exit codes for an archive that was not unpacked.
const (
	FileArchiveUnsafe = 5
	FileArchiveLimit  = 6
	FileArchiveFull   = 7
	FileNotArchive    = 8
	// FilePartial is a selection that failed after changing some entries.
	FilePartial = 9
	// FileMounted refuses a change to a folder that is, or holds, a separate
	// mount.
	FileMounted = 10
	// FileHoldsRestore refuses deleting or moving a folder that holds a
	// restore's stage folder.
	FileHoldsRestore = 11
	// FileNoSpace refuses a file of a batch upload that would leave the disk
	// less than UploadSpare.
	FileNoSpace = 12
	// FileUncertain is an upload that failed after its file took its name.
	FileUncertain = 13
)

type fileHelper struct {
	request FileRequest
	t       tracker
	out     *bufio.Writer
	stdin   io.Reader
	entries int
	buffer  []byte
	// part is the upload part being read, which a refused file of a batch
	// is skipped to the end of.
	part *uploadPart
	// folder is the path, below the root, of the folder an upload writes in.
	folder string
	// leftover: the last upload's staging file could not be removed.
	leftover bool

	exit     func(int)
	stop     atomic.Bool
	mu       sync.Mutex
	aborted  bool
	abortErr error
}

// Within reports whether the slash-separated path value is root or inside
// it. "/" holds every absolute path.
func Within(value, root string) bool {
	// Discovery calls this for every mount and system path; no concatenation.
	if root == "/" {
		return strings.HasPrefix(value, "/")
	}
	return strings.HasPrefix(value, root) && (len(value) == len(root) || value[len(root)] == '/')
}

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

// ownParts splits a request's path below the root, refusing Ludock's own
// names.
func ownParts(value string) ([]string, error) {
	parts, err := components(value)
	if err == nil && slices.ContainsFunc(parts, Reserved) {
		return nil, errUnsafe
	}
	return parts, err
}

func (h *fileHelper) blocked(value string, recursive bool) bool {
	for _, root := range h.request.Blocked {
		if Within(value, root) || (recursive && Within(root, value)) {
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
	parts, err := ownParts(relative)
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
	parts, err := ownParts(relative)
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
	if h.buffer == nil {
		h.buffer = make([]byte, 64*1024)
	}
	buffer := h.buffer
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

// header writes an entry's archive header. Ownership, and folders' setgid
// and sticky bits, are kept only for backups.
func (h *fileHelper) header(name string, stat *unix.Stat_t, directory bool) error {
	if len(name) > 4096 {
		return errors.New("an archive path is too long")
	}
	entry := tarstream.Header{Name: name, Type: "file", Size: stat.Size, Mode: int64(stat.Mode) & 0o777,
		MtimeSeconds: float64(max(0, stat.Mtim.Sec))}
	if directory {
		entry.Type, entry.Size = "directory", 0
	}
	if h.request.Operation == "backup" {
		entry.UID, entry.GID = int64(stat.Uid), int64(stat.Gid)
		if directory {
			entry.Mode = int64(stat.Mode) & 0o3777
		}
	}
	header, err := tarstream.EncodeHeader(entry)
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
	// Checked as the walk goes, not only before it, in case a mount appeared
	// since.
	if h.request.Operation == "backup" {
		if same, err := sameMount(parent, file); err != nil {
			return err
		} else if !same {
			return errMounted
		}
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
			if Reserved(child) {
				continue
			}
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
		if _, err := h.out.Write(tarstream.Padding(info.Size)); err != nil {
			return err
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
	// Checked again here, not only before the change, in case a mount
	// appeared since.
	if same, err := sameMount(parent, directory); err != nil {
		return err
	} else if !same {
		return errMounted
	}
	children, err := names(directory)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(children, validStage) {
		return errHoldsRestore
	}
	for _, child := range children {
		if err := h.removeEntry(directory, child, depth+1); err != nil {
			return err
		}
	}
	h.t.close(directory)
	return unix.Unlinkat(int(parent), name, unix.AT_REMOVEDIR)
}

// unmounted refuses an entry that is, or holds, a separate mount, whether
// the request declared it or only the file system shows it, or that holds a
// restore's stage folder, which can be the only copy of the previous data,
// before a change that would delete or move what is inside it. A stage is
// below the top of a data folder when another server's, or a wider
// ludock.files, shows the folder that holds it.
func (h *fileHelper) unmounted(parent fd, name, relative string) error {
	absolute := h.absolute(relative)
	if slices.ContainsFunc(h.request.Mounts, func(mount string) bool { return Within(mount, absolute) }) {
		return errMounted
	}
	count := 0
	return h.oneMount(parent, name, 0, &count)
}

// oneMount checks files too: unlinking a mounted file fails only after
// what was removed before it is gone.
func (h *fileHelper) oneMount(parent fd, name string, depth int, count *int) error {
	if *count++; depth > 64 || *count > 1000000 {
		return errors.New("the folder has too many entries to check")
	}
	info, err := lstatAt(parent, name)
	if err != nil {
		return err
	}
	flags := directoryFlags
	if !isDir(info) {
		flags = unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC
	}
	entry, err := h.t.openAt(parent, name, flags, 0)
	if err != nil {
		return err
	}
	defer h.t.close(entry)
	if same, err := sameMount(parent, entry); err != nil {
		return err
	} else if !same {
		return errMounted
	}
	if !isDir(info) {
		return nil
	}
	children, err := names(entry)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(children, validStage) {
		return errHoldsRestore
	}
	for _, child := range children {
		if err := h.oneMount(entry, child, depth+1, count); err != nil {
			return err
		}
	}
	return nil
}

// stages lists, by path from the root, up to three restore stage folders
// below the root's top at or below relative, which a backup leaves out and
// a restore must not move. Other names Ludock reserves are skipped, as
// backups skip them: none can hold a stage, and those at the top stay put. A path that is missing or not a folder
// holds none; past the walk's limits, which no backup reaches, it refuses.
func (h *fileHelper) stages(root fd, relative string) ([]string, error) {
	directory := root
	if relative != "" {
		var err error
		if directory, err = h.pinDirectory(root, relative); err != nil {
			if isNotExist(err) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
				return []string{}, nil
			}
			return nil, err
		}
	}
	found, count := []string{}, 0
	var walk func(directory fd, relative string, depth int) error
	walk = func(directory fd, relative string, depth int) error {
		children, err := names(directory)
		if err != nil {
			return err
		}
		// The root's children are at depth 1 here and 0 in a restore's walk.
		if count += len(children); depth > 65 || count > 1000000 {
			return errors.New("the folder has too many entries to check")
		}
		for _, child := range children {
			if len(found) == 3 {
				return nil
			}
			// Each entry is checked with lstat: some network file systems
			// list every entry's type as unknown.
			info, err := lstatAt(directory, child)
			if isNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if !isDir(info) {
				continue
			}
			name := path.Join(relative, child)
			if relative != "" && validStage(child) {
				found = append(found, name)
			}
			if Reserved(child) {
				continue
			}
			opened, err := h.t.openAt(directory, child, directoryFlags, 0)
			if err != nil {
				return err
			}
			err = walk(opened, name, depth+1)
			h.t.close(opened)
			if err != nil {
				return err
			}
		}
		return nil
	}
	return found, walk(directory, relative, 0)
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

// uploadPart reads one file's body and then its trailer from stdin, never
// past them, so the next file of a batch follows in sync.
type uploadPart struct {
	in      io.Reader
	body    int64
	trailer []byte
	// ready: the helper asked for the trailer.
	ready bool
}

var errUploadStream = errors.New("upload did not match its declared size or was interrupted")

// read returns body bytes, consumes the trailer once the body is read, and
// returns io.EOF after both.
func (p *uploadPart) read(buffer []byte) (int, error) {
	want := p.body
	if want == 0 {
		want = int64(len(p.trailer))
	}
	if want == 0 {
		return 0, io.EOF
	}
	n, err := p.in.Read(buffer[:min(int64(len(buffer)), want)])
	if p.body > 0 {
		p.body -= int64(n)
	} else {
		if subtle.ConstantTimeCompare(buffer[:n], p.trailer[:n]) != 1 {
			return 0, errUploadStream
		}
		p.trailer, n = p.trailer[n:], 0
	}
	if err != nil && (err != io.EOF || p.body > 0 || len(p.trailer) > 0) {
		return n, errUploadStream
	}
	return n, nil
}

// upload writes stdin's next size bytes to name, committing them only
// after the trailer id. Errors other than errUploadStream leave the rest of
// the part unread.
func (h *fileHelper) upload(parent fd, name string, size int64, id string, expected *FileVersion) error {
	if size < 0 || size > 1<<53-1 {
		return errors.New("invalid upload size")
	}
	trailer := []byte(id)
	if id == "" {
		id = newUUID()
	}
	temporary, err := uploadName(id)
	if err != nil {
		return err
	}
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
	if h.request.Operation == "upload-files" {
		var stat unix.Statfs_t
		if err := unix.Fstatfs(int(parent), &stat); err != nil {
			return err
		}
		if space := Space(&stat); size > space.Available-UploadSpare(space.Total) {
			return &noSpaceError{space}
		}
	}
	file, err := h.t.openAt(parent, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		h.t.close(file)
		if err := unix.Unlinkat(int(parent), temporary, 0); err != nil && !isNotExist(err) {
			// The backend's cleanup request removes it.
			h.leftover = true
		}
	}()
	idle := time.AfterFunc(60*time.Second, func() { h.abortUpload(errors.New("upload interrupted")) })
	defer idle.Stop()
	part := &uploadPart{in: h.stdin, body: size, trailer: trailer}
	h.part = part
	if h.buffer == nil {
		h.buffer = make([]byte, 64*1024)
	}
	for part.body > 0 {
		n, readErr := part.read(h.buffer)
		for offset := 0; offset < n; {
			written, err := unix.Write(int(file), h.buffer[offset:n])
			if err != nil {
				return err
			}
			if written == 0 {
				return errors.New("upload write made no progress")
			}
			offset += written
		}
		if readErr != nil {
			return readErr
		}
		idle.Reset(60 * time.Second)
	}
	// The staged file stays root's, mode 0600, until its trailer arrives, so
	// nothing else reads it before the backend has authorized it.
	if err := unix.Fsync(int(file)); err != nil {
		return err
	}
	if err := h.readyForTrailer(part); err != nil {
		return err
	}
	// A single upload ends with its input.
	if h.request.Operation == "upload" {
		if n, err := h.stdin.Read(h.buffer[:1]); n > 0 || err != io.EOF {
			return errUploadStream
		}
	}
	if h.uploadAborted() {
		return errUploadStream
	}
	// Checked once the input is complete, so the backend reads the helper's
	// exit code instead of an unfinished upload.
	if expected != nil {
		if err := h.expectVersion(parent, name, original, *expected); err != nil {
			return err
		}
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
	written, err := fstat(file)
	if err != nil {
		return err
	}
	// The folder, found again from the root by its path, must still be the one
	// written into: a rename while the file arrived could have moved it out of
	// the root. A move in the moment between this check and the publish below
	// can't be caught from here.
	if err := h.sameFolder(parent); err != nil {
		return err
	}
	// Publishing goes by the staging name, which must still be the inode
	// written.
	if staged, err := lstatAt(parent, temporary); err != nil || staged.Dev != written.Dev || staged.Ino != written.Ino {
		return errors.New("the staged file changed")
	}
	// Only now may the game read it. Set permissions while the helper still
	// owns the new inode; CHOWN is enough to restore game ownership without
	// granting FOWNER.
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
			return errPublished
		}
	}
	// What took the name between the check and the publish is unknown.
	if published, err := lstatAt(parent, name); err != nil || published.Dev != written.Dev || published.Ino != written.Ino {
		return errPublished
	}
	if err := unix.Fsync(int(parent)); err != nil {
		return errPublished
	}
	return nil
}

// sameFolder confirms that folder, walked again from the root without
// following links, is still the directory parent holds.
func (h *fileHelper) sameFolder(parent fd) error {
	opened := len(h.t.open)
	defer h.t.closeFrom(opened)
	root, err := h.pinRoot()
	if err != nil {
		return err
	}
	folder, err := h.pinDirectory(root, h.folder)
	if err != nil {
		return err
	}
	now, err := fstat(folder)
	if err != nil {
		return err
	}
	held, err := fstat(parent)
	if err != nil {
		return err
	}
	if now.Dev != held.Dev || now.Ino != held.Ino {
		return errors.New("the folder moved")
	}
	return nil
}

// sweepStaging removes the staging files left in folder by uploads cut off
// earlier. The server's file lock is held, so no other upload is writing.
func (h *fileHelper) sweepStaging(folder fd) error {
	children, err := names(folder)
	if err != nil {
		return err
	}
	for _, name := range children {
		id, found := strings.CutPrefix(name, ".ludock-upload-")
		id, staged := strings.CutSuffix(id, ".tmp")
		if !found || !staged || !uploadIDPattern.MatchString(id) {
			continue
		}
		if info, err := lstatAt(folder, name); err == nil && isRegular(info) {
			if err := unix.Unlinkat(int(folder), name, 0); err != nil && !isNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// readyForTrailer reads a part's trailer. In a batch the backend sends it
// only after this line, so it checks access and the game right before each
// file can be published rather than when the trailer was queued.
func (h *fileHelper) readyForTrailer(part *uploadPart) error {
	if h.request.Operation == "upload-files" {
		if _, err := h.out.WriteString(`{"ready":true}` + "\n"); err != nil {
			return err
		}
		if err := h.out.Flush(); err != nil {
			return err
		}
	}
	part.ready = true
	for {
		_, err := part.read(h.buffer)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// UploadSpare is the free space an upload leaves on the server's disk: 1 GiB,
// or a twentieth of a smaller disk, so the game can still save.
func UploadSpare(total int64) int64 { return min(1<<30, total/20) }

type noSpaceError struct{ space FileSpace }

func (e *noSpaceError) Error() string { return "not enough free space" }

// A batch upload holds at most UploadBatchFiles files, and several files
// at most UploadBatchBytes in all; a larger file is uploaded alone.
const (
	UploadBatchFiles = 100
	UploadBatchBytes = 16 << 20
)

// UploadResult is the file helper's line for each file of a batch upload.
type UploadResult struct {
	OK bool `json:"ok,omitempty"`
	// Code is the exit code a single upload would have ended with.
	Code int `json:"code,omitempty"`
	// Space is the disk's when Code is FileNoSpace.
	Space *FileSpace `json:"space,omitempty"`
	// Ready asks for the file's trailer, ahead of its result.
	Ready bool `json:"ready,omitempty"`
	// Leftover: the file's staging file could not be removed.
	Leftover bool `json:"leftover,omitempty"`
}

// uploadFiles writes a batch into one folder: each file's body and trailer
// follow the previous one's. A file that fails is skipped in step and
// reported; only a broken stream ends the batch, and each line is flushed
// once its file is committed or refused.
func (h *fileHelper) uploadFiles(relative string) error {
	// The files are listed on stdin, ahead of their bodies, since a long list
	// could pass an argument's size limit: its length, then its JSON.
	var length [8]byte
	if _, err := io.ReadFull(h.stdin, length[:]); err != nil {
		return errUploadStream
	}
	size := binary.BigEndian.Uint64(length[:])
	if size > 4<<20 {
		return errUploadStream
	}
	list := make([]byte, size)
	if _, err := io.ReadFull(h.stdin, list); err != nil || json.Unmarshal(list, &h.request.Files) != nil {
		return errUploadStream
	}
	var total int64
	for _, file := range h.request.Files {
		if file.Size < 0 || file.Size > 1<<53-1 {
			return errors.New("invalid upload size")
		}
		total += file.Size
	}
	if len(h.request.Files) > UploadBatchFiles || (len(h.request.Files) > 1 && total > UploadBatchBytes) {
		return errors.New("the batch is too large")
	}
	h.buffer = make([]byte, 64*1024)
	for _, file := range h.request.Files {
		h.part, h.leftover = nil, false
		opened := len(h.t.open)
		err := h.check(path.Join(relative, file.Name), false)
		if parts, partsErr := ownParts(file.Name); err == nil && (partsErr != nil || len(parts) != 1 || parts[0] != file.Name) {
			err = errUnsafe
		}
		// The root and folder are found again for each file, as for a single
		// upload, in case either was moved meanwhile.
		var folder fd
		if err == nil {
			folder, err = h.pinRoot()
		}
		if err == nil && relative != "" {
			folder, err = h.pinDirectory(folder, relative)
		}
		if err == nil {
			err = h.upload(folder, file.Name, file.Size, file.UploadID, nil)
		}
		h.t.closeFrom(opened)
		if errors.Is(err, errUploadStream) || h.uploadAborted() {
			return errUploadStream
		}
		result := UploadResult{OK: err == nil, Leftover: h.leftover}
		var full *noSpaceError
		switch {
		case err == nil:
		case errors.As(err, &full) || errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT):
			result.Code = FileNoSpace
			if full != nil {
				result.Space = &full.space
			}
		default:
			result.Code = fileExit(err)
		}
		if err != nil {
			part := h.part
			if part == nil {
				part = &uploadPart{in: h.stdin, body: file.Size, trailer: []byte(file.UploadID)}
			}
			// Reading past a refused file costs at most a batch; a larger one
			// is reported and ends the command instead.
			if part.body > UploadBatchBytes {
				if err := h.result(result); err != nil {
					return err
				}
				return errUploadStream
			}
			idle := time.AfterFunc(60*time.Second, func() { h.abortUpload(errors.New("upload interrupted")) })
			for part.body > 0 {
				if _, readErr := part.read(h.buffer); readErr != nil {
					idle.Stop()
					return errUploadStream
				}
				idle.Reset(60 * time.Second)
			}
			if !part.ready && h.readyForTrailer(part) != nil {
				idle.Stop()
				return errUploadStream
			}
			idle.Stop()
		}
		if err := h.result(result); err != nil {
			return err
		}
	}
	if n, err := h.stdin.Read(h.buffer[:1]); n > 0 || err != io.EOF {
		return errUploadStream
	}
	return nil
}

// result writes a batch file's line, flushed so the backend has it even if
// the command then fails.
func (h *fileHelper) result(result UploadResult) error {
	if err := h.json(result); err != nil {
		return err
	}
	if _, err := h.out.Write([]byte{'\n'}); err != nil {
		return err
	}
	return h.out.Flush()
}

// expectVersion confirms the target still holds the expected content. The
// stat taken before hashing is the one the commit compares against, so a
// write after the hash is caught there.
func (h *fileHelper) expectVersion(parent fd, name string, original *unix.Stat_t, expected FileVersion) error {
	if original == nil || original.Size != expected.Size {
		return errFileChanged
	}
	file, err := h.t.openAt(parent, name, fileFlags, 0)
	if err != nil {
		return err
	}
	defer h.t.close(file)
	opened, err := fstat(file)
	if err != nil {
		return err
	}
	if opened.Dev != original.Dev || opened.Ino != original.Ino {
		return errFileChanged
	}
	hash := sha256.New()
	buffer := make([]byte, 64*1024)
	for position := int64(0); position < expected.Size; {
		n, err := unix.Pread(int(file), buffer[:min(int64(len(buffer)), expected.Size-position)], position)
		if err != nil {
			return err
		}
		if n == 0 {
			return errFileChanged
		}
		hash.Write(buffer[:n])
		position += int64(n)
	}
	if hex.EncodeToString(hash.Sum(nil)) != strings.ToLower(expected.SHA256) {
		return errFileChanged
	}
	return nil
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
	parts, err := ownParts(h.request.Path)
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
	case "space":
		// A folder below the root can be on another disk, such as a nested
		// mount.
		directory := root
		if relative != "" {
			if directory, err = h.pinDirectory(root, relative); err != nil {
				return err
			}
		}
		var stat unix.Statfs_t
		if err := unix.Fstatfs(int(directory), &stat); err != nil {
			return err
		}
		return h.json(Space(&stat))
	case "mounts":
		if err := ownMount(h.request.Base, h.absolute(relative)); err != nil {
			return err
		}
		stages, err := h.stages(root, relative)
		if err != nil {
			return err
		}
		return h.json(map[string]any{"safe": true, "stages": stages})
	case "backup":
		if err := h.check(relative, true); err != nil {
			return err
		}
		if err := ownMount(h.request.Base, h.request.Root); err != nil {
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
			if Reserved(name) {
				continue
			}
			if err := h.archiveEntry(root, name, "source/"+name, 0); err != nil {
				return err
			}
		}
		_, err = h.out.Write(tarstream.End())
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
			if Reserved(name) || h.blocked(h.absolute(path.Join(relative, name)), false) {
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
	switch operation {
	case "delete-paths", "move", "download-paths":
		return h.selection(root, operation)
	case "upload-files":
		h.folder = relative
		return h.uploadFiles(relative)
	case "upload-prepare":
		folder, err := h.pinDirectory(root, relative)
		if err != nil {
			return err
		}
		if err := h.sweepStaging(folder); err != nil {
			return err
		}
		var stat unix.Statfs_t
		if err := unix.Fstatfs(int(folder), &stat); err != nil {
			return err
		}
		return h.json(Space(&stat))
	case "upload-files-cleanup":
		folder, err := h.pinDirectory(root, relative)
		if err != nil {
			return err
		}
		for _, file := range h.request.Files {
			temporary, err := uploadName(file.UploadID)
			if err != nil {
				return err
			}
			if err := unix.Unlinkat(int(folder), temporary, 0); err != nil && !isNotExist(err) {
				return err
			}
		}
		return h.json(map[string]bool{"ok": true})
	}
	parent, name, err := h.parentOf(root, relative)
	// A folder that is gone took its staging folder with it.
	if operation == "unpack-cleanup" && isNotExist(err) {
		return h.json(map[string]bool{"ok": true})
	}
	if err != nil {
		return err
	}
	switch operation {
	case "unpack":
		return h.unpack(parent, name)
	case "unpack-cleanup":
		stage, err := unpackName(h.request.UploadID)
		if err != nil {
			return err
		}
		if err := h.removeStage(parent, stage); err != nil {
			return err
		}
		return h.json(map[string]bool{"ok": true})
	case "mkdir":
		owner, err := fstat(parent)
		if err != nil {
			return err
		}
		if err := unix.Mkdirat(int(parent), name, 0o777); err != nil {
			return nameTaken(err)
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
		if err := h.unmounted(parent, name, relative); err != nil {
			return err
		}
		if err := h.removeEntry(parent, name, 0); err != nil {
			return err
		}
		return h.json(map[string]bool{"ok": true})
	case "rename":
		destinationParts, err := ownParts(h.request.Destination)
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
		if err := h.unmounted(parent, name, relative); err != nil {
			return err
		}
		if _, err := lstatAt(targetParent, targetName); err == nil {
			return errNameTaken
		} else if !isNotExist(err) {
			return err
		}
		if err := renameNoReplace(parent, name, targetParent, targetName, info); err != nil {
			return nameTaken(err)
		}
		return h.json(map[string]bool{"ok": true})
	case "upload":
		if h.request.Size == nil {
			return errors.New("invalid upload size")
		}
		h.folder = path.Dir(relative)
		if err := h.sweepStaging(parent); err != nil {
			return err
		}
		if err := h.upload(parent, name, *h.request.Size, h.request.UploadID, h.request.Expected); err != nil {
			return err
		}
		return h.json(map[string]bool{"ok": true})
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
		// The type and size of the file being sent lead the stream, so they
		// describe exactly what follows even if the path changes meanwhile.
		kind, size := "file", info.Size
		if isDir(info) {
			kind, size = "directory", 0
		}
		if err := h.json(map[string]any{"type": kind, "size": size}); err != nil {
			return err
		}
		if _, err := h.out.Write([]byte{'\n'}); err != nil {
			return err
		}
		if isDir(info) {
			h.t.close(file)
			if err := h.archiveEntry(parent, name, name, 0); err != nil {
				return err
			}
			_, err := h.out.Write(tarstream.End())
			return err
		}
		return h.streamFile(file, info.Size)
	}
	return errors.New("unknown file operation")
}

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
	switch request.Operation {
	case "download", "upload", "upload-files", "download-paths":
		limit = 30 * time.Minute
	case "unpack":
		limit = UnpackDuration
	case "backup":
		// The backend's deadline, started just after this one, ends the read
		// and reports the timeout; this one only stops a helper it lost.
		limit = BackupLimit + time.Minute
	}
	deadline := time.AfterFunc(limit, func() {
		if request.Operation == "upload" || request.Operation == "upload-files" {
			h.abortUpload(errors.New("upload deadline"))
			return
		}
		// An unpack stops itself, so it removes its staging folder.
		if request.Operation == "unpack" {
			h.stop.Store(true)
			if h.exit != nil {
				time.AfterFunc(time.Minute, func() { h.exit(1) })
			}
			return
		}
		// A selection may have changed some entries already.
		if request.Operation == "delete-paths" || request.Operation == "move" {
			os.Exit(FilePartial)
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
		return fileExit(err)
	}
	return 0
}

// fileExit is the exit code that reports err.
func fileExit(err error) int {
	switch {
	case errors.Is(err, errNameTaken):
		return FileNameTaken
	case errors.Is(err, errFileChanged):
		return FileChanged
	case errors.Is(err, errArchiveUnsafe):
		return FileArchiveUnsafe
	case errors.Is(err, errArchiveLimit):
		return FileArchiveLimit
	case errors.Is(err, errArchiveFull), errors.Is(err, unix.ENOSPC), errors.Is(err, unix.EDQUOT):
		return FileArchiveFull
	case errors.Is(err, errNotArchive):
		return FileNotArchive
	case errors.Is(err, errPartial):
		return FilePartial
	case errors.Is(err, errMounted):
		return FileMounted
	case errors.Is(err, errHoldsRestore):
		return FileHoldsRestore
	case errors.Is(err, errPublished):
		return FileUncertain
	}
	return 1
}

var (
	errNameTaken    = errors.New("the name is in use")
	errFileChanged  = errors.New("the file changed")
	errPartial      = errors.New("part of the selection changed")
	errMounted      = errors.New("the folder is or holds a separate mount")
	errHoldsRestore = errors.New("the folder holds a restore's stage folder")
	errPublished    = errors.New("the upload failed after it took its name")
)

func nameTaken(err error) error {
	if errors.Is(err, unix.EEXIST) {
		return errNameTaken
	}
	return err
}

// renameat2 is swapped in tests to act like a kernel without
// RENAME_NOREPLACE.
var renameat2 = unix.Renameat2

// renameNoReplace moves an entry without ever replacing what is at the
// destination: game servers write these trees while files are managed.
func renameNoReplace(parent fd, name string, targetParent fd, targetName string, info *unix.Stat_t) error {
	err := renameat2(int(parent), name, int(targetParent), targetName, unix.RENAME_NOREPLACE)
	if err == nil || (!errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EINVAL)) {
		return err
	}
	// A plain rename would replace anything created at the destination since
	// it was checked. A file can move safely instead: linking fails if the
	// destination exists.
	if !isRegular(info) {
		return errors.New("this filesystem cannot rename folders without risking an overwrite")
	}
	if err := unix.Linkat(int(parent), name, int(targetParent), targetName, 0); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(parent), name, 0); err != nil {
		return errPartial
	}
	return nil
}
