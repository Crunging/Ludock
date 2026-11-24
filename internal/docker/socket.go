package docker

import (
	"os"
	"path/filepath"
	"strings"
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
	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	return err == nil && mountDeleted(string(mountinfo), resolved)
}

// mountDeleted reports whether the mount at target, in /proc/self/mountinfo
// form, has a source the kernel marks deleted.
func mountDeleted(mountinfo, target string) bool {
	deleted := false
	for line := range strings.Lines(mountinfo) {
		fields := strings.Fields(line)
		// Later mounts at the same point hide earlier ones.
		if len(fields) >= 5 && fields[4] == target {
			deleted = strings.HasSuffix(fields[3], "//deleted")
		}
	}
	return deleted
}
