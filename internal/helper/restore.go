package helper

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

type RestoreRequest struct {
	Root      string `json:"root"`
	Stage     string `json:"stage"`
	Operation string `json:"operation"`
	// Item, when set, limits the operation to that path below Root. The
	// replaced copy waits in the stage as old/item, a rolled-back one as
	// returned.
	Item string `json:"item,omitempty"`
	// Aside names the folder below Root that setAside moves live data into.
	Aside string `json:"aside,omitempty"`
}

// RestoreFailure is the only error text the restore helper prints.
const RestoreFailure = "Restore data changed, is unsafe, or cannot be accessed. Keep the server stopped and review recovery."

// StagePrefix starts the name of a restore's stage folder, which sits in the
// root it restores; no restored path may start with it.
const StagePrefix = ".ludock-restore-"

// StageName names the stage folder of the restore with that operation ID.
func StageName(id string) string { return StagePrefix + id }

// SetAsidePrefix starts the folder, beside the data it returns, that undoing
// a restore keeps what a server started elsewhere wrote meanwhile in.
const SetAsidePrefix = "ludock-set-aside-"

// SetAsideName names the set-aside folder of the restore with that ID.
func SetAsideName(id string) string { return SetAsidePrefix + id }

func validStage(name string) bool {
	id, ok := strings.CutPrefix(name, StagePrefix)
	return ok && ValidID(id)
}

var idPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// ValidID reports whether id is a lowercase RFC 4122 UUID, as Ludock gives
// operations and backups.
func ValidID(id string) bool { return idPattern.MatchString(id) }

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
func (h *restoreHelper) moveContents(source, destination fd, omitted ...string) error {
	children, err := names(source)
	if err != nil {
		return err
	}
	if len(children) > 1000000 {
		return errUnsafe
	}
	for _, name := range children {
		if slices.Contains(omitted, name) {
			continue
		}
		if strings.HasPrefix(name, StagePrefix) {
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
		// A mount inside the data, such as one the host added under a bind
		// source, is never emptied.
		if same, err := sameMount(parent, directory); err != nil || !same {
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
	// Each directory is synced once, after its entries are removed, and the
	// top-level caller syncs the parent it removed from: a sync per entry made
	// cleaning up a large world outlast its time limit.
	return nil
}

func (h *restoreHelper) pinStage(root fd, optional bool) (fd, error) {
	stage, err := h.t.openAt(root, h.request.Stage, directoryFlags, 0)
	if optional && isNotExist(err) {
		return -1, nil
	}
	return stage, err
}

// pinAside makes, or reopens after an interrupted attempt, the set-aside
// folder: a real directory on the root's mount.
func (h *restoreHelper) pinAside(root fd) (fd, error) {
	if err := unix.Mkdirat(int(root), h.request.Aside, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, err
	}
	aside, err := h.t.openAt(root, h.request.Aside, directoryFlags, 0)
	if err != nil {
		return -1, err
	}
	if same, err := sameMount(root, aside); err != nil || !same {
		return -1, errUnsafe
	}
	return aside, nil
}

func (h *restoreHelper) run() (any, error) {
	if !validStage(h.request.Stage) {
		return nil, errUnsafe
	}
	if h.request.Aside != "" {
		id, ok := strings.CutPrefix(h.request.Aside, SetAsidePrefix)
		if !ok || !ValidID(id) || h.request.Operation != "setAside" {
			return nil, errUnsafe
		}
	} else if h.request.Operation == "setAside" {
		return nil, errUnsafe
	}
	root, err := h.pinRoot()
	if err != nil {
		return nil, err
	}
	ok := map[string]bool{"ok": true}
	var item []string
	if h.request.Item != "" {
		if item, err = itemParts(h.request.Item); err != nil {
			return nil, err
		}
		if h.request.Operation == "check" {
			return h.checkItem(root, item)
		}
	}
	switch h.request.Operation {
	case "space":
		var stat unix.Statfs_t
		if err := unix.Fstatfs(int(root), &stat); err != nil {
			return nil, err
		}
		bytes := FreeBytes(&stat)
		if bytes > 1<<53-1 {
			return nil, errUnsafe
		}
		// The device identifies roots that share a filesystem and its space.
		var info unix.Stat_t
		if err := unix.Fstat(int(root), &info); err != nil {
			return nil, err
		}
		return map[string]any{"availableBytes": bytes, "device": strconv.FormatUint(uint64(info.Dev), 10)}, nil
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
		// An earlier cleanup may have removed the stage and failed to sync.
		return ok, syncAll(root)
	}
	if item != nil && h.request.Operation != "cleanup" {
		return ok, h.runItem(root, stage, item)
	}
	switch h.request.Operation {
	case "cleanup":
		if err := h.unchanged(root, h.request.Stage, stage); err != nil {
			return nil, err
		}
		if same, err := sameMount(root, stage); err != nil || !same {
			return nil, errUnsafe
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
		if err := h.moveContents(next, root); err != nil {
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
			if strings.HasPrefix(name, StagePrefix) {
				return nil, errUnsafe
			}
			if err := h.removeEntry(root, name, 0); err != nil {
				return nil, err
			}
		}
	case "setAside":
		// Like rollbackClean, but what is live is kept: a server started
		// elsewhere may have written there.
		if _, err := h.t.openAt(stage, "old", directoryFlags, 0); err != nil {
			return nil, err
		}
		aside, err := h.pinAside(root)
		if err != nil {
			return nil, err
		}
		if err := h.moveContents(root, aside, h.request.Stage, h.request.Aside); err != nil {
			return nil, err
		}
		return ok, syncAll(aside, stage, root)
	case "rollbackOld":
		old, err := h.t.openAt(stage, "old", directoryFlags, 0)
		if err != nil {
			return nil, err
		}
		if err := h.moveContents(old, root); err != nil {
			return nil, err
		}
	default:
		return nil, errUnsafe
	}
	return ok, syncAll(stage, root)
}

// sameMount reports whether two open directories are on one mount, which
// also tells apart bind mounts of one filesystem.
func sameMount(left, right fd) (bool, error) {
	a, err := mountOf(left)
	if err != nil {
		return false, err
	}
	b, err := mountOf(right)
	return err == nil && a == b, err
}

// mountOf is mountID; tests replace it, since they can't mount.
var mountOf = mountID

// removable refuses a folder that cleanup could not empty after it moved
// into the restore folder: one with another mount or a special file anywhere
// inside it.
func (h *restoreHelper) removable(parent fd, name string, depth int) error {
	h.removed++
	if depth > 64 || h.removed > 1000000 {
		return errUnsafe
	}
	directory, err := h.t.openAt(parent, name, directoryFlags, 0)
	if err != nil {
		return err
	}
	defer h.t.close(directory)
	if same, err := sameMount(parent, directory); err != nil || !same {
		return errUnsafe
	}
	children, err := names(directory)
	if err != nil {
		return err
	}
	for _, child := range children {
		info, err := lstatAt(directory, child)
		if err != nil {
			return err
		}
		if isDir(info) {
			if err := h.removable(directory, child, depth+1); err != nil {
				return err
			}
		} else if !isRegular(info) && !isSymlink(info) {
			return errUnsafe
		}
	}
	return nil
}

// mountID reads a descriptor's mount ID from statx, or from fdinfo on
// kernels before 5.8. It fails when neither knows it.
func mountID(descriptor fd) (uint64, error) {
	var info unix.Statx_t
	if err := unix.Statx(int(descriptor), "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &info); err == nil && info.Mask&unix.STATX_MNT_ID != 0 {
		return info.Mnt_id, nil
	}
	data, err := os.ReadFile("/proc/self/fdinfo/" + strconv.Itoa(int(descriptor)))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "mnt_id:"); ok {
			return strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		}
	}
	return 0, errUnsafe
}

func itemParts(item string) ([]string, error) {
	parts, err := recordParts(item)
	if err != nil || len(parts) > 62 {
		return nil, errUnsafe
	}
	for _, part := range parts {
		if strings.HasPrefix(part, StagePrefix) {
			return nil, errUnsafe
		}
	}
	return parts, nil
}

// itemParent pins the folder holding an item below base. Every folder on
// the way is a real directory on base's mount, so neither a link nor a
// nested mount can redirect the item.
func (h *restoreHelper) itemParent(base fd, parts []string) (fd, error) {
	current := base
	for _, name := range parts[:len(parts)-1] {
		next, err := h.t.openAt(current, name, directoryFlags, 0)
		if err != nil {
			return -1, err
		}
		if same, err := sameMount(base, next); err != nil || !same {
			return -1, errUnsafe
		}
		current = next
	}
	return current, nil
}

// movable returns an item's metadata, or nil when it is absent. Only folders
// and singly linked regular files on the root's filesystem move.
func movable(parent fd, name string, device uint64) (*unix.Stat_t, error) {
	info, err := lstatAt(parent, name)
	if isNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Dev != device || (!isDir(info) && (!isRegular(info) || info.Nlink != 1)) {
		return nil, errUnsafe
	}
	return info, nil
}

// move renames an item to an absent destination and proves the same inode
// arrived. An absent item may have moved in an interrupted earlier attempt,
// so both folders are synced either way.
func move(from fd, name string, to fd, target string, info *unix.Stat_t) error {
	if info == nil {
		return syncAll(from, to)
	}
	if err := unix.Renameat2(int(from), name, int(to), target, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	moved, err := lstatAt(to, target)
	if err != nil {
		return err
	}
	if !sameInode(info, moved) {
		return errUnsafe
	}
	return syncAll(from, to)
}

// checkItem reports, before the server stops, whether the item's folder
// exists, and refuses an item that cannot be replaced.
func (h *restoreHelper) checkItem(root fd, parts []string) (any, error) {
	parent, err := h.itemParent(root, parts)
	if isNotExist(err) {
		return map[string]bool{"parent": false}, nil
	}
	if err != nil {
		return nil, err
	}
	rootInfo, err := fstat(root)
	if err != nil {
		return nil, err
	}
	info, err := movable(parent, parts[len(parts)-1], rootInfo.Dev)
	if err != nil {
		return nil, err
	}
	if info != nil && isDir(info) {
		if err := h.removable(parent, parts[len(parts)-1], 0); err != nil {
			return nil, err
		}
	}
	return map[string]bool{"parent": true}, nil
}

func (h *restoreHelper) runItem(root, stage fd, parts []string) error {
	rootInfo, err := fstat(root)
	if err != nil {
		return err
	}
	name := parts[len(parts)-1]
	switch h.request.Operation {
	case "moveOld":
		old, err := h.t.openAt(stage, "old", directoryFlags, 0)
		if err != nil {
			return err
		}
		parent, err := h.itemParent(root, parts)
		if err != nil {
			return err
		}
		info, err := movable(parent, name, rootInfo.Dev)
		if err != nil {
			return err
		}
		if info != nil && isDir(info) {
			if err := h.removable(parent, name, 0); err != nil {
				return err
			}
		}
		return move(parent, name, old, "item", info)
	case "moveNew":
		next, err := h.t.openAt(stage, "new", directoryFlags, 0)
		if err != nil {
			return err
		}
		staged, err := h.itemParent(next, parts)
		if err != nil {
			return err
		}
		info, err := movable(staged, name, rootInfo.Dev)
		if err != nil {
			return err
		}
		if info == nil {
			return errUnsafe
		}
		parent, err := h.itemParent(root, parts)
		if err != nil {
			return err
		}
		return move(staged, name, parent, name, info)
	case "rollbackClean":
		// The replacement goes back into the stage rather than being deleted,
		// so nothing outside the stage is ever removed.
		if _, err := h.t.openAt(stage, "old", directoryFlags, 0); err != nil {
			return err
		}
		parent, err := h.itemParent(root, parts)
		if err != nil {
			return err
		}
		info, err := movable(parent, name, rootInfo.Dev)
		if err != nil {
			return err
		}
		// Only a live entry the swap installed is taken back: while the
		// replacement is still staged, a live entry belongs to someone else
		// and both are kept for review.
		next, err := h.t.openAt(stage, "new", directoryFlags, 0)
		if err != nil {
			return err
		}
		staged, err := h.itemParent(next, parts)
		if err != nil {
			return err
		}
		if _, err := lstatAt(staged, name); err == nil {
			if info != nil {
				return errUnsafe
			}
		} else if !isNotExist(err) {
			return err
		}
		return move(parent, name, stage, "returned", info)
	case "setAside":
		if _, err := h.t.openAt(stage, "old", directoryFlags, 0); err != nil {
			return err
		}
		parent, err := h.itemParent(root, parts)
		if err != nil {
			return err
		}
		info, err := movable(parent, name, rootInfo.Dev)
		if err != nil || info == nil {
			return err
		}
		aside, err := h.pinAside(root)
		if err != nil {
			return err
		}
		return move(parent, name, aside, name, info)
	case "rollbackOld":
		old, err := h.t.openAt(stage, "old", directoryFlags, 0)
		if err != nil {
			return err
		}
		info, err := movable(old, "item", rootInfo.Dev)
		if err != nil {
			return err
		}
		parent, err := h.itemParent(root, parts)
		if err != nil {
			return err
		}
		return move(old, "item", parent, name, info)
	}
	return errUnsafe
}

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

// ExtractRequest is restore-extract's argument. Its fields, and
// ExtractRecord's, stay in key order: the encoding is the one a map gave.
type ExtractRequest struct {
	MaxBytes     int64  `json:"maxBytes"`
	ReserveBytes *int64 `json:"reserveBytes"`
	Root         string `json:"root"`
	Stage        string `json:"stage"`
}

// ExtractRecord is the line before each entry restore-extract reads. The
// extractor refuses one with a missing or null number, except mtime.
type ExtractRecord struct {
	GID   *int64   `json:"gid"`
	Mode  *int64   `json:"mode"`
	Mtime *float64 `json:"mtime"`
	Name  string   `json:"name"`
	Size  *int64   `json:"size"`
	Type  string   `json:"type"`
	UID   *int64   `json:"uid"`
}

// checked returns the record's numbers when it is a file or directory with
// all of them in range.
func (r *ExtractRecord) checked() (size, uid, gid, mode int64, ok bool) {
	size, okSize := inRange(r.Size, 0, 1<<53-1)
	uid, okUID := inRange(r.UID, 0, 4294967294)
	gid, okGID := inRange(r.GID, 0, 4294967294)
	mode, okMode := inRange(r.Mode, -(1<<53 - 1), 1<<53-1)
	ok = (r.Type == "file" || r.Type == "directory") && okSize && okUID && okGID && okMode
	return size, uid, gid, mode, ok
}

func inRange(value *int64, low, high int64) (int64, bool) {
	if value == nil || *value < low || *value > high {
		return 0, false
	}
	return *value, true
}

// ExtractFailure is the only error text the extractor prints.
const ExtractFailure = "Restore extraction failed: the archive or destination changed or is unsafe."

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
	// The last parent stays open: archive entries arrive grouped by
	// directory.
	parentKey string
	parentFD  fd
	// dirty holds directories, by path below the destination, whose entries
	// changed and must be synced before extraction reports success.
	dirty map[string][]string
	// records counts the entries read.
	records int
}

// parent creates (0700) and pins the directories above an entry. The
// descriptor stays owned by the extractor.
func (x *extractor) parent(names []string) (fd, error) {
	if len(names) == 0 {
		return x.destination, nil
	}
	key := strings.Join(names, "/")
	if x.parentFD > 0 && key == x.parentKey {
		return x.parentFD, nil
	}
	current := x.destination
	for index, name := range names {
		if err := unix.Mkdirat(int(current), name, 0o700); err == nil {
			x.changed(names[:index])
		} else if !errors.Is(err, unix.EEXIST) {
			return -1, err
		}
		next, err := x.t.openAt(current, name, directoryFlags, 0)
		if current != x.destination {
			x.t.close(current)
		}
		if err != nil {
			return -1, err
		}
		current = next
	}
	if x.parentFD > 0 {
		x.t.close(x.parentFD)
	}
	x.parentKey, x.parentFD = key, current
	return current, nil
}

func (x *extractor) changed(names []string) {
	if x.dirty == nil {
		x.dirty = map[string][]string{}
	}
	x.dirty[strings.Join(names, "/")] = names
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

// ExtractFull is printed, with exit status ExtractFullStatus, when an entry
// would take the destination's free space below the reserve.
const (
	ExtractFull       = "Restore extraction stopped: the disk reached the configured reserve."
	ExtractFullStatus = 3
)

var errFull = errors.New("restore reserve reached")

// ExtractCut is printed, with exit status ExtractCutStatus, when the input
// ends partway through a record or a file's bytes, as when the stream to the
// helper is cut; nothing in it was refused.
const (
	ExtractCut       = "Restore extraction stopped: its input ended early."
	ExtractCutStatus = 4
)

var errCut = errors.New("restore input ended early")

// room fails unless the destination keeps reserve bytes free after an entry
// of size bytes, counted in whole filesystem blocks plus one for its inode
// and directory entry: the staging estimate cannot bound allocation.
func (x *extractor) room(size, reserve int64) error {
	var stat unix.Statfs_t
	if err := statfs(int(x.destination), &stat); err != nil {
		return err
	}
	block := max(stat.Bsize, stat.Frsize, 1)
	needed := (size+block-1)/block*block + block
	if FreeBytes(&stat) < needed+reserve {
		return errFull
	}
	return nil
}

// statfs is replaced in tests.
var statfs = unix.Fstatfs

// copyChunk bounds how much a file can write between space checks, so other
// writers to the same filesystem cannot push it far into the reserve.
const copyChunk = 1 << 20

// RunRestoreExtract writes a staged restore from stdin: each entry is one
// JSON record line followed by exactly its file bytes. Directories get their
// metadata last so file writes cannot change their times or modes.
func RunRestoreExtract(argument string, stdin io.Reader, stdout, stderr io.Writer) int {
	x := &extractor{}
	defer x.t.closeAll()
	if err := x.run(argument, stdin); err != nil {
		status, message := extractExit(err)
		io.WriteString(stderr, message)
		return status
	}
	// The count lets the sender tell a complete stream from one that ended
	// between records.
	io.WriteString(stdout, "ok "+strconv.Itoa(x.records))
	return 0
}

// extractExit reports a full disk, whether found by a space check or by a
// write or sync that another writer beat to the space, apart from failures.
func extractExit(err error) (int, string) {
	if errors.Is(err, errFull) || errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) {
		return ExtractFullStatus, ExtractFull
	}
	if errors.Is(err, errCut) {
		return ExtractCutStatus, ExtractCut
	}
	return 1, ExtractFailure
}

func (x *extractor) run(argument string, stdin io.Reader) error {
	var request ExtractRequest
	if json.Unmarshal([]byte(argument), &request) != nil || !strings.HasPrefix(request.Root, "/") || !validStage(request.Stage) {
		return errUnsafe
	}
	reserve, ok := inRange(request.ReserveBytes, 0, 1<<53-1)
	if !ok {
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
	chunk := make([]byte, copyChunk)
	count := 0
	var total float64
	for {
		line, err := reader.ReadSlice('\n')
		if err == bufio.ErrBufferFull || len(line) > 16385 {
			return errUnsafe
		}
		if err == io.EOF {
			if len(line) > 0 {
				return errCut
			}
			break
		}
		if err != nil {
			return errCut
		}
		var record ExtractRecord
		if json.Unmarshal(bytes.TrimSuffix(line, []byte("\n")), &record) != nil {
			return errUnsafe
		}
		count++
		x.records = count
		size, uid, gid, mode, ok := record.checked()
		if count > 100000 || !ok {
			return errUnsafe
		}
		// Kept in float64: past 2^53 its rounding refuses streams an exact
		// sum would take.
		total += float64(size)
		if total > float64(request.MaxBytes) {
			return errUnsafe
		}
		names, err := recordParts(record.Name)
		if err != nil || len(names) > 64 {
			return errUnsafe
		}
		if err := x.room(size, reserve); err != nil {
			return err
		}
		for _, name := range names {
			if strings.HasPrefix(name, StagePrefix) {
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
			for remaining := size; remaining > 0; {
				n := min(remaining, int64(len(chunk)))
				if _, err := io.ReadFull(reader, chunk[:n]); err != nil {
					return errCut
				}
				if err := x.room(remaining, reserve); err != nil {
					return err
				}
				if _, err := fdWriter(file).Write(chunk[:n]); err != nil {
					return err
				}
				remaining -= n
			}
			if err := metadata(file, uid, gid, mode, record.Mtime); err != nil {
				return err
			}
			if err := unix.Fsync(int(file)); err != nil {
				return err
			}
			x.t.close(file)
		}
		x.changed(names[:len(names)-1])
	}
	// Files were synced as written; their directories are synced once here,
	// before success is reported, rather than after every entry.
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
		delete(x.dirty, strings.Join(record.names, "/"))
	}
	for key, names := range x.dirty {
		if key == "" {
			continue
		}
		directory, err := x.parent(names)
		if err != nil {
			return err
		}
		if err := unix.Fsync(int(directory)); err != nil {
			return err
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
