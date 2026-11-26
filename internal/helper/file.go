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
	"fmt"
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
	// List says an upload's prepare or cleanup reads its files from stdin;
	// without it, prepare is for one file in Path and reads nothing.
	List bool `json:"list,omitempty"`
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

// UploadFile is a file of a batch upload. Name is its path below the
// batch's folder, whose missing folders the upload makes. Without Replace, a
// file whose name is taken is refused with FileNameTaken.
type UploadFile struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	UploadID string `json:"uploadId"`
	Replace  bool   `json:"replace,omitempty"`
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
	// FileArchiveDeep refuses an unpack whose files would pass MaxPath.
	FileArchiveDeep = 14
	// FileACLNotKept refuses replacing a file whose access ACL can't be kept.
	FileACLNotKept = 15
	// FileNotFolder refuses a file of a batch upload where a file has the
	// name of a folder it needs, and FileIsFolder one whose name is a folder's.
	FileNotFolder = 16
	FileIsFolder  = 17
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
	// folder is the path, below the root, of the folder an upload writes in,
	// and replace lets a batch's file replace one there.
	folder  string
	replace bool
	// file is the position in its batch of the file being uploaded.
	file int
	// leftover: the last upload's staging file could not be removed.
	leftover bool
	// excluded is what the request's excluded mounts hide, by identity.
	excluded exclusions

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
		return errExcluded
	}
	return h.checkIdentity(relative, recursive)
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
	if isDir(info) {
		return nil, errIsFolder
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

// header writes the archive header of file. Ownership, ACLs, and folders'
// setgid and sticky bits, are kept only for backups.
func (h *fileHelper) header(name string, file fd, stat *unix.Stat_t, directory bool) error {
	if len(name) > 4096 {
		return errors.New("an archive path is too long")
	}
	backup := h.request.Operation == "backup"
	entry := tarstream.Header{Name: name, Type: "file", Size: stat.Size, MtimeSeconds: float64(max(0, stat.Mtim.Sec)), PAX: map[string]string{}}
	mode := stat.Mode & 0o777
	if directory {
		entry.Type, entry.Size = "directory", 0
		if backup {
			mode = stat.Mode & 0o3777
		}
	}
	access, err := readACL(file, accessACL)
	if err != nil {
		return err
	}
	if access != nil {
		mode = fallbackMode(mode, access)
	}
	if backup {
		entry.UID, entry.GID = int64(stat.Uid), int64(stat.Gid)
		var def []aclEntry
		if directory {
			if def, err = readACL(file, defaultACL); err != nil {
				return err
			}
		}
		// bsdtar restores a folder with a default ACL but no access ACL as
		// mode 0000, so it is given the access ACL its mode is, as GNU tar
		// does.
		if def != nil && access == nil {
			access = []aclEntry{{aclUserObj, uint16(mode >> 6 & 7), aclNoID}, {aclGroupObj, uint16(mode >> 3 & 7), aclNoID},
				{aclOther, uint16(mode & 7), aclNoID}}
		}
		if _, masked := aclPerm(access, aclMask); masked || def != nil {
			entry.PAX[ACLAccessRecord] = aclText(access)
		}
		if def != nil {
			entry.PAX[ACLDefaultRecord] = aclText(def)
		}
		if len(access) > MaxACLEntries || len(def) > MaxACLEntries {
			return errors.New("an ACL has too many entries to back up")
		}
	}
	entry.Mode = int64(mode)
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
		if err := h.header(relative+"/", file, info, true); err != nil {
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
		if err := h.header(relative, file, info, false); err != nil {
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
	if h.excludedIn(parent, info) {
		return errExcluded
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
// unmounted checks the tree at name from depth, which a staging folder
// starts a level up.
func (h *fileHelper) unmounted(parent fd, name, relative string, depth int) error {
	absolute := h.absolute(relative)
	if slices.ContainsFunc(h.request.Mounts, func(mount string) bool { return Within(mount, absolute) }) {
		return errMounted
	}
	count := 0
	return h.oneMount(parent, name, depth, &count)
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
	// A batch upload not confirmed to replace refuses a name taken now, or
	// by the time the file would be published, having written nothing to it.
	keep := h.request.Operation == "upload-files" && !h.replace
	if keep && original != nil {
		return errNameTaken
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
	written, err := fstat(file)
	if err != nil {
		return err
	}
	// The destination must be as it was, the folder, found again from the
	// root by its path, still the one written into, and the staging name,
	// which publishing goes by, still the inode written. Checked before the
	// game may read the file and again right before publishing; a move
	// between the last check and the publish goes unseen. When the root is
	// its own mount, the game can't move a folder out of it through that
	// mount (a rename across it gets EXDEV); only another mount of the same
	// storage can, on the host, in another container, or in the game's own
	// container when one of its mounts exposes a parent of the root, and
	// whoever holds that can already write there. When the root is a folder
	// inside a larger mount, the game can move a folder out of the root within
	// that mount, but only to where the game itself can already write. So
	// this catches a move during a long transfer and reports it; it is not a
	// boundary against any of them. A member's file that lands in a folder
	// moved in that moment lands where whoever moved the folder put it, which
	// the member neither chose nor could arrange.
	unchanged := func() error {
		current, err := uploadTarget(parent, name)
		if err != nil {
			return err
		}
		if keep && original == nil && current != nil {
			return errNameTaken
		}
		if (current == nil) != (original == nil) || (original != nil && (original.Dev != current.Dev || original.Ino != current.Ino ||
			original.Mode != current.Mode || original.Uid != current.Uid || original.Gid != current.Gid ||
			original.Size != current.Size || mtimeMs(original) != mtimeMs(current) || ctimeMs(original) != ctimeMs(current))) {
			return errors.New("the upload target changed")
		}
		if err := h.sameFolder(parent); err != nil {
			return err
		}
		if staged, err := lstatAt(parent, temporary); err != nil || staged.Dev != written.Dev || staged.Ino != written.Ino {
			return errors.New("the staged file changed")
		}
		return nil
	}
	if err := unchanged(); err != nil {
		return err
	}
	// Only now may the game read the staged inode. Once it has its owner the
	// game may also write to it, as it may a moment later once it has its
	// name, so nothing is gained by changing it in between. The owner is set
	// on the staged inode before the publish; whether that inode took the
	// name is checked below. The group comes first, while the mode is still
	// 0600, so the final mode's group bits only ever apply to the target's
	// group. The owner stays the helper until last, so it may still set the
	// mode without FOWNER, which would also let a publish override a sticky
	// folder; the owner bits meanwhile grant root nothing DAC_OVERRIDE
	// doesn't. Until the owner changes, the future owner gets the group or
	// other bits, so when it will change, neither may grant more than the
	// owner bits will: a mode that does, such as 0460, is narrowed to the
	// owner's.
	var mode uint32
	if original != nil {
		mode = original.Mode & 0o777
	} else if mode, err = followMode(parent, 0o666, currentUmask()); err != nil {
		return err
	}
	// A replaced file keeps its access ACL: its group bits are the ACL's
	// mask, which would otherwise become the owning group's own rights. A new
	// one gets what creating it in its folder would have given it, since the
	// staged file's ACL was cut down to 0600. It is read once the input is
	// in, so a refusal is the command's result.
	var acl []byte
	if original != nil {
		if acl, err = h.readAccessACL(parent, name, original); err != nil {
			return err
		}
	} else if acl, err = createdACL(parent, 0o666); err != nil {
		return err
	}
	if int(owner.Uid) != helperUID() {
		owned := mode >> 6 & 7
		mode = mode&0o700 | mode&0o070&(owned<<3) | mode&0o007&owned
		if acl != nil {
			if acl = narrowACL(acl, owner.Uid); acl == nil {
				return errACLNotKept
			}
		}
	}
	if err := fchown(int(file), -1, int(owner.Gid)); err != nil {
		return err
	}
	// Setting an ACL sets the mode bits too, so a file with one never has
	// its mask as group bits on their own. A file that gets none drops any
	// the folder's default ACL gave the staged file, which the mode would
	// otherwise switch on: a replacement whose original had none, or a new
	// file whose folder lost its default ACL meanwhile.
	var aclErr error
	if acl != nil {
		aclErr = fsetxattr(int(file), accessACL, acl, 0)
	} else if err := unix.Fremovexattr(int(file), accessACL); err != nil && !unsupportedXattr(err) {
		aclErr = err
	}
	if aclErr != nil && original != nil {
		return errACLNotKept
	} else if aclErr != nil {
		return aclErr
	}
	if acl == nil {
		if err := fchmod(int(file), mode); err != nil {
			return err
		}
	}
	if err := fchown(int(file), int(owner.Uid), -1); err != nil {
		return err
	}
	if err := unix.Fsync(int(file)); err != nil {
		return err
	}
	if h.uploadAborted() {
		return errors.New("upload interrupted")
	}
	if err := unchanged(); err != nil {
		return err
	}
	// link never replaces an existing name, unlike rename.
	var publishErr error
	if original != nil {
		publishErr = renameat(int(parent), temporary, int(parent), name)
	} else {
		publishErr = linkat(int(parent), temporary, int(parent), name, 0)
	}
	// A publish can take effect and still fail, as on NFS when its reply is
	// lost, and a replayed link then even says the name exists: the file is
	// published if it has the name, and otherwise unknown, since the game
	// could have moved a published file away before the name was read.
	if publishErr != nil {
		if published, err := lstatAt(parent, name); err != nil || published.Dev != written.Dev || published.Ino != written.Ino {
			return errPublished
		}
	}
	if original == nil {
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

// stagingAge is how long ago a staging file must last have been written
// for a sweep to remove it. An upload command ends within 30 minutes and
// writes its file as it goes, so one untouched for an hour belongs to no
// upload still running, here or through another path to the same storage,
// with room for the clocks of hosts that share it.
const stagingAge = time.Hour

// sweepChunk is how many names a sweep reads at a time, and sweepEntries
// and sweepTime its budget; tests shrink them and watch readNames.
var (
	sweepChunk   = 1024
	sweepEntries = 10000
	sweepTime    = 2 * time.Second
	readNames    = (*os.File).Readdirnames
)

// sweepBudget is what the sweeps of one upload's folders have spent: the
// names they looked at and the time since they started.
type sweepBudget struct {
	started time.Time
	seen    int
}

// sweepStaging removes the staging files that uploads cut off earlier left
// in folder, h.folder below the root, reading it a chunk at a time. It
// returns how many it could not remove; those are left, and free space still
// decides each upload. It is housekeeping and never holds up the write: the
// sweeps of one upload stop once they have looked at sweepEntries names or
// run for sweepTime, so a folder too large to sweep within that keeps its
// stale staging files until it shrinks.
func (h *fileHelper) sweepStaging(folder fd, budget *sweepBudget) (int, error) {
	started := budget.started
	// A folder that can't be read is left unswept; only a moved one is an
	// error.
	duplicate, err := unix.Dup(int(folder))
	if err != nil {
		return 0, nil
	}
	directory := os.NewFile(uintptr(duplicate), "directory")
	defer directory.Close()
	if _, err := directory.Seek(0, 0); err != nil {
		return 0, nil
	}
	spent := func() bool { return budget.seen >= sweepEntries || time.Since(started) >= sweepTime }
	stuck := 0
	for {
		if spent() {
			return stuck, nil
		}
		children, readErr := readNames(directory, min(sweepChunk, sweepEntries-budget.seen))
		budget.seen += len(children)
		for _, name := range children {
			// Checked before each candidate's calls, which can each be slow on
			// network storage.
			if time.Since(started) >= sweepTime {
				return stuck, nil
			}
			id, found := strings.CutPrefix(name, ".ludock-upload-")
			id, staged := strings.CutSuffix(id, ".tmp")
			// What an excluded mount hides from the game is never touched.
			if !found || !staged || !uploadIDPattern.MatchString(id) || h.blocked(h.absolute(path.Join(h.folder, name)), false) {
				continue
			}
			info, err := lstatAt(folder, name)
			if err != nil || !isRegular(info) || h.excludedIn(folder, info) || time.Since(time.Unix(info.Mtim.Unix())) < stagingAge {
				continue
			}
			// The folder must still be the approved one, in case it moved.
			if time.Since(started) >= sweepTime {
				return stuck, nil
			}
			if err := h.sameFolder(folder); err != nil {
				return stuck, err
			}
			if time.Since(started) >= sweepTime {
				return stuck, nil
			}
			if err := unix.Unlinkat(int(folder), name, 0); err != nil && !isNotExist(err) {
				stuck++
			}
		}
		if readErr != nil {
			return stuck, nil
		}
	}
}

// removeStaging removes the staging file of the upload id in folder, at
// relative below the root, unless an excluded mount hides it from the game.
func (h *fileHelper) removeStaging(folder fd, relative, id string) error {
	temporary, err := uploadName(id)
	if err != nil {
		return err
	}
	if info, err := lstatAt(folder, temporary); h.blocked(h.absolute(path.Join(relative, temporary)), false) || (err == nil && h.excludedIn(folder, info)) {
		return errExcluded
	}
	if err := unix.Unlinkat(int(folder), temporary, 0); err != nil && !isNotExist(err) {
		return err
	}
	return nil
}

// readyForTrailer reads a part's trailer. In a batch the backend sends it
// only after this line, so it checks access and the game right before each
// file can be published rather than when the trailer was queued.
func (h *fileHelper) readyForTrailer(part *uploadPart) error {
	if h.request.Operation == "upload-files" {
		if _, err := h.out.WriteString(fmt.Sprintf(`{"file":%d,"ready":true}`, h.file) + "\n"); err != nil {
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

// MaxPath bounds the path below a root that a file change uses: Linux's
// PATH_MAX. The files package explains it.
const MaxPath = 4096

// A batch upload holds at most UploadBatchFiles files, and several files
// at most UploadBatchBytes in all; a larger file is uploaded alone.
const (
	UploadBatchFiles = 100
	UploadBatchBytes = 16 << 20
)

// A file of a batch upload sits at most UploadDepth folders below the
// batch's folder, and a batch's files are in at most UploadBatchFolders
// folders below it, counting each folder on the way once. Mod packs and
// resource packs go about 10 deep, and a batch of 100 files rarely spans
// more than 150 folders; the page splits a batch before it passes them.
const (
	UploadDepth        = 32
	UploadBatchFolders = 256
)

// Version is the protocol this helper speaks, for uploads and for archives
// and restores, which the version operation reports, so Ludock never drives
// an older helper. It goes up whenever either changes.
const Version = 4

// UploadPrepared is the free space a batch upload starts with, after its
// folder's sweep, and how many staging files the sweep could not remove.
type UploadPrepared struct {
	FileSpace
	Stuck int `json:"stuck,omitempty"`
}

// UploadResult is the file helper's line for each file of a batch upload.
type UploadResult struct {
	OK bool `json:"ok,omitempty"`
	// Code is the exit code a single upload would have ended with.
	Code int `json:"code,omitempty"`
	// Space is the disk's when Code is FileNoSpace.
	Space *FileSpace `json:"space,omitempty"`
	// File is the position in the batch of the file a line is about, which
	// the backend holds each request and result to.
	File int `json:"file"`
	// Ready asks for the file's trailer, ahead of its result.
	Ready bool `json:"ready,omitempty"`
	// Folder asks to make the missing folder at this path below the batch's
	// folder, one the file goes in; the backend answers with FolderGo once
	// access is confirmed. Body asks for the file's body, once its folders
	// are made.
	Folder string `json:"folder,omitempty"`
	Body   bool   `json:"body,omitempty"`
	// Leftover: the file's staging file could not be removed.
	Leftover bool `json:"leftover,omitempty"`
}

// uploadFiles writes a batch into one folder: each file's body and trailer
// follow the previous one's. A file that fails is skipped in step and
// reported; only a broken stream ends the batch, and each line is flushed
// once its file is committed or refused.
func (h *fileHelper) uploadFiles(relative string) error {
	if err := h.readFiles(); err != nil {
		return err
	}
	if err := checkUploadList(relative, h.request.Files, true); err != nil {
		return err
	}
	// The folders the backend approved, so no file asks for one again, and
	// those whose making failed by identity, so no spelling reaches one.
	asked, unset := map[string]error{}, map[[2]uint64]error{}
	h.buffer = make([]byte, 64*1024)
	for index, file := range h.request.Files {
		h.part, h.leftover, h.file = nil, false, index
		opened := len(h.t.open)
		parts, _ := ownParts(file.Name)
		// The root and folders are found again for each file, as for a single
		// upload, in case any was moved or replaced meanwhile. The file is
		// checked against exclusions once its folders are there, right before
		// it is written, since making them waits on the backend.
		folder, err := h.uploadPin(relative, parts[:len(parts)-1], file.Size, asked, unset)
		if err == nil {
			err = h.check(path.Join(relative, file.Name), false)
		}
		if errors.Is(err, errUploadStream) {
			return err
		}
		if lineErr := h.line(fmt.Sprintf(`{"file":%d,"body":true}`, index)); lineErr != nil {
			return lineErr
		}
		if err == nil {
			h.folder, h.replace = path.Join(append([]string{relative}, parts[:len(parts)-1]...)...), file.Replace
			err = h.upload(folder, parts[len(parts)-1], file.Size, file.UploadID, nil)
		}
		h.t.closeFrom(opened)
		if errors.Is(err, errUploadStream) || h.uploadAborted() {
			return errUploadStream
		}
		result := UploadResult{File: index, OK: err == nil, Leftover: h.leftover}
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

// uploadDestinations lists the folders below a batch's folder that its files
// go in, each once, "" for the folder itself, which is all of them for no
// files.
func uploadDestinations(files []UploadFile) []string {
	var folders []string
	for _, file := range files {
		folder := path.Dir(file.Name)
		if folder == "." {
			folder = ""
		}
		if !slices.Contains(folders, folder) {
			folders = append(folders, folder)
		}
	}
	if len(folders) == 0 {
		folders = []string{""}
	}
	return folders
}

// FolderGo is the backend’s answer to a Folder request once access is
// confirmed.
const FolderGo = 'y'

// confirmFolder asks the backend to confirm access before the missing
// folder at folder, below the batch's folder, is made for the current file.
func (h *fileHelper) confirmFolder(folder string) error {
	request, _ := json.Marshal(UploadResult{File: h.file, Folder: folder})
	if err := h.line(string(request)); err != nil {
		return err
	}
	var answer [1]byte
	if _, err := io.ReadFull(h.stdin, answer[:]); err != nil || answer[0] != FolderGo {
		return errUploadStream
	}
	return nil
}

// line writes a line of a batch's output, flushed so the backend can answer.
func (h *fileHelper) line(text string) error {
	if _, err := h.out.WriteString(text + "\n"); err != nil {
		return err
	}
	return h.out.Flush()
}

// readFiles reads the request's files from stdin, ahead of anything else
// there, since a long list could pass an argument's size limit: its length,
// then its JSON.
func (h *fileHelper) readFiles() error {
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
	return nil
}

// readList reads a list sent alone, to its end, so the command never acts
// on part of one or exits before the backend has sent all of it, and checks
// it before anything is done with it.
func (h *fileHelper) readList(relative string, ids bool) error {
	if err := h.readFiles(); err != nil {
		return err
	}
	var rest [1]byte
	if n, err := h.stdin.Read(rest[:]); n > 0 || err != io.EOF {
		return errUploadStream
	}
	// A batch has a file, and no more than it can; a cleanup removes only
	// staging names it can make from every one of its upload IDs.
	if len(h.request.Files) == 0 || len(h.request.Files) > UploadBatchFiles {
		return errUploadStream
	}
	for _, file := range h.request.Files {
		if _, err := uploadName(file.UploadID); ids && err != nil {
			return err
		}
	}
	return checkUploadList(relative, h.request.Files, false)
}

// uploadPin opens the folder a file of size bytes goes in, dirs below the
// batch's folder at relative, without following links. Missing folders are
// made only if the file fits on the disk they would be made on, each as New
// folder makes it: after the backend confirms access again, in its parent
// found again from the root, and checked against exclusions. asked holds
// each folder the backend approved and how making it went; none is asked
// for twice, so one made and then removed refuses the file. unset holds,
// by identity, the folders made whose setup failed, which are never used.
func (h *fileHelper) uploadPin(relative string, dirs []string, size int64, asked map[string]error, unset map[[2]uint64]error) (fd, error) {
	pin := func(depth int) (fd, error) {
		folder, err := h.pinRoot()
		if err == nil && relative != "" {
			folder, err = h.pinDirectory(folder, relative)
		}
		if err == nil {
			folder, err = h.openFolders(folder, dirs[:depth])
		}
		return folder, err
	}
	opened := len(h.t.open)
	folder, err := pin(0)
	for depth := 0; err == nil && depth < len(dirs); depth++ {
		at := path.Join(append([]string{relative}, dirs[:depth+1]...)...)
		// One that failed after approval, even once made, isn't used.
		failed, approved := asked[at]
		if failed != nil {
			return -1, failed
		}
		next, openErr := h.openFolders(folder, dirs[depth:depth+1])
		if openErr == nil {
			if info, statErr := fstat(next); statErr != nil {
				openErr = statErr
			} else if failed := unset[[2]uint64{info.Dev, info.Ino}]; failed != nil {
				openErr = failed
			}
		}
		if openErr == nil || !isNotExist(openErr) {
			folder, err = next, openErr
			continue
		}
		if approved {
			return -1, errUnsafe
		}
		// Every missing folder is on the disk of the deepest that exists; one
		// that doesn't fit the file isn't asked for.
		if err = fits(folder, size); err != nil {
			break
		}
		h.t.closeFrom(opened)
		if err = h.confirmFolder(path.Join(dirs[:depth+1]...)); err != nil {
			break
		}
		// The answer can take a while, and the parent and its disk can change
		// meanwhile.
		if folder, err = pin(depth); err == nil {
			err = h.check(at, false)
		}
		if err == nil {
			err = fits(folder, size)
		}
		var owner *unix.Stat_t
		if err == nil {
			owner, err = fstat(folder)
		}
		if err == nil {
			// Another file or the game may have made it meanwhile.
			if next, err = h.makeFolder(folder, folder, dirs[depth], owner); errors.Is(err, unix.EEXIST) {
				next, err = h.openFolders(folder, dirs[depth:depth+1])
			}
		}
		var made *unsetFolder
		if errors.As(err, &made) {
			unset[[2]uint64{made.dev, made.ino}] = err
		}
		switch full := (*noSpaceError)(nil); {
		case errors.As(err, &full):
			// Its measure was this file's; a later one is told only the disk is short.
			asked[at] = unix.ENOSPC
		case !errors.Is(err, errUploadStream):
			asked[at] = err
		}
		folder = next
	}
	return folder, err
}

// openFolders opens the folders parts below folder without following links.
func (h *fileHelper) openFolders(folder fd, parts []string) (fd, error) {
	for _, part := range parts {
		next, err := h.t.openAt(folder, part, directoryFlags, 0)
		// O_NOFOLLOW with O_DIRECTORY refuses a link as not a folder too.
		if errors.Is(err, unix.ENOTDIR) {
			if info, statErr := lstatAt(folder, part); statErr == nil && !isSymlink(info) {
				return -1, errNotFolder
			}
		}
		if err != nil {
			return -1, err
		}
		folder = next
	}
	return folder, nil
}

// fits refuses a file of size bytes that would leave the disk under folder
// less than UploadSpare.
func fits(folder fd, size int64) error {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(folder), &stat); err != nil {
		return err
	}
	if space := Space(&stat); size > space.Available-UploadSpare(space.Total) {
		return &noSpaceError{space}
	}
	return nil
}

// checkUploadList refuses a whole list unless every file's path below the
// batch's folder at relative is its own, within MaxPath and UploadDepth,
// and the files are in at most UploadBatchFolders folders; with sizes, a
// batch also within UploadBatchFiles and UploadBatchBytes.
func checkUploadList(relative string, files []UploadFile, sizes bool) error {
	var total int64
	folders := map[string]bool{}
	for _, file := range files {
		parts, err := ownParts(file.Name)
		if err != nil || len(parts) == 0 || len(parts)-1 > UploadDepth || strings.Join(parts, "/") != file.Name ||
			len(path.Join(relative, file.Name)) > MaxPath {
			return errUnsafe
		}
		for depth := 1; depth < len(parts); depth++ {
			folders[strings.Join(parts[:depth], "/")] = true
		}
		if sizes && (file.Size < 0 || file.Size > 1<<53-1) {
			return errors.New("invalid upload size")
		}
		total += file.Size
	}
	if len(folders) > UploadBatchFolders || (sizes && (len(files) > UploadBatchFiles || (len(files) > 1 && total > UploadBatchBytes))) {
		return errors.New("the batch is too large")
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

// followMode is the mode a create with mode gets, without a default ACL, in
// folder like. A folder its owning group may write to is the evidence of a
// game whose umask leaves group write, such as 0002, so the entry gets group
// write and no more for group and others than the folder gives; any other
// folder keeps the helper's umask. With an access ACL the mode's group bits
// are its mask, so the owning group's entry, as the mask limits it, stands in
// for them.
func followMode(like fd, mode uint32, umask int) (uint32, error) {
	info, err := fstat(like)
	if err != nil {
		return 0, err
	}
	entries, err := readACL(like, accessACL)
	if err != nil {
		return 0, err
	}
	folder := info.Mode
	if group, ok := aclPerm(entries, aclGroupObj); ok {
		if mask, masked := aclPerm(entries, aclMask); masked {
			group &= mask
		}
		folder = folder&^0o070 | group<<3
	}
	created := mode &^ uint32(umask)
	if folder&0o020 == 0 {
		return created, nil
	}
	return (created | mode&0o020) & (0o700 | folder&0o077), nil
}

// makeFolder creates name in folder, owned by owner's user and group, as a
// new folder there would be made: with folder's default ACL if it has one,
// and otherwise with followMode of like. Where the kernel passes on folder's
// setgid bit and group, the mode is set at creation, since a chmod by a
// helper outside that group drops the bit; elsewhere group and other access
// wait until the group is set, since until then the future group's members
// hold the other bits.
func (h *fileHelper) makeFolder(folder, like fd, name string, owner *unix.Stat_t) (fd, error) {
	inherited, err := readACL(folder, defaultACL)
	if err != nil {
		return -1, err
	}
	mode, final := uint32(0o777), uint32(0)
	if inherited == nil {
		info, err := fstat(folder)
		if err != nil {
			return -1, err
		}
		if final, err = followMode(like, 0o777, currentUmask()); err != nil {
			return -1, err
		}
		if mode = final; info.Mode&unix.S_ISGID == 0 {
			mode &^= 0o077
		}
	}
	// The helper does one operation at a time, so nothing else creates an
	// entry while the umask is cleared; with a default ACL it doesn't apply.
	umask := syscall.Umask(0)
	err = unix.Mkdirat(int(folder), name, mode)
	syscall.Umask(umask)
	if err != nil {
		return -1, err
	}
	directory, err := h.t.openAt(folder, name, directoryFlags, 0)
	if err != nil {
		return -1, err
	}
	if inherited == nil && mode != final {
		if err = fchown(int(directory), -1, int(owner.Gid)); err == nil {
			err = fchmod(int(directory), final)
		}
	}
	if err == nil {
		err = fchown(int(directory), int(owner.Uid), int(owner.Gid))
	}
	if err != nil {
		if info, statErr := fstat(directory); statErr == nil {
			err = &unsetFolder{dev: uint64(info.Dev), ino: info.Ino, err: err}
		}
		return -1, err
	}
	return directory, nil
}

// unsetFolder is a folder makeFolder made but couldn't finish setting up.
type unsetFolder struct {
	dev, ino uint64
	err      error
}

func (e *unsetFolder) Error() string { return e.err.Error() }
func (e *unsetFolder) Unwrap() error { return e.err }

func currentUmask() int {
	old := syscall.Umask(0)
	syscall.Umask(old)
	return old
}

func (h *fileHelper) run() error {
	h.resolveExclusions()
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
	check := h.check
	if folderOperations[operation] {
		check = h.checkFolder
	}
	if err := check(relative, operation == "download" || operation == "delete" || operation == "rename"); err != nil {
		return err
	}
	switch operation {
	case "check":
		return h.json(map[string]bool{"safe": true})
	case "version":
		return h.json(map[string]int{"version": Version})
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
		if err := h.header("source/", root, info, true); err != nil {
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
			if h.excludedIn(directory, info) {
				continue
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
		return h.uploadFiles(relative)
	case "upload-prepare":
		if !h.request.List {
			h.request.Files = nil
		} else if err := h.readList(relative, false); err != nil {
			return err
		}
		folder, err := h.pinDirectory(root, relative)
		if err != nil {
			return err
		}
		// The staging files cut-off uploads left go from each folder the
		// batch writes into that exists, starting at a different one each
		// time, within one sweep's budget; with no files listed, from the
		// batch's folder. Only that folder's own sweep can fail the batch:
		// what can't be swept below it is left, and each file's own write
		// reports a folder that is wrong.
		destinations := uploadDestinations(h.request.Files)
		sweep := &sweepBudget{started: time.Now()}
		first := int(time.Now().UnixNano() % int64(len(destinations)))
		stuck := 0
		for offset := range destinations {
			if sweep.seen >= sweepEntries || time.Since(sweep.started) >= sweepTime {
				break
			}
			inside := destinations[(first+offset)%len(destinations)]
			opened := len(h.t.open)
			h.folder = path.Join(relative, inside)
			below, err := folder, error(nil)
			if inside != "" {
				below, err = h.pinDirectory(folder, inside)
			}
			var left int
			if err == nil {
				left, err = h.sweepStaging(below, sweep)
				stuck += left
			}
			h.t.closeFrom(opened)
			if err != nil && inside == "" {
				return err
			}
		}
		// A space that can't be measured is reported as none, which the backend
		// refuses, so the sweep's count still reaches it.
		var stat unix.Statfs_t
		if err := unix.Fstatfs(int(folder), &stat); err != nil {
			return h.json(UploadPrepared{Stuck: stuck})
		}
		return h.json(UploadPrepared{FileSpace: Space(&stat), Stuck: stuck})
	case "upload-files-cleanup":
		if !h.request.List {
			return errUnsafe
		}
		if err := h.readList(relative, true); err != nil {
			return err
		}
		folder, err := h.pinDirectory(root, relative)
		if err != nil {
			return err
		}
		var refused error
		for _, file := range h.request.Files {
			opened := len(h.t.open)
			inside, below, err := path.Dir(file.Name), folder, error(nil)
			if inside == "." {
				inside = ""
			} else {
				below, err = h.pinDirectory(folder, inside)
			}
			// A folder that is gone took its staging files with it.
			if err == nil {
				err = h.removeStaging(below, path.Join(relative, inside), file.UploadID)
			} else if isNotExist(err) {
				err = nil
			}
			h.t.closeFrom(opened)
			if err != nil {
				refused = err
			}
		}
		if refused != nil {
			return refused
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
		if _, err := h.makeFolder(parent, parent, name, owner); err != nil {
			return nameTaken(err)
		}
		return h.json(map[string]bool{"ok": true})
	case "delete":
		if err := h.unmounted(parent, name, relative, 0); err != nil {
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
		if err := h.unmounted(parent, name, relative, 0); err != nil {
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
		if err := h.upload(parent, name, *h.request.Size, h.request.UploadID, h.request.Expected); err != nil {
			return err
		}
		return h.json(map[string]bool{"ok": true})
	case "upload-cleanup":
		if err := h.removeStaging(parent, path.Dir(relative), h.request.UploadID); err != nil {
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
	case errors.Is(err, errArchiveDeep):
		return FileArchiveDeep
	case errors.Is(err, errACLNotKept):
		return FileACLNotKept
	case errors.Is(err, errNotFolder):
		return FileNotFolder
	case errors.Is(err, errIsFolder):
		return FileIsFolder
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
	errNotFolder    = errors.New("a file has the name of a folder the upload needs")
	errIsFolder     = errors.New("a folder has the upload's name")
)

func nameTaken(err error) error {
	if errors.Is(err, unix.EEXIST) {
		return errNameTaken
	}
	return err
}

// renameat2 is swapped in tests to act like a kernel without
// RENAME_NOREPLACE, and renameat and linkat to fail after taking effect.
var (
	renameat2 = unix.Renameat2
	renameat  = unix.Renameat
	linkat    = unix.Linkat
	// fchown and fchmod let tests see the order an upload hands over in,
	// helperUID stand in for a helper that isn't the files' owner, and
	// fsetxattr fail to keep an ACL.
	fchown    = unix.Fchown
	fchmod    = unix.Fchmod
	helperUID = os.Geteuid
	fsetxattr = unix.Fsetxattr
)

// renameNoReplace moves an entry without replacing anything with content at
// the destination: game servers write these trees while files are managed.
// A folder renames as a restore does, where the filesystem lacks
// RENAME_NOREPLACE by a plain rename to a name just checked free, which can
// only ever replace an empty folder.
func renameNoReplace(parent fd, name string, targetParent fd, targetName string, info *unix.Stat_t) error {
	if !isRegular(info) {
		return renameChecked(parent, name, targetParent, targetName)
	}
	err := renameat2(int(parent), name, int(targetParent), targetName, unix.RENAME_NOREPLACE)
	if err == nil || (!errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EINVAL)) {
		return err
	}
	// A plain rename of a file would replace one made at the destination
	// since it was checked, with its content. Linking fails if the
	// destination exists, so a file moves by link and unlink instead.
	if err := unix.Linkat(int(parent), name, int(targetParent), targetName, 0); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(parent), name, 0); err != nil {
		return errPartial
	}
	return nil
}
