package helper

import (
	"errors"
	"slices"
	"strings"

	"github.com/Crunging/Ludock/internal/tarstream"
	"golang.org/x/sys/unix"
)

type selected struct {
	name, relative string
	info           *unix.Stat_t
}

// selection deletes, moves, or archives Paths, names of entries in the
// folder Path. Every entry is checked before any is changed.
func (h *fileHelper) selection(root fd, operation string) error {
	if len(h.request.Paths) == 0 || len(h.request.Paths) > 1000 {
		return errUnsafe
	}
	folder, err := components(h.request.Path)
	if err != nil {
		return err
	}
	var items []selected
	for _, name := range h.request.Paths {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
			return errUnsafe
		}
		relative := strings.Join(append(slices.Clone(folder), name), "/")
		if slices.ContainsFunc(items, func(item selected) bool { return item.name == name }) {
			return errUnsafe
		}
		if err := h.check(relative, true); err != nil {
			return err
		}
		items = append(items, selected{name: name, relative: relative})
	}
	parent, err := h.pinDirectory(root, strings.Join(folder, "/"))
	if err != nil {
		return err
	}
	for index := range items {
		if items[index].info, err = lstatAt(parent, items[index].name); err != nil {
			return err
		}
	}
	switch operation {
	case "delete-paths":
		for _, item := range items {
			if err := h.removeEntry(parent, item.name, 0); err != nil {
				return err
			}
		}
		return h.json(map[string]bool{"ok": true})
	case "download-paths":
		for _, item := range items {
			if !isDir(item.info) && (!isRegular(item.info) || item.info.Nlink != 1) {
				return errors.New("only regular files and directories can be downloaded")
			}
		}
		if err := h.json(map[string]any{"type": "directory", "size": 0}); err != nil {
			return err
		}
		if _, err := h.out.Write([]byte{'\n'}); err != nil {
			return err
		}
		for _, item := range items {
			if err := h.archiveEntry(parent, item.name, item.name, 0); err != nil {
				return err
			}
		}
		_, err := h.out.Write(tarstream.End())
		return err
	}
	return h.move(root, parent, items)
}

// mountOf is the deepest nested mount holding value, or "" for the root's.
func (h *fileHelper) mountOf(value string) string {
	owner := ""
	for _, mount := range h.request.Mounts {
		if within(value, mount) && len(mount) > len(owner) {
			owner = mount
		}
	}
	return owner
}

// move renames each entry into Destination, a folder of the same mount,
// never replacing a name there and never moving a link or a mount.
func (h *fileHelper) move(root, parent fd, items []selected) error {
	parts, err := components(h.request.Destination)
	if err != nil {
		return err
	}
	destination := strings.Join(parts, "/")
	if err := h.check(destination, false); err != nil {
		return err
	}
	target, err := h.pinDirectory(root, destination)
	if err != nil {
		return err
	}
	targetInfo, err := fstat(target)
	if err != nil {
		return err
	}
	for _, item := range items {
		absolute := h.absolute(item.relative)
		if isSymlink(item.info) || within(destination, item.relative) || item.info.Dev != targetInfo.Dev ||
			h.mountOf(absolute) != h.mountOf(h.absolute(destination)) ||
			slices.ContainsFunc(h.request.Mounts, func(mount string) bool { return within(mount, absolute) }) {
			return errUnsafe
		}
		if _, err := lstatAt(target, item.name); err == nil {
			return errNameTaken
		} else if !isNotExist(err) {
			return err
		}
	}
	for _, item := range items {
		if err := renameNoReplace(parent, item.name, target, item.name, item.info); err != nil {
			return nameTaken(err)
		}
	}
	return h.json(map[string]bool{"ok": true})
}
