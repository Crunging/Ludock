// Package backups stores, validates, and restores server data archives.
// Backups and restores keep the server stopped, then return it to its
// initial running state.
package backups

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/textutil"
)

type Settings struct {
	Destination    string `json:"destination"`
	RetentionCount int    `json:"retentionCount"`
	MaxBytes       int64  `json:"maxBytes"`
	ReserveBytes   int64  `json:"reserveBytes"`
}

const (
	DefaultRetention = 10
	DefaultMaxBytes  = 100 << 30
	DefaultReserve   = 5 << 30
)

type Root struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

func fail(code, message string) *apperr.Error { return apperr.New(code, 409, message) }

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func checkID(id string) error {
	if !uuidPattern.MatchString(id) {
		return fail("BACKUP_ID", "Invalid backup identifier.")
	}
	return nil
}

func within(value, root string) bool { return value == root || strings.HasPrefix(value, root+"/") }

// RootList splits a colon-delimited root setting such as
// LUDOCK_BACKUP_ROOTS, dropping blanks and duplicates.
func RootList(value string) []string {
	var roots []string
	for _, root := range strings.Split(value, ":") {
		if root = strings.TrimSpace(root); root != "" && !slices.Contains(roots, root) {
			roots = append(roots, root)
		}
	}
	return roots
}

// Storage checks destinations against approved roots and Ludock's mounts.
type Storage struct {
	Docker *docker.Client
	// Roots are approved destinations, ':'-separated; Self names Ludock's
	// own container.
	Roots, Self string
}

func NewStorage(client *docker.Client, roots []string, self string) *Storage {
	return &Storage{Docker: client, Roots: strings.Join(roots, ":"), Self: self}
}

// ValidateSettings checks limits, the destination, and that it is a
// writable mount of Ludock's own container.
func (s *Storage) ValidateSettings(ctx context.Context, settings Settings) (Settings, error) {
	if settings.Destination == "" || textutil.Length(settings.Destination) > 1024 || settings.RetentionCount < 1 ||
		settings.RetentionCount > 1000 || settings.MaxBytes <= 0 || settings.MaxBytes > 1<<53-1 ||
		settings.ReserveBytes < 0 || settings.ReserveBytes > 1<<53-1 {
		return settings, apperr.New("INVALID_REQUEST", 400, "Invalid request")
	}
	directory, err := s.ApprovedDirectory(settings.Destination)
	if err != nil {
		return settings, err
	}
	if unix.Access(directory, unix.W_OK|unix.X_OK) != nil {
		return settings, fail("BACKUP_DESTINATION", "Ludock cannot write to the backup folder. Check its mount and directory permissions, then save again.")
	}
	if !docker.ContainerIDPattern.MatchString(s.Self) {
		return settings, fail("BACKUP_MOUNT_UNVERIFIED", "Identify Ludock's container with LUDOCK_SELF_CONTAINER before configuring backups.")
	}
	self, err := s.Docker.Inspect(ctx, s.Self)
	if err != nil {
		return settings, fail("BACKUP_MOUNT_UNVERIFIED", "Ludock could not verify its backup mount. Set LUDOCK_SELF_CONTAINER to its Docker container name or ID.")
	}
	var mount *docker.MountPoint
	for index := range self.Mounts {
		entry := &self.Mounts[index]
		if within(directory, entry.Destination) && (mount == nil || len(entry.Destination) > len(mount.Destination)) {
			mount = entry
		}
	}
	if mount == nil || !mount.RW || (mount.Type != "bind" && mount.Type != "volume") {
		return settings, fail("BACKUP_MOUNT_UNVERIFIED", "The backup destination must be a writable mounted directory in Ludock's container.")
	}
	return settings, nil
}

var errDestination = fail("BACKUP_DESTINATION",
	"The backup folder or an approved backup root is missing or unreadable. Create the folder and check its mount and permissions, then try again.")

// ApprovedDirectory resolves a destination inside the approved roots
// without symbolic-link traversal.
func (s *Storage) ApprovedDirectory(directory string) (string, error) {
	resolved, err := s.resolveDirectory(directory)
	if err != nil {
		if _, ok := apperr.As(err); ok {
			return "", err
		}
		return "", errDestination
	}
	return resolved, nil
}

