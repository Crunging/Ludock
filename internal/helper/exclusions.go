package helper

import (
	"errors"
	"path"

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
	// unresolved are destinations whose entry couldn't be found, below whose
	// parent folder nothing is written.
	unresolved []string
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
	h.excluded = exclusions{entries: map[identity]bool{}, ancestors: map[identity]bool{}}
	for _, destination := range h.request.Blocked {
		if err := h.resolveExclusion(destination); err != nil {
			h.excluded.unresolved = append(h.excluded.unresolved, destination)
		}
	}
	h.t.refuse = func(info *unix.Stat_t) bool { return h.excluded.entries[identityOf(info)] }
	exclusionsResolved()
}

func (h *fileHelper) resolveExclusion(destination string) error {
	parts, err := components(destination)
	if err != nil || len(parts) == 0 {
		return errUnsafe
	}
	opened := len(h.t.open)
	defer h.t.closeFrom(opened)
	current, err := h.t.openPath("/", directoryFlags)
	if err != nil {
		return err
	}
	var ancestors []identity
	for _, part := range parts[:len(parts)-1] {
		info, err := fstat(current)
		if err != nil {
			return err
		}
		ancestors = append(ancestors, identityOf(info))
		if current, err = h.t.openAt(current, part, directoryFlags, 0); err != nil {
			return err
		}
	}
	info, err := fstat(current)
	if err != nil {
		return err
	}
	entry, err := lstatAt(current, parts[len(parts)-1])
	if err != nil {
		return err
	}
	for _, ancestor := range append(ancestors, identityOf(info)) {
		h.excluded.ancestors[ancestor] = true
	}
	h.excluded.entries[identityOf(entry)] = true
	return nil
}

// isExcluded reports an entry an excluded mount hides from the game.
func (h *fileHelper) isExcluded(info *unix.Stat_t) bool {
	return info != nil && h.excluded.entries[identityOf(info)]
}

// checkIdentity refuses relative, below the root, when it is an excluded
// entry or, for a change to it as a whole (recursive), holds one; and a write
// below the parent folder of an exclusion that couldn't be resolved. Folders
// on the way are refused as they open.
func (h *fileHelper) checkIdentity(relative string, recursive bool) error {
	if len(h.request.Blocked) == 0 {
		return nil
	}
	if !readOnlyOperations[h.request.Operation] {
		for _, destination := range h.excluded.unresolved {
			if Within(h.absolute(relative), path.Dir(destination)) {
				return errExcluded
			}
		}
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
	if h.isExcluded(entry) || (recursive && h.excluded.ancestors[identityOf(entry)]) {
		return errExcluded
	}
	return nil
}
