package helper

import (
	"errors"
	"strings"

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
	// deepest folder found on the way: nothing is written into it, since an
	// entry made there could become the destination.
	unresolved map[identity]bool
}

type identity struct{ device, inode uint64 }

func identityOf(info *unix.Stat_t) identity { return identity{uint64(info.Dev), info.Ino} }

var errExcluded = errors.New("this path overlaps an excluded mount")

// exclusionsResolved runs once the exclusions are resolved; tests use it to
// move an excluded entry to another spelling.
var exclusionsResolved = func() {}

// readOnlyOperations are the file operations that write nothing.
var readOnlyOperations = map[string]bool{"list": true, "stat": true, "download": true, "download-paths": true, "check": true,
	"space": true, "mounts": true, "backup": true}

// resolveExclusions walks each excluded destination from "/" without
// following links, then refuses any later open of one of its entries.
func (h *fileHelper) resolveExclusions() {
	if len(h.request.Blocked) == 0 {
		return
	}
	h.excluded = exclusions{entries: map[identity]bool{}, ancestors: map[identity]bool{}, unresolved: map[identity]bool{}}
	for _, destination := range h.request.Blocked {
		h.resolveExclusion(destination)
	}
	h.t.refuse = func(info *unix.Stat_t) bool { return h.excluded.entries[identityOf(info)] }
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
		h.excluded.unresolved[identityOf(info)] = true
		return
	}
}

// isExcluded reports an entry an excluded mount hides from the game.
func (h *fileHelper) isExcluded(info *unix.Stat_t) bool {
	return info != nil && h.excluded.entries[identityOf(info)]
}

// checkIdentity refuses relative, below the root, when it is an excluded
// entry or, for a change to it as a whole (recursive), holds one; and a write
// below the parent folder of an exclusion that couldn't be resolved. Folders
// on the way are refused as they open.
func (h *fileHelper) checkIdentity(relative string, recursive bool) (err error) {
	if len(h.request.Blocked) == 0 {
		return nil
	}
	parts, err := ownParts(relative)
	if err != nil {
		return err
	}
	opened := len(h.t.open)
	defer h.t.closeFrom(opened)
	folder, err := h.pinRoot()
	if err != nil {
		return err
	}
	var entry *unix.Stat_t
	if len(parts) == 0 {
		if entry, err = fstat(folder); err != nil {
			return err
		}
	} else {
		defer func() {
			if parent, statErr := fstat(folder); err == nil && statErr == nil && h.writesInto(parent) {
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
		if entry, err = lstatAt(folder, parts[len(parts)-1]); err != nil {
			return nil
		}
	}
	if h.isExcluded(entry) || (recursive && (h.excluded.ancestors[identityOf(entry)] || h.excluded.unresolved[identityOf(entry)])) {
		return errExcluded
	}
	return nil
}

// writesInto reports a write into a folder an unresolved exclusion stopped
// at.
func (h *fileHelper) writesInto(folder *unix.Stat_t) bool {
	return !readOnlyOperations[h.request.Operation] && h.excluded.unresolved[identityOf(folder)]
}

// checkInto refuses putting entries into folder, below the root, as a move
// does into its destination.
func (h *fileHelper) checkInto(folder string) error {
	if len(h.request.Blocked) == 0 {
		return nil
	}
	parts, err := ownParts(folder)
	if err != nil {
		return err
	}
	opened := len(h.t.open)
	defer h.t.closeFrom(opened)
	root, err := h.pinRoot()
	if err != nil {
		return err
	}
	directory, err := h.pinDirectory(root, strings.Join(parts, "/"))
	if err != nil {
		return err
	}
	info, err := fstat(directory)
	if err != nil {
		return err
	}
	if h.writesInto(info) {
		return errExcluded
	}
	return nil
}
