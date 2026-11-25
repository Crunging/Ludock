package helper

import (
	"errors"

	"golang.org/x/sys/unix"
)

// The helper leaves an excluded mount out, so at its destination it sees the
// parent mount's own entry, which the game never sees. Paths are compared
// by spelling, but storage that matches names case-insensitively or with
// normalization opens that entry under other spellings too, so the
// exclusion also holds by identity: an entry is refused when it is what a
// destination's path names now, or named when the command began.
type exclusions struct {
	// start is where the paths led when the command began; its folders and
	// entries stay open until it ends, since some filesystems, such as exFAT,
	// number an entry anew once it leaves their cache.
	start snapshot
	// current is where they lead now, walked again for each folder looked
	// into: once for a whole listing, once per folder of a recursive walk.
	current snapshot
	at      identity
	fresh   bool
}

// snapshot is where the excluded destinations' paths lead at one moment.
type snapshot struct {
	// entries are the destinations themselves, which nothing may open or
	// change.
	entries map[identity]bool
	// folders are those on the way to one, which a change to a whole folder
	// (delete, rename, move, download) may not take.
	folders map[identity]bool
	// stops are, for a path that stops short, the deepest folder found, with
	// the name missing from it. Nothing is written into it, since an entry
	// made there could become the destination, and one that has appeared
	// under that name is refused like an excluded entry.
	stops map[identity][]missingName
	// bindings are, for a path that reaches its last folder, that folder and
	// the destination's name in it, looked up live at each check, so an entry
	// replaced since the snapshot is still recognized.
	bindings map[identity][]missingName
}

// missingName is a name a path lacks; only its last one is the destination
// itself, and any other must be a folder to lead there.
type missingName struct {
	name string
	last bool
}

type identity struct{ device, inode uint64 }

func identityOf(info *unix.Stat_t) identity { return identity{uint64(info.Dev), info.Ino} }

var errExcluded = errors.New("this path overlaps an excluded mount")

// exclusionsResolved runs once the exclusions are resolved; tests use it to
// move an excluded entry to another spelling.
var exclusionsResolved = func() {}

// folderOperations don't change the entry their path names in its folder:
// they write inside the folder it names, or only remove Ludock's own
// staging beside it, which can't become an exclusion.
var folderOperations = map[string]bool{"upload-files": true, "upload-prepare": true, "upload-files-cleanup": true, "delete-paths": true,
	"move": true, "upload-cleanup": true, "unpack-cleanup": true}

// readOnlyOperations are the file operations that write nothing.
var readOnlyOperations = map[string]bool{"list": true, "stat": true, "download": true, "download-paths": true, "check": true, "upload-version": true,
	"space": true, "mounts": true, "backup": true}

// resolveExclusions takes the start's snapshot, then refuses any later open
// of an excluded entry.
func (h *fileHelper) resolveExclusions() {
	if len(h.request.Blocked) == 0 {
		return
	}
	h.excluded.start = h.snapshot(func(descriptor int) { h.t.add(fd(descriptor)) })
	h.t.refuse = h.excludedIn
	exclusionsResolved()
}

// snapshot walks every destination from "/" by its spelling, without
// following links or the tracker's refusal. keep, when set, keeps each
// descriptor it opens.
func (h *fileHelper) snapshot(keep func(int)) snapshot {
	taken := snapshot{entries: map[identity]bool{}, folders: map[identity]bool{}, stops: map[identity][]missingName{},
		bindings: map[identity][]missingName{}}
	for _, destination := range h.request.Blocked {
		taken.walk(destination, keep)
	}
	return taken
}

func (s snapshot) walk(destination string, keep func(int)) {
	release := func(descriptor int) {
		if keep != nil {
			keep(descriptor)
		} else {
			unix.Close(descriptor)
		}
	}
	parts, err := components(destination)
	if err != nil || len(parts) == 0 {
		return
	}
	current, err := unix.Open("/", directoryFlags, 0)
	if err != nil {
		return
	}
	defer func() { release(current) }()
	for index, part := range parts {
		var folder unix.Stat_t
		if unix.Fstat(current, &folder) != nil {
			return
		}
		s.folders[identityOf(&folder)] = true
		if index == len(parts)-1 {
			s.bindings[identityOf(&folder)] = append(s.bindings[identityOf(&folder)], missingName{part, true})
			if entry, err := unix.Openat(current, part, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0); err == nil {
				var info unix.Stat_t
				if unix.Fstat(entry, &info) == nil {
					s.entries[identityOf(&info)] = true
				}
				release(entry)
				return
			}
			s.stops[identityOf(&folder)] = append(s.stops[identityOf(&folder)], missingName{part, true})
			return
		}
		next, err := unix.Openat(current, part, directoryFlags, 0)
		if err != nil {
			s.stops[identityOf(&folder)] = append(s.stops[identityOf(&folder)], missingName{part, false})
			return
		}
		release(current)
		current = next
	}
}

