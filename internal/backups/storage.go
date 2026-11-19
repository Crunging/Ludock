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
	"sync"

	"golang.org/x/sys/unix"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/textutil"
)

type Settings struct {
	Destination string `json:"destination"`
	// MaxBytes caps all servers' archives together; 0 means no cap, so only
	// the free-space reserve limits them.
	MaxBytes     int64 `json:"maxBytes"`
	ReserveBytes int64 `json:"reserveBytes"`
	// CopyTo is another approved folder that gets a second copy of every
	// backup; empty means none.
	CopyTo string `json:"copyTo,omitempty"`
}

const (
	DefaultReserve = 5 << 30
	maxSetting     = 1<<53 - 1
)

// valid checks the limits; Storage.ValidateSettings also checks the folder.
func (s Settings) valid() bool {
	return s.Destination != "" && textutil.Length(s.Destination) <= 1024 && textutil.Length(s.CopyTo) <= 1024 && s.MaxBytes >= 0 && s.MaxBytes <= maxSetting &&
		s.ReserveBytes >= 0 && s.ReserveBytes <= maxSetting
}

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

// CopyRoot is approved as a second folder, beside the configured roots,
// whenever a folder is mounted there, so a second copy needs only a volume.
const CopyRoot = "/backups-copy"

// mountInfo lists the mounts of Ludock's own container; tests replace it.
var mountInfo = "/proc/self/mountinfo"

// mountedAt reports whether something is mounted exactly at path.
func mountedAt(path string) bool {
	data, err := os.ReadFile(mountInfo)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) > 4 && fields[4] == path {
			return true
		}
	}
	return false
}

// ApprovedRoots are the configured roots, then CopyRoot when it is mounted.
// With no configured roots backups are off, so CopyRoot isn't offered.
func (s *Storage) ApprovedRoots() []string {
	roots := RootList(s.Roots)
	if len(roots) > 0 && !slices.Contains(roots, CopyRoot) && mountedAt(CopyRoot) {
		roots = append(roots, CopyRoot)
	}
	return roots
}

// Storage checks destinations against approved roots and Ludock's mounts.
type Storage struct {
	Docker *docker.Client
	// Roots are approved destinations, ':'-separated; Self names Ludock's
	// own container.
	Roots, Self string

	// readers counts open archives by backup ID, and claimed holds archives
	// retention is removing, so it never removes one being read.
	usageMu sync.Mutex
	readers map[string]int
	claimed map[string]bool
}

// claimIdle reserves an archive no one is reading for removal; release
// ends the claim.
func (s *Storage) claimIdle(id string) (release func(), ok bool) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if s.readers[id] > 0 || s.claimed[id] {
		return nil, false
	}
	if s.claimed == nil {
		s.claimed = map[string]bool{}
	}
	s.claimed[id] = true
	return func() {
		s.usageMu.Lock()
		delete(s.claimed, id)
		s.usageMu.Unlock()
	}, true
}

// Reading reports whether an archive is open for reading.
func (s *Storage) Reading(id string) bool {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	return s.readers[id] > 0
}

func (s *Storage) startRead(id string) bool {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if s.claimed[id] {
		return false
	}
	if s.readers == nil {
		s.readers = map[string]int{}
	}
	s.readers[id]++
	return true
}

func (s *Storage) endRead(id string) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if s.readers[id]--; s.readers[id] <= 0 {
		delete(s.readers, id)
	}
}

// Archive is an open backup archive; retention leaves it alone until it
// is closed.
type Archive struct {
	*os.File
	once sync.Once
	done func()
}

func (a *Archive) Close() error {
	a.once.Do(a.done)
	return a.File.Close()
}

func NewStorage(client *docker.Client, roots []string, self string) *Storage {
	return &Storage{Docker: client, Roots: strings.Join(roots, ":"), Self: self}
}

