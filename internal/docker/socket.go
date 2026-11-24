package docker

import (
	"path/filepath"
	"strings"

	"github.com/Crunging/Ludock/internal/mountinfo"
)

// SocketReplaced reports whether the Docker socket at path is a bind mount of
// a file Docker has since deleted. A daemon that creates its own socket, not
// systemd's, makes a new one each time it starts, and a container that keeps
// running across that, as with live restore, keeps the old file, which
// nothing answers on again.
func SocketReplaced(path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	mounts, err := mountinfo.Read()
	return err == nil && mountDeleted(mounts, resolved)
}

// mountDeleted reports whether the mount at target has a source the kernel
// marks deleted.
func mountDeleted(mounts []mountinfo.Mount, target string) bool {
	deleted := false
	for _, mount := range mounts {
		// Later mounts at the same point hide earlier ones.
		if mount.Point == target {
			deleted = strings.HasSuffix(mount.Root, "//deleted")
		}
	}
	return deleted
}