// currentTaken runs after the current snapshot is retaken for folder; tests
// use it to replace an entry after it.
var currentTaken = func(folder *unix.Stat_t) {}

// now is where the paths lead as of looking into folder.
func (h *fileHelper) now(folder *unix.Stat_t) snapshot {
	if !h.excluded.fresh || h.excluded.at != identityOf(folder) {
		h.excluded.current, h.excluded.at, h.excluded.fresh = h.snapshot(nil), identityOf(folder), true
		currentTaken(folder)
	}
	return h.excluded.current
}

// excludedIn reports an entry of parent that an excluded mount hides from
// the game: what a destination's path names now or named at the start,
// including one that has appeared where a path stopped short. Storage that
// matches names loosely may reach it under another spelling, which names an
// entry in the same folder, so the destination's name is looked up live in
// parent and compared by identity. A folder that held a destination at the
// start can't be told apart from that folder reached under another spelling,
// so its names are looked up too, even if it has since been moved away: that
// can hide a file the game sees, never show one it doesn't. A hard link
// elsewhere is a name the game sees, so only the snapshots match it.
func (h *fileHelper) excludedIn(parent fd, info *unix.Stat_t) bool {
	if len(h.request.Blocked) == 0 || info == nil {
		return false
	}
	if h.excluded.start.entries[identityOf(info)] {
		return true
	}
	folder, err := fstat(parent)
	if err != nil {
		return true
	}
	for _, taken := range []snapshot{h.excluded.start, h.now(folder)} {
		if taken.entries[identityOf(info)] {
			return true
		}
		for _, missing := range append(taken.bindings[identityOf(folder)], taken.stops[identityOf(folder)]...) {
			if entry, err := lstatAt(parent, missing.name); err == nil && sameInode(entry, info) && (missing.last || isDir(entry)) {
				return true
			}
		}
	}
	return false
}

// stoppedAt reports a folder a path stops short at, at the start or now.
func (h *fileHelper) stoppedAt(folder *unix.Stat_t) bool {
	return h.excluded.start.stops[identityOf(folder)] != nil || h.now(folder).stops[identityOf(folder)] != nil
}

// onPath reports a folder on the way to a destination, at the start or now.
func (h *fileHelper) onPath(folder *unix.Stat_t) bool {
	return h.excluded.start.folders[identityOf(folder)] || h.now(folder).folders[identityOf(folder)]
}

// checkIdentity refuses relative, below the root, when it is an excluded
// entry or, for a change to it as a whole (recursive), holds one; and a write
// below the parent folder of an exclusion that couldn't be resolved. Folders
// on the way are refused as they open.
func (h *fileHelper) checkIdentity(relative string, recursive bool) (err error) {
	return h.checkIdentityOf(relative, recursive, true)
}

// checkIdentityOf is checkIdentity; entry says relative names an entry that
// is changed in its folder, rather than a folder written inside.
func (h *fileHelper) checkIdentityOf(relative string, recursive, entry bool) (err error) {
	if len(h.request.Blocked) == 0 {
		return nil
	}
	// Ludock's own names, such as a staging folder, are checked here too;
	// requests can't name them, which callers check.
	parts, err := components(relative)
	if err != nil {
		return err
	}
	opened := len(h.t.open)
	defer h.t.closeFrom(opened)
	folder, err := h.pinRoot()
	if err != nil {
		return err
	}
	var target *unix.Stat_t
	if len(parts) == 0 {
		if target, err = fstat(folder); err != nil {
			return err
		}
	} else {
		defer func() {
			if parent, statErr := fstat(folder); entry && err == nil && statErr == nil && h.writesInto(parent) {
				err = errExcluded
			}
		}()
		for _, part := range parts[:len(parts)-1] {
			if folder, err = h.t.openAt(folder, part, directoryFlags, 0); errors.Is(err, errExcluded) {
				return err
			} else if err != nil {
				// What doesn't exist yet holds no exclusion; the change itself
				// reports why it can't go on.
				return nil
			}
		}
		if target, err = lstatAt(folder, parts[len(parts)-1]); err != nil {
			return nil
		}
	}
	// Only a folder can be on the way to a destination.
	if h.excludedIn(folder, target) || (recursive && isDir(target) && h.onPath(target)) {
		return errExcluded
	}
	return nil
}

// checkFolder is check for a folder written inside, not changed itself.
func (h *fileHelper) checkFolder(relative string, recursive bool) error {
	if h.blocked(h.absolute(relative), recursive) {
		return errExcluded
	}
	return h.checkIdentityOf(relative, recursive, false)
}

// writesInto reports a write into a folder an unresolved exclusion stopped
// at; a move checks its destination with it.
func (h *fileHelper) writesInto(folder *unix.Stat_t) bool {
	return !readOnlyOperations[h.request.Operation] && len(h.request.Blocked) > 0 && h.stoppedAt(folder)
}