// ValidateSettings checks limits, the destination, and that it is a
// writable mount of Ludock's own container. The second copy's folder is
// checked only when saving (ValidateCopyFolder), so a missing one never
// stops a backup.
func (s *Storage) ValidateSettings(ctx context.Context, settings Settings) (Settings, error) {
	if !settings.valid() {
		return settings, apperr.New("INVALID_REQUEST", 400, "Invalid request")
	}
	if _, err := s.validateFolder(ctx, settings.Destination); err != nil {
		return settings, err
	}
	return settings, nil
}

// validateFolder resolves an approved folder that is a writable mount of
// Ludock's own container.
func (s *Storage) validateFolder(ctx context.Context, folder string) (string, error) {
	directory, err := s.ApprovedDirectory(folder)
	if err != nil {
		return "", err
	}
	if unix.Access(directory, unix.W_OK|unix.X_OK) != nil {
		return "", fail("BACKUP_DESTINATION", "Ludock cannot write to the backup folder. Check its mount and directory permissions, then save again.")
	}
	if !docker.ContainerIDPattern.MatchString(s.Self) {
		return "", fail("BACKUP_MOUNT_UNVERIFIED", "Identify Ludock's container with LUDOCK_SELF_CONTAINER before configuring backups.")
	}
	self, err := s.Docker.Inspect(ctx, s.Self)
	if err != nil {
		return "", fail("BACKUP_MOUNT_UNVERIFIED", "Ludock could not verify its backup mount. Set LUDOCK_SELF_CONTAINER to its Docker container name or ID.")
	}
	var mount *docker.MountPoint
	for index := range self.Mounts {
		entry := &self.Mounts[index]
		if within(directory, entry.Destination) && (mount == nil || len(entry.Destination) > len(mount.Destination)) {
			mount = entry
		}
	}
	if mount == nil || !mount.RW || (mount.Type != "bind" && mount.Type != "volume") {
		return "", fail("BACKUP_MOUNT_UNVERIFIED", "The backup destination must be a writable mounted directory in Ludock's container.")
	}
	return directory, nil
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
	configured := s.ApprovedRoots()
	if len(configured) == 0 {
		return "", fail("BACKUP_DESTINATION", "No backup roots are configured. Set LUDOCK_BACKUP_ROOTS to a mounted backup folder and recreate Ludock, then save again.")
	}
	var roots []string
	for _, value := range configured {
		// Only roots that can hold the directory are resolved, so an
		// unreachable second folder leaves the others usable.
		if !filepath.IsAbs(value) || filepath.Clean(value) == "/" || !within(filepath.Clean(directory), filepath.Clean(value)) {
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
		return 0, CapacityError(codeDisk, 0)
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

func (s *Storage) Open(directory, id string) (*Archive, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	missing := fail("INVALID_BACKUP", "The backup archive is missing or unreadable.")
	if !s.startRead(id) {
		return nil, missing
	}
	file, err := s.openFile(directory, id)
	if err != nil {
		s.endRead(id)
		if _, ok := apperr.As(err); !ok {
			err = missing
		}
		return nil, err
	}
	return &Archive{File: file, done: func() { s.endRead(id) }}, nil
}

func (s *Storage) openFile(directory, id string) (*os.File, error) {
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
	return s.removeFile(directory, id+".tar", assertAccess)
}

func (s *Storage) removeFile(directory, name string, assertAccess func() error) error {
	dir, err := s.pin(directory)
	if err != nil {
		return err
	}
	defer dir.close()
	var stat unix.Stat_t
	err = unix.Fstatat(dir.fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
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
	if err := unix.Unlinkat(dir.fd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return unix.Fsync(dir.fd)
}

// present reports whether a published archive is a regular file in directory.
func (s *Storage) present(directory, id string) bool {
	if directory == "" || checkID(id) != nil {
		return false
	}
	dir, err := s.pin(directory)
	if err != nil {
		return false
	}
	defer dir.close()
	var stat unix.Stat_t
	return unix.Fstatat(dir.fd, id+".tar", &stat, unix.AT_SYMLINK_NOFOLLOW) == nil && stat.Mode&unix.S_IFMT == unix.S_IFREG
}
