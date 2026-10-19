package compose

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Crunging/Ludock/internal/apperr"
)

// RootList splits a colon-delimited root setting.
func RootList(value string) []string {
	var roots []string
	for _, root := range strings.Split(value, ":") {
		if root = strings.TrimSpace(root); root != "" && !slices.Contains(roots, root) {
			roots = append(roots, root)
		}
	}
	return roots
}

var errUnsafeRoot = apperr.New("UNSAFE_ROOT", 400, "Use a dedicated absolute data directory as an approved root")

// ConfiguredRoots validates LUDOCK_COMPOSE_ROOTS.
func ConfiguredRoots(value string) ([]string, error) {
	var roots []string
	for _, root := range RootList(value) {
		if !filepath.IsAbs(root) {
			return nil, errUnsafeRoot
		}
		clean := filepath.Clean(root)
		if slices.Contains([]string{"/", "/etc", "/proc", "/sys", "/dev", "/run", "/var/run"}, clean) {
			return nil, errUnsafeRoot
		}
		roots = append(roots, clean)
	}
	return roots, nil
}

// ApprovedPath resolves candidate lexically inside one of roots.
func ApprovedPath(candidate string, roots []string) (string, error) {
	if !filepath.IsAbs(candidate) || strings.Contains(candidate, "\x00") {
		return "", apperr.New("UNAPPROVED_PATH", 400, "An absolute path inside an approved root is required")
	}
	resolved := filepath.Clean(candidate)
	for _, root := range roots {
		if resolved == root || strings.HasPrefix(resolved, root+"/") {
			return resolved, nil
		}
	}
	return "", apperr.New("UNAPPROVED_PATH", 400, "The path is outside the deployment's approved roots")
}

const maxConfigBytes = 2 * 1024 * 1024

var errUnsafeConfig = apperr.New("UNSAFE_CONFIG_PATH", 400, "A configuration file is missing, unreadable, or traverses a symbolic link")

// ReadApprovedFile reads a regular file inside the roots. Every parent is
// pinned by descriptor: O_NOFOLLOW on the final name alone would not stop a
// concurrent parent symlink swap. optional only allows a missing final file.
func ReadApprovedFile(candidate string, roots []string, optional bool) ([]byte, bool, error) {
	resolved, err := ApprovedPath(candidate, roots)
	if err != nil {
		return nil, false, err
	}
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	directory, err := unix.Open("/", flags, 0)
	if err != nil {
		return nil, false, errUnsafeConfig
	}
	defer func() { unix.Close(directory) }()
	parts := strings.Split(strings.TrimPrefix(resolved, "/"), "/")
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(directory, part, flags, 0)
		if err != nil {
			return nil, false, errUnsafeConfig
		}
		unix.Close(directory)
		directory = next
	}
	// O_NONBLOCK lets fstat reject FIFOs before a read waits for a writer.
	fd, err := unix.Openat(directory, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if optional && errors.Is(err, unix.ENOENT) {
			return nil, false, nil
		}
		return nil, false, errUnsafeConfig
	}
	file := os.NewFile(uintptr(fd), "config")
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxConfigBytes {
		return nil, false, apperr.New("INVALID_CONFIG_FILE", 400, "Configuration inputs must be regular files within the size limit")
	}
	data := make([]byte, maxConfigBytes+1)
	total := 0
	for total < len(data) {
		n, err := file.Read(data[total:])
		total += n
		if err != nil || n == 0 {
			break
		}
	}
	if total > maxConfigBytes {
		return nil, false, apperr.New("INVALID_CONFIG_FILE", 400, "Configuration input exceeds the size limit")
	}
	return data[:total], true, nil
}
