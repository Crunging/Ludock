package helper

import (
	"errors"

	"golang.org/x/sys/unix"
)

// The helper leaves an excluded mount out, so at its destination it sees the
// parent mount's own entry, which the game never sees. Paths are compared
// by spelling, but storage that matches names case-insensitively or with
// normalization opens that entry under other spellings too, so the
// exclusion also holds by identity: each destination is resolved once, at
// the start of a command, to the entry it names and the folders on the way
// to it.
type exclusions struct {
	// entries are the excluded destinations themselves, which nothing may
	// open or change.
	entries map[identity]bool
	// ancestors are the folders that hold one, which a change to a whole
	// folder (delete, rename, move, download) may not take.
	ancestors map[identity]bool
	// unresolved are, for destinations whose entry couldn't be found, the
	// deepest folder found on the way, with the names missing from it.
	// Nothing is written into it, since an entry made there could become
	// the destination, and one that appears under such a name later in the
	// command is refused like an excluded entry.
	unresolved map[identity][]string
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
var readOnlyOperations = map[string]bool{"list": true, "stat": true, "download": true, "download-paths": true, "check": true,
	"space": true, "mounts": true, "backup": true}

// resolveExclusions walks each excluded destination from "/" without
// following links, then refuses any later open of one of its entries.
func (h *fileHelper) resolveExclusions() {
	if len(h.request.Blocked) == 0 {
		return
	}
	h.excluded = exclusions{entries: map[identity]bool{}, ancestors: map[identity]bool{}, unresolved: map[identity][]string{}}
	for _, destination := range h.request.Blocked {
		h.resolveExclusion(destination)
	}
	h.t.refuse = h.excludedIn
	exclusionsResolved()
}

// resolveExclusion records a destination's entry and the folders on its
// way, holding each open until the command ends: some filesystems, such as
// exFAT, number an entry anew once it leaves their cache. Where the entry
// can't be found, the deepest folder reached is recorded instead.
func (h *fileHelper) resolveExclusion(destination string) {
	parts, err := components(destination)
	if err != nil || len(parts) == 0 {
		return
	}
	current, err := h.t.openPath("/", directoryFlags)
	if err != nil {
		return
	}
	for index, part := range parts {
		info, err := fstat(current)
		if err != nil {
			return
		}
		if index == len(parts)-1 {
			if entry, err := h.t.openAt(current, part, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0); err == nil {
				if entryInfo, err := fstat(entry); err == nil {
					h.excluded.ancestors[identityOf(info)] = true
					h.excluded.entries[identityOf(entryInfo)] = true
					return
				}
			}
		} else if next, err := h.t.openAt(current, part, directoryFlags, 0); err == nil {
			h.excluded.ancestors[identityOf(info)] = true
			current = next
			continue
		}
		h.excluded.unresolved[identityOf(info)] = append(h.excluded.unresolved[identityOf(info)], part)
		return
	}
}

// excludedIn reports an entry of parent that an excluded mount hides from
// the game: one resolved at the start, or one that has since appeared where
// an unresolved destination was missing. Storage that matches names loosely
// may reach that one under another spelling, so it is found by looking the
// missing name up again and comparing identities.
func (h *fileHelper) excludedIn(parent fd, info *unix.Stat_t) bool {
	if info == nil {
		return false
	}
	if h.excluded.entries[identityOf(info)] {
		return true
	}
	if len(h.excluded.unresolved) == 0 {
		return false
	}
	folder, err := fstat(parent)
	if err != nil {
		return true
	}
	for _, name := range h.excluded.unresolved[identityOf(folder)] {
		if appeared, err := lstatAt(parent, name); err == nil && sameInode(appeared, info) {
			return true
		}
	}
	return false
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
	if h.excludedIn(folder, target) || (recursive && (h.excluded.ancestors[identityOf(target)] || h.excluded.unresolved[identityOf(target)] != nil)) {
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
	return !readOnlyOperations[h.request.Operation] && h.excluded.unresolved[identityOf(folder)] != nil
}
