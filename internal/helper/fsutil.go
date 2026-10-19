// Package helper implements the `ludock helper` subcommands that run inside
// disposable helper containers, speaking JSON request and output
// protocols with the backend. Every path is walked one
// component at a time through pinned directory descriptors that never follow
// symbolic links, and failures never print paths or raw filesystem errors.
package helper

import (
	"errors"
	"os"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

var errUnsafe = errors.New("unsafe path")

const (
	directoryFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fileFlags      = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
)

// fd is a pinned descriptor.
type fd int

// tracker closes every descriptor still open when a helper exits.
type tracker struct{ open []fd }

func (t *tracker) add(descriptor fd) fd {
	t.open = append(t.open, descriptor)
	return descriptor
}

func (t *tracker) close(descriptor fd) {
	if index := slices.Index(t.open, descriptor); index >= 0 {
		t.open = slices.Delete(t.open, index, index+1)
	}
	unix.Close(int(descriptor))
}

func (t *tracker) closeAll() {
	for _, descriptor := range t.open {
		unix.Close(int(descriptor))
	}
	t.open = nil
}

func (t *tracker) openAt(parent fd, name string, flags int, mode uint32) (fd, error) {
	if name == "" || strings.Contains(name, "/") {
		return -1, errUnsafe
	}
	descriptor, err := unix.Openat(int(parent), name, flags, mode)
	if err != nil {
		return -1, err
	}
	return t.add(fd(descriptor)), nil
}

func (t *tracker) openPath(path string, flags int) (fd, error) {
	descriptor, err := unix.Open(path, flags, 0)
	if err != nil {
		return -1, err
	}
	return t.add(fd(descriptor)), nil
}

// walk pins each directory of an absolute path from start, without
// following symbolic links.
func (t *tracker) walk(start string, parts []string) (fd, error) {
	current, err := t.openPath(start, directoryFlags)
	if err != nil {
		return -1, err
	}
	for _, part := range parts {
		next, err := t.openAt(current, part, directoryFlags, 0)
		t.close(current)
		if err != nil {
			return -1, err
		}
		current = next
	}
	return current, nil
}

func fstat(descriptor fd) (*unix.Stat_t, error) {
	var info unix.Stat_t
	return &info, unix.Fstat(int(descriptor), &info)
}

func lstatAt(parent fd, name string) (*unix.Stat_t, error) {
	var info unix.Stat_t
	return &info, unix.Fstatat(int(parent), name, &info, unix.AT_SYMLINK_NOFOLLOW)
}

func isDir(info *unix.Stat_t) bool     { return info.Mode&unix.S_IFMT == unix.S_IFDIR }
func isRegular(info *unix.Stat_t) bool { return info.Mode&unix.S_IFMT == unix.S_IFREG }
func isSymlink(info *unix.Stat_t) bool { return info.Mode&unix.S_IFMT == unix.S_IFLNK }

func sameInode(a, b *unix.Stat_t) bool { return a.Dev == b.Dev && a.Ino == b.Ino }

// names lists a pinned directory without consuming its offset.
func names(directory fd) ([]string, error) {
	duplicate, err := unix.Dup(int(directory))
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(duplicate), "directory")
	defer file.Close()
	if _, err := file.Seek(0, 0); err != nil {
		return nil, err
	}
	return file.Readdirnames(-1)
}

func mtimeMs(info *unix.Stat_t) float64 {
	return float64(info.Mtim.Sec)*1000 + float64(info.Mtim.Nsec)/1e6
}

func ctimeMs(info *unix.Stat_t) float64 {
	return float64(info.Ctim.Sec)*1000 + float64(info.Ctim.Nsec)/1e6
}

func isNotExist(err error) bool { return errors.Is(err, unix.ENOENT) }