func (s *Storage) resolveDirectory(directory string) (string, error) {
	if !filepath.IsAbs(directory) || strings.Contains(directory, "\x00") {
		return "", fail("BACKUP_DESTINATION", "Choose an absolute mounted backup destination inside LUDOCK_BACKUP_ROOTS.")
	}
	configured := RootList(s.Roots)
	if len(configured) == 0 {
		return "", fail("BACKUP_DESTINATION", "No backup roots are configured. Set LUDOCK_BACKUP_ROOTS to a mounted backup folder and recreate Ludock, then save again.")
	}
	var roots []string
	for _, value := range configured {
		if !filepath.IsAbs(value) || filepath.Clean(value) == "/" {
			continue
		}
		canonical, err := filepath.EvalSymlinks(value)
		if err != nil {
			return "", err
		}
		if filepath.Clean(value) != canonical {
			return "", fail("BACKUP_DESTINATION", "Configured backup roots must not traverse symbolic links.")
		}
		roots = append(roots, canonical)
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return "", err
	}
	cleaned := filepath.Clean(directory)
	index := slices.IndexFunc(roots, func(root string) bool { return within(resolved, root) })
	if index < 0 || !within(cleaned, roots[index]) {
		return "", fail("BACKUP_DESTINATION", "Backup destination must be an existing mounted directory inside LUDOCK_BACKUP_ROOTS, without symlink traversal.")
	}
	root := roots[index]
	current := root
	relative, _ := filepath.Rel(root, cleaned)
	for _, part := range strings.Split(relative, "/") {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fail("BACKUP_DESTINATION", "Backup destination cannot traverse symbolic links.")
		}
	}
	for _, path := range []string{root, directory} {
		if info, err := os.Lstat(path); err != nil || !info.IsDir() {
			return "", fail("BACKUP_DESTINATION", "Backup destination must be a directory.")
		}
	}
	return resolved, nil
}

// pinned is a destination directory held open by descriptor.
type pinned struct{ fd int }

func (p pinned) close() { unix.Close(p.fd) }

func (s *Storage) pin(directory string) (pinned, error) {
	approved, err := s.ApprovedDirectory(directory)
	if err != nil {
		return pinned{}, err
	}
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Open("/", flags, 0)
	if err != nil {
		return pinned{}, errDestination
	}
	for _, component := range strings.Split(approved, "/") {
		if component == "" {
			continue
		}
		next, err := unix.Openat(fd, component, flags, 0)
		unix.Close(fd)
		if err != nil {
			return pinned{}, errDestination
		}
		fd = next
	}
	return pinned{fd}, nil
}

func budget(directory pinned, reserve, additional int64) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(directory.fd, &stat); err != nil {
		return 0, err
	}
	available := helper.FreeBytes(&stat) - reserve
	if available < additional {
		return 0, fail("BACKUP_CAPACITY", "The backup destination does not have enough free space above its configured reserve.")
	}
	return available, nil
}

// AvailableBytes measures free space in the destination through the same
// pinned directory used by archive writes, without creating a probe file.
func (s *Storage) AvailableBytes(directory string) (int64, error) {
	dir, err := s.pin(directory)
	if err != nil {
		return 0, err
	}
	defer dir.close()
	var stat unix.Statfs_t
	if unix.Faccessat(dir.fd, ".", unix.W_OK|unix.X_OK, 0) != nil || unix.Fstatfs(dir.fd, &stat) != nil {
		return 0, fail("BACKUP_DISK_UNAVAILABLE", "Available disk space could not be measured for the backup destination.")
	}
	bytes := helper.FreeBytes(&stat)
	if bytes > 1<<53-1 {
		return 0, fail("BACKUP_DISK_UNAVAILABLE", "Available disk space could not be measured for the backup destination.")
	}
	return bytes, nil
}

// AssertSpace requires additional bytes above the reserve.
func (s *Storage) AssertSpace(directory string, reserve, additional int64) error {
	dir, err := s.pin(directory)
	if err != nil {
		return err
	}
	defer dir.close()
	_, err = budget(dir, reserve, additional)
	return err
}

// RemovePartial removes an unpublished archive.
func (s *Storage) RemovePartial(directory, id string) error {
	if err := checkID(id); err != nil {
		return err
	}
	dir, err := s.pin(directory)
	if err != nil {
		return err
	}
	defer dir.close()
	if err := unix.Unlinkat(dir.fd, id+".tar.partial", 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

func (s *Storage) Open(directory, id string) (*os.File, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	dir, err := s.pin(directory)
	if err != nil {
		return nil, err
	}
	defer dir.close()
	fd, err := unix.Openat(dir.fd, id+".tar", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fail("INVALID_BACKUP", "The backup archive is missing or unreadable.")
	}
	file := os.NewFile(uintptr(fd), id+".tar")
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, fail("INVALID_BACKUP", "The backup archive is not a regular file.")
	}
	return file, nil
}

// Remove deletes a published archive. Only a regular file is removed.
func (s *Storage) Remove(directory, id string, assertAccess func() error) error {
	if err := checkID(id); err != nil {
		return err
	}
	dir, err := s.pin(directory)
	if err != nil {
		return err
	}
	defer dir.close()
	var stat unix.Stat_t
	err = unix.Fstatat(dir.fd, id+".tar", &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fail("INVALID_BACKUP", "Refusing to remove an archive that is not a regular file.")
	}
	if assertAccess != nil {
		if err := assertAccess(); err != nil {
			return err
		}
	}
	if err := unix.Unlinkat(dir.fd, id+".tar", 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return unix.Fsync(dir.fd)
}
