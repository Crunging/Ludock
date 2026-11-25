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
	"unicode/utf8"
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
	// Aside names the folder below Root that setAside moves live data into,
	// and rollbackOld any live entry in an original's way.
	Aside string `json:"aside,omitempty"`
	// Base is the helper's mount that holds Root. Moving live data out
	// refuses any other mount on its way or inside it.
	Base string `json:"base,omitempty"`
}

// The restore helper's operations, named by RestoreRequest.Operation.
const (
	RestoreCheck         = "check"
	RestoreSpace         = "space"
	RestoreStageExists   = "stageExists"
	RestoreStage         = "stage"
	RestoreCleanup       = "cleanup"
	RestoreMoveOld       = "moveOld"
	RestoreMoveNew       = "moveNew"
	RestoreRollbackClean = "rollbackClean"
	RestoreSetAside      = "setAside"
	RestoreRollbackOld   = "rollbackOld"
)

// RestoreFailure is the only error text the restore helper prints.
const RestoreFailure = "Restore data changed, is unsafe, or cannot be accessed. Keep the server stopped and review recovery."

// RestoreMounted is the restore helper's exit code for data that holds, or
// is reached through, another mount. It matches FileMounted, so one refusal
// covers both helpers.
const RestoreMounted = FileMounted

// RestoreHoldsStage is the restore helper's exit code for data that holds
// another restore's stage folder below its top, which a restore never moves
// or removes. It matches FileHoldsRestore.
const RestoreHoldsStage = FileHoldsRestore

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
		// Ludock's own names, such as another restore's stage, stay put.
		if slices.Contains(omitted, name) || Reserved(name) {
			continue
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
		if err := renameChecked(source, name, destination, name); err != nil {
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

// renameChecked renames to a name the caller found free. A game started
// meanwhile may make it after that check, which RENAME_NOREPLACE refuses; a
// filesystem without it gets the plain rename whole restores always used.
func renameChecked(from fd, name string, to fd, target string) error {
	err := renameat2(int(from), name, int(to), target, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		err = renameat(int(from), name, int(to), target)
	}
	return err
}

func (h *restoreHelper) removeEntry(parent fd, name string, depth int) error {
	h.removed++
	if depth > 64 || h.removed > 1000000 {
		return errUnsafe
	}
	// Only another restore's stage has this name here: this one's is never
	// passed in, and its old copy can't hold one.
	if validStage(name) {
		return errHoldsRestore
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
// folder: a real directory on the root's mount, owned like the root once
// undone. It is owned before anything moves in, so a retry fixes one that
// an interruption left as the helper made it.
func (h *restoreHelper) pinAside(root, stage fd) (fd, error) {
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
	previous, err := rootBefore(root, stage)
	if err != nil {
		return -1, err
	}
	if err := ownRoot(aside, previous); err != nil {
		return -1, err
	}
	return aside, nil
}

// asideFor checks that a live entry in parent, in the way of an original,
// can move, and pins the set-aside folder once one must: a server started
// since the restore stopped may have made a newer copy by that name.
func (h *restoreHelper) asideFor(root, stage, parent, aside fd, name string, info *unix.Stat_t) (fd, error) {
	if h.request.Aside == "" {
		return -1, errUnsafe
	}
	// Held to moveOld's standard for moving live data: no other mount or
	// special file inside, and no other restore's stage.
	if isDir(info) {
		if err := h.safeToMove(parent, name, 0, true); err != nil {
			return -1, err
		}
	}
	if aside >= 0 {
		return aside, nil
	}
	return h.pinAside(root, stage)
}

// setAsideEntry moves a live entry into the set-aside folder under a name
// the folder doesn't hold yet, since an earlier attempt may have set a copy
// aside by that name.
func setAsideEntry(parent fd, name string, aside fd, info *unix.Stat_t) error {
	if info == nil {
		return syncAll(parent, aside)
	}
	target := name
	for count := 2; ; count++ {
		if _, err := lstatAt(aside, target); isNotExist(err) {
			break
		} else if err != nil {
			return err
		} else if count > 1000 {
			return errUnsafe
		}
		// The numbered name still fits the filesystem's 255-byte limit.
		suffix, base := " ("+strconv.Itoa(count)+")", name
		for len(base)+len(suffix) > 255 {
			base = base[:len(base)-1]
		}
		for utf8.ValidString(name) && !utf8.ValidString(base) {
			base = base[:len(base)-1]
		}
		target = base + suffix
	}
	if err := renameChecked(parent, name, aside, target); err != nil {
		return err
	}
	moved, err := lstatAt(aside, target)
	if err != nil {
		return err
	}
	if !sameInode(info, moved) {
		return errUnsafe
	}
	return syncAll(parent, aside)
}

func (h *restoreHelper) run() (any, error) {
	if !validStage(h.request.Stage) {
		return nil, errUnsafe
	}
	if h.request.Aside != "" {
		id, ok := strings.CutPrefix(h.request.Aside, SetAsidePrefix)
		if !ok || !ValidID(id) || (h.request.Operation != RestoreSetAside && h.request.Operation != RestoreRollbackOld) {
			return nil, errUnsafe
		}
	} else if h.request.Operation == RestoreSetAside {
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
		if h.request.Operation == RestoreCheck {
			return h.checkItem(root, item)
		}
	}
	switch h.request.Operation {
	case RestoreSpace:
		return h.space(root)
	case RestoreStageExists:
		stage, err := h.pinStage(root, true)
		return map[string]bool{"exists": stage >= 0}, err
	case RestoreStage:
		return ok, h.makeStage(root)
	}
	stage, err := h.pinStage(root, h.request.Operation == RestoreCleanup)
	if err != nil {
		return nil, err
	}
	if stage < 0 {
		// An earlier cleanup may have removed the stage and failed to sync.
		return ok, syncAll(root)
	}
	if item != nil && h.request.Operation != RestoreCleanup {
		if err := h.runItem(root, stage, item); err != nil {
			return nil, err
		}
		return h.done(root)
	}
	switch h.request.Operation {
	case RestoreCleanup:
		return ok, h.cleanup(root, stage)
	case RestoreMoveOld:
		err = h.moveOld(root, stage)
	case RestoreMoveNew:
		err = h.moveNew(root, stage)
	case RestoreRollbackClean:
		err = h.rollbackClean(root, stage)
	case RestoreSetAside:
		return ok, h.setAside(root, stage)
	case RestoreRollbackOld:
		err = h.rollbackOld(root, stage)
	default:
		return nil, errUnsafe
	}
	if err != nil {
		return nil, err
	}
	if err := syncAll(stage, root); err != nil {
		return nil, err
	}
	return h.done(root)
}

// done answers a step that ran. rollbackOld also reports whether the root
// holds the set-aside folder, which a retry after an interruption still
// finds.
func (h *restoreHelper) done(root fd) (any, error) {
	if h.request.Operation != RestoreRollbackOld || h.request.Aside == "" {
		return map[string]bool{"ok": true}, nil
	}
	_, err := lstatAt(root, h.request.Aside)
	if err != nil && !isNotExist(err) {
		return nil, err
	}
	return map[string]bool{"ok": true, "kept": err == nil}, nil
}

func (h *restoreHelper) space(root fd) (any, error) {
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
}

func (h *restoreHelper) makeStage(root fd) error {
	if err := unix.Mkdirat(int(root), h.request.Stage, 0o700); err != nil {
		return err
	}
	stage, err := h.pinStage(root, false)
	if err != nil {
		return err
	}
	for _, name := range []string{"new", "old"} {
		if err := unix.Mkdirat(int(stage), name, 0o700); err != nil {
			return err
		}
	}
	next, err := h.t.openAt(stage, "new", directoryFlags, 0)
	if err != nil {
		return err
	}
	old, err := h.t.openAt(stage, "old", directoryFlags, 0)
	if err != nil {
		return err
	}
	return syncAll(next, old, stage, root)
}

func (h *restoreHelper) cleanup(root, stage fd) error {
	if err := h.unchanged(root, h.request.Stage, stage); err != nil {
		return err
	}
	if same, err := sameMount(root, stage); err != nil || !same {
		return errUnsafe
	}
	children, err := names(stage)
	if err != nil {
		return err
	}
	for _, name := range children {
		if err := h.removeEntry(stage, name, 0); err != nil {
			return err
		}
	}
	if err := syncAll(stage); err != nil {
		return err
	}
	if err := h.unchanged(root, h.request.Stage, stage); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(root), h.request.Stage, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return syncAll(root)
}

func (h *restoreHelper) moveOld(root, stage fd) error {
	old, err := h.t.openAt(stage, "old", directoryFlags, 0)
	if err != nil {
		return err
	}
	// Cleanup could never remove a mount that moved into the stage, which
	// would leave the old data there with the live paths changed.
	if err := ownMount(h.request.Base, h.request.Root); err != nil {
		return err
	}
	if err := h.ownTree(root, true); err != nil {
		return err
	}
	if err := h.recordRoot(root, stage); err != nil {
		return err
	}
	return h.moveContents(root, old, h.request.Stage)
}

func (h *restoreHelper) moveNew(root, stage fd) error {
	next, err := h.t.openAt(stage, "new", directoryFlags, 0)
	if err != nil {
		return err
	}
	if err := h.moveContents(next, root); err != nil {
		return err
	}
	// The extractor gave new the archived root's owner, mode, and ACL. The
	// root keeps its own ACL; one without takes the archived ACL's fallback
	// mode, not its mask as the group's rights.
	archived, err := fstat(next)
	if err != nil {
		return err
	}
	restored, err := getxattr(next, accessACL)
	if err != nil {
		return err
	}
	if restored != nil {
		live, err := getxattr(root, accessACL)
		if err != nil {
			return err
		}
		if live == nil {
			entries, err := decodeACL(restored)
			if err != nil {
				return err
			}
			archived.Mode = fallbackMode(archived.Mode, entries)
		}
	}
	return ownRoot(root, archived)
}

func (h *restoreHelper) rollbackClean(root, stage fd) error {
	// Prove the old data exists before deleting a failed replacement.
	if _, err := h.t.openAt(stage, "old", directoryFlags, 0); err != nil {
		return err
	}
	if err := h.ownTree(root, false); err != nil {
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
		if err := h.removeEntry(root, name, 0); err != nil {
			return err
		}
	}
	return nil
}

func (h *restoreHelper) setAside(root, stage fd) error {
	// Like rollbackClean, but what is live is kept: a server started
	// elsewhere may have written there.
	if _, err := h.t.openAt(stage, "old", directoryFlags, 0); err != nil {
		return err
	}
	if err := h.ownTree(root, false); err != nil {
		return err
	}
	aside, err := h.pinAside(root, stage)
	if err != nil {
		return err
	}
	rootInfo, err := fstat(root)
	if err != nil {
		return err
	}
	children, err := names(root)
	if err != nil {
		return err
	}
	if len(children) > 1000000 {
		return errUnsafe
	}
	for _, name := range children {
		if name == h.request.Stage || name == h.request.Aside || Reserved(name) {
			continue
		}
		info, err := movable(root, name, rootInfo.Dev)
		if err != nil {
			return err
		}
		if err := setAsideEntry(root, name, aside, info); err != nil {
			return err
		}
	}
	return syncAll(aside, stage, root)
}

// rollbackOld returns the originals. A live entry holding an original's
// name moves aside first; every move is one rename, so a retry after any
// interruption finds both copies and goes on from there.
func (h *restoreHelper) rollbackOld(root, stage fd) error {
	old, err := h.t.openAt(stage, "old", directoryFlags, 0)
	if err != nil {
		return err
	}
	rootInfo, err := fstat(root)
	if err != nil {
		return err
	}
	children, err := names(old)
	if err != nil {
		return err
	}
	if len(children) > 1000000 {
		return errUnsafe
	}
	var aside fd = -1
	for _, name := range children {
		if Reserved(name) {
			continue
		}
		info, err := movable(root, name, rootInfo.Dev)
		if err != nil {
			return err
		}
		if info == nil {
			continue
		}
		if aside, err = h.asideFor(root, stage, root, aside, name, info); err != nil {
			return err
		}
		if err := setAsideEntry(root, name, aside, info); err != nil {
			return err
		}
	}
	if err := h.moveContents(old, root); err != nil {
		return err
	}
	previous, err := rootBefore(root, stage)
	if err != nil {
		return err
	}
	return ownRoot(root, previous)
}

// rootBefore returns the root's owner and mode from before the restore.
// Without a record, moveOld never ran, so the root's own never changed.
func rootBefore(root, stage fd) (*unix.Stat_t, error) {
	previous, err := lstatAt(stage, rootRecord)
	if err == nil && isRegular(previous) {
		return previous, nil
	}
	if err != nil && !isNotExist(err) {
		return nil, err
	}
	return fstat(root)
}

// rootRecord is the file in a stage whose owner and mode are the root's
// own from before the restore, which undoing it puts back.
const rootRecord = "root"

// recordRoot saves the root's owner and mode in the stage once, before
// anything moves: a repeated moveOld finds the root still as it was.
func (h *restoreHelper) recordRoot(root, stage fd) error {
	if _, err := lstatAt(stage, rootRecord); err == nil || !isNotExist(err) {
		return err
	}
	info, err := fstat(root)
	if err != nil {
		return err
	}
	temporary := rootRecord + ".new"
	if err := unix.Unlinkat(int(stage), temporary, 0); err != nil && !isNotExist(err) {
		return err
	}
	file, err := h.t.openAt(stage, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	if err := unix.Fchown(int(file), int(info.Uid), int(info.Gid)); err != nil {
		return err
	}
	if err := unix.Fchmod(int(file), info.Mode&0o7777); err != nil {
		return err
	}
	if err := unix.Fsync(int(file)); err != nil {
		return err
	}
	if err := unix.Renameat(int(stage), temporary, int(stage), rootRecord); err != nil {
		return err
	}
	return syncAll(stage)
}

// ownRoot gives a folder the owner and mode of from, changing only what
// differs, so a filesystem that refuses ownership changes refuses only real
// ones.
func ownRoot(root fd, from *unix.Stat_t) error {
	current, err := fstat(root)
	if err != nil {
		return err
	}
	if current.Uid != from.Uid || current.Gid != from.Gid {
		if err := unix.Fchown(int(root), int(from.Uid), int(from.Gid)); err != nil {
			return err
		}
		// chown can clear setgid, so the mode is compared after it.
		if current, err = fstat(root); err != nil {
			return err
		}
	}
	if current.Mode&0o7777 != from.Mode&0o7777 {
		return unix.Fchmod(int(root), from.Mode&0o7777)
	}
	return nil
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
// inside it, or another restore's stage.
func (h *restoreHelper) removable(parent fd, name string, depth int) error {
	return h.safeToMove(parent, name, depth, true)
}

// safeToMove refuses a folder holding another restore's stage at any depth,
// which may be the only copy of that server's previous data, and with whole,
// one that removable refuses.
func (h *restoreHelper) safeToMove(parent fd, name string, depth int, whole bool) error {
	h.removed++
	if depth > 64 || h.removed > 1000000 {
		return errUnsafe
	}
	directory, err := h.t.openAt(parent, name, directoryFlags, 0)
	if err != nil {
		return err
	}
	defer h.t.close(directory)
	if same, err := sameMount(parent, directory); err != nil {
		return err
	} else if !same && whole {
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
		info, err := lstatAt(directory, child)
		if err != nil {
			return err
		}
		if isDir(info) {
			if err := h.safeToMove(directory, child, depth+1, whole); err != nil {
				return err
			}
		} else if whole && !isRegular(info) && !isSymlink(info) {
			return errUnsafe
		}
	}
	return nil
}

// ownTree refuses, before anything moves, live data holding another
// restore's stage below the top and, with whole, live data that cleanup
// could not remove once moved: an entry on another mount, or a folder holding
// one or a special file.
func (h *restoreHelper) ownTree(root fd, whole bool) error {
	children, err := names(root)
	if err != nil {
		return err
	}
	for _, name := range children {
		if Reserved(name) || name == h.request.Aside {
			continue
		}
		info, err := lstatAt(root, name)
		if err != nil {
			return err
		}
		if isDir(info) {
			if err := h.safeToMove(root, name, 0, whole); err != nil {
				return err
			}
			continue
		}
		if !whole {
			continue
		}
		entry, err := h.t.openAt(root, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		same, err := sameMount(root, entry)
		h.t.close(entry)
		if err != nil {
			return err
		}
		if !same {
			return errMounted
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
	if slices.ContainsFunc(parts, Reserved) {
		return nil, errUnsafe
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
	if err := ownMount(h.request.Base, h.request.Root+"/"+strings.Join(parts, "/")); err != nil {
		return nil, err
	}
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
	case RestoreMoveOld:
		old, err := h.t.openAt(stage, "old", directoryFlags, 0)
		if err != nil {
			return err
		}
		if err := ownMount(h.request.Base, h.request.Root+"/"+strings.Join(parts, "/")); err != nil {
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
	case RestoreMoveNew:
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
	case RestoreRollbackClean:
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
		if info != nil && isDir(info) {
			if err := h.safeToMove(parent, name, 0, false); err != nil {
				return err
			}
		}
		return move(parent, name, stage, "returned", info)
	case RestoreSetAside:
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
		if isDir(info) {
			if err := h.safeToMove(parent, name, 0, false); err != nil {
				return err
			}
		}
		aside, err := h.pinAside(root, stage)
		if err != nil {
			return err
		}
		return setAsideEntry(parent, name, aside, info)
	case RestoreRollbackOld:
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
		if info != nil {
			live, err := movable(parent, name, rootInfo.Dev)
			if err != nil {
				return err
			}
			if live != nil {
				aside, err := h.asideFor(root, stage, parent, -1, name, live)
				if err != nil {
					return err
				}
				if err := setAsideEntry(parent, name, aside, live); err != nil {
					return err
				}
			}
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
		switch {
		case errors.Is(err, errMounted):
			return RestoreMounted
		case errors.Is(err, errHoldsRestore):
			return RestoreHoldsStage
		}
		return 1
	}
	encoded, _ := json.Marshal(result)
	stdout.Write(encoded)
	return 0
}

// ExtractRequest is restore-extract's argument.
type ExtractRequest struct {
	MaxBytes     int64  `json:"maxBytes"`
	ReserveBytes *int64 `json:"reserveBytes"`
	Root         string `json:"root"`
	Stage        string `json:"stage"`
}

// ExtractRecord is the line before each entry restore-extract reads. The
// extractor refuses one with a missing or null number, except mtime. ACL and
// DefaultACL are the entry's ACLs in ACLText's form, if it has them.
type ExtractRecord struct {
	ACL        string   `json:"acl,omitempty"`
	DefaultACL string   `json:"defaultAcl,omitempty"`
	GID        *int64   `json:"gid"`
	Mode       *int64   `json:"mode"`
	Mtime      *float64 `json:"mtime"`
	Name       string   `json:"name"`
	Size       *int64   `json:"size"`
	Type       string   `json:"type"`
	UID        *int64   `json:"uid"`
}

// acls returns the record's ACLs in the kernel's form. Only a folder has a
// default ACL.
func (r *ExtractRecord) acls() (entryACLs, bool) {
	var acls entryACLs
	for _, field := range []struct {
		text   string
		target *[]aclEntry
	}{{r.ACL, &acls.access}, {r.DefaultACL, &acls.def}} {
		if field.text == "" {
			continue
		}
		value, err := ParseACLText(field.text)
		if err != nil {
			return acls, false
		}
		*field.target, _ = decodeACL(value)
	}
	return acls, acls.def == nil || r.Type == "directory"
}

type entryACLs struct{ access, def []aclEntry }

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
	// aclsLost says the destination refused an ACL an entry had.
	aclsLost bool
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

// metadata gives an extracted entry its owner, then its ACLs and mode, since
// chown can clear setgid.
func (x *extractor) metadata(descriptor fd, uid, gid, mode int64, mtime *float64, directory bool, acls entryACLs) error {
	if err := unix.Fchown(int(descriptor), int(uid), int(gid)); err != nil {
		return err
	}
	// The only place an archive's mode is applied. The restore runs as root
	// and gives entries the archive's owners, so honoring setuid, or setgid
	// on a file, would let whoever can write to the backup folder plant a
	// privileged program in a game's data. Folders keep setgid and sticky.
	permissions := uint32(mode & 0o777)
	if directory {
		permissions = uint32(mode & 0o3777)
	}
	// An ACL the destination refuses leaves the archive's mode, which gives
	// no one more than the ACL did, and none inherited from the stage's
	// folders, which could grant what the entry's own didn't. The mode is
	// set after the ACL, whose own bits it then matches, since setting an
	// ACL can clear a folder's setgid.
	for _, acl := range []struct {
		attribute string
		entries   []aclEntry
	}{{accessACL, acls.access}, {defaultACL, acls.def}} {
		if acl.entries == nil {
			continue
		}
		err := fsetxattr(int(descriptor), acl.attribute, encodeACL(acl.entries), 0)
		if err == nil {
			if acl.attribute == accessACL {
				permissions = permissions&^0o777 | aclMode(acl.entries)
			}
			continue
		}
		if !refusedACL(err) {
			return err
		}
		x.aclsLost = true
		if err := unix.Fremovexattr(int(descriptor), acl.attribute); err != nil && !unsupportedXattr(err) {
			return err
		}
	}
	// A folder whose ACL was recorded without a default one gets none from
	// the stage's folders for what is made in it later.
	if directory && acls.access != nil && acls.def == nil {
		if err := unix.Fremovexattr(int(descriptor), defaultACL); err != nil && !unsupportedXattr(err) {
			return err
		}
	}
	if err := unix.Fchmod(int(descriptor), permissions); err != nil {
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

// refusedACL is an ACL the destination's filesystem can't hold: one without
// ACLs, or one that can't store its IDs or its size.
func refusedACL(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.E2BIG) || errors.Is(err, unix.EOVERFLOW)
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

// ExtractACLsLost follows the count restore-extract prints when the
// destination refused an entry's ACLs.
const ExtractACLsLost = "acls-lost"

// maxRecordLine bounds a record line: a name escaped to its longest and two
// ACLs of MaxACLEntries entries fit.
const maxRecordLine = 1 << 17

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
	report := "ok " + strconv.Itoa(x.records)
	if x.aclsLost {
		report += " " + ExtractACLsLost
	}
	io.WriteString(stdout, report)
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
		acls           entryACLs
	}
	var directories []directoryRecord
	var rootMeta *directoryRecord
	reader := bufio.NewReaderSize(stdin, maxRecordLine)
	chunk := make([]byte, copyChunk)
	count := 0
	var total float64
	for {
		line, err := reader.ReadSlice('\n')
		if err == bufio.ErrBufferFull || len(line) > maxRecordLine {
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
		acls, aclsOK := record.acls()
		if count > 100000 || !ok || !aclsOK {
			return errUnsafe
		}
		// Kept in float64: past 2^53 its rounding refuses streams an exact
		// sum would take.
		total += float64(size)
		if total > float64(request.MaxBytes) {
			return errUnsafe
		}
		// The root's own record, first if sent, is the stage's new folder.
		if record.Name == "" {
			if count != 1 || record.Type != "directory" || size != 0 {
				return errUnsafe
			}
			rootMeta = &directoryRecord{nil, uid, gid, mode, nil, acls}
			continue
		}
		names, err := recordParts(record.Name)
		if err != nil || len(names) > 64 {
			return errUnsafe
		}
		if err := x.room(size, reserve); err != nil {
			return err
		}
		if slices.ContainsFunc(names, Reserved) {
			return errUnsafe
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
			directories = append(directories, directoryRecord{names, uid, gid, mode, record.Mtime, acls})
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
			if err := x.metadata(file, uid, gid, mode, record.Mtime, false, acls); err != nil {
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
		if err := x.metadata(directory, record.uid, record.gid, record.mode, record.mtime, true, record.acls); err != nil {
			return err
		}
		if err := unix.Fsync(int(directory)); err != nil {
			return err
		}
		x.t.close(directory)
		delete(x.dirty, strings.Join(record.names, "/"))
	}
	if rootMeta != nil {
		// The stage's new folder carries the root's ACL to moveNew, so it
		// keeps none it inherited from the stage.
		if rootMeta.acls.access == nil {
			if err := unix.Fremovexattr(int(x.destination), accessACL); err != nil && !unsupportedXattr(err) {
				return err
			}
		}
		if err := x.metadata(x.destination, rootMeta.uid, rootMeta.gid, rootMeta.mode, nil, true, rootMeta.acls); err != nil {
			return err
		}
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
