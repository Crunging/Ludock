// Package backups stores, validates, and restores server data archives.
// Backups and restores keep the server stopped, then return it to its
// initial running state.
package backups

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/mountinfo"
	"github.com/Crunging/Ludock/internal/self"
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
	// CopyChosen records an administrator's choice of CopyTo, even none,
	// which a found copy folder never overrides.
	CopyChosen bool `json:"copyChosen,omitempty"`
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

func checkID(id string) error {
	if !helper.ValidID(id) {
		return fail("BACKUP_ID", "Invalid backup identifier.")
	}
	return nil
}

// CopyRoot is approved as a second folder, beside the configured roots,
// whenever a folder is mounted there, so a second copy needs only a volume.
const CopyRoot = "/backups-copy"

// mountedAt reports whether something is mounted exactly at path.
func mountedAt(path string) bool {
	mounts, err := mountinfo.Read()
	return err == nil && slices.ContainsFunc(mounts, func(mount mountinfo.Mount) bool { return mount.Point == path })
}

// FoundCopy is the copy folder Ludock found: CopyRoot when something is
// mounted there, or DevCopyRoot. With no configured roots backups are off,
// so there is none.
func (s *Storage) FoundCopy() string {
	if len(s.Roots) == 0 {
		return ""
	}
	if s.DevCopyRoot != "" {
		return s.DevCopyRoot
	}
	if mountedAt(CopyRoot) {
		return CopyRoot
	}
	return ""
}

// ApprovedRoots are the configured roots, then the found copy folder.
func (s *Storage) ApprovedRoots() []string {
	roots := slices.Clone(s.Roots)
	if found := s.FoundCopy(); found != "" && !slices.Contains(roots, found) {
		roots = append(roots, found)
	}
	return roots
}

// Storage checks destinations against approved roots and Ludock's mounts.
type Storage struct {
	// Roots are approved destinations.
	Roots []string
	Own   *self.Container
	// DevCopyRoot stands in for a folder mounted at CopyRoot when developing
	// against cmd/fakedocker, which can't mount one.
	DevCopyRoot string

	// readers counts open archives by backup ID, and claimed holds archives
	// retention is removing, so it never removes one being read.
	usageMu sync.Mutex
	readers map[string]int
	claimed map[string]bool

	folders folderGuard
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

// readStall is how long one read of an archive may take. It is apart from
// copyStall so that tests shortening a copy's patience don't also fail
// ordinary reads on a slow machine.
var readStall = 2 * time.Minute

// Archive is an open backup archive; retention leaves it alone until it
// is closed. A read blocked in the kernel can't be interrupted, so reads run
// on a goroutine of their own and one that doesn't return within readStall,
// as on a network share that stopped responding, fails with errFolderStalled.
// Read and Close are for one goroutine.
type Archive struct {
	file     *os.File
	once     sync.Once
	done     func()
	requests chan int
	results  chan archiveRead
	exited   chan struct{}
	buffer   []byte
	stalled  bool
}

type archiveRead struct {
	n   int
	err error
}

// readArchive is a seam for a read that never returns.
var readArchive = func(file *os.File, p []byte) (int, error) { return file.Read(p) }

func newArchive(file *os.File, done func()) *Archive {
	a := &Archive{file: file, done: done, requests: make(chan int),
		results: make(chan archiveRead, 1), exited: make(chan struct{})}
	go func() {
		defer close(a.exited)
		defer file.Close()
		for size := range a.requests {
			if len(a.buffer) < size {
				a.buffer = make([]byte, size)
			}
			n, err := readArchive(file, a.buffer[:size])
			a.results <- archiveRead{n, err}
		}
	}()
	return a
}

func (a *Archive) Read(p []byte) (int, error) {
	if a.stalled {
		return 0, errFolderStalled
	}
	if len(p) == 0 {
		return 0, nil
	}
	a.requests <- min(len(p), 1<<20)
	timer := time.NewTimer(readStall)
	defer timer.Stop()
	select {
	case result := <-a.results:
		return copy(p, a.buffer[:result.n]), result.err
	case <-timer.C:
		a.stalled = true
		return 0, errFolderStalled
	}
}

// Seek lets a walk skip bodies it doesn't read. It reads nothing itself.
func (a *Archive) Seek(offset int64, whence int) (int64, error) {
	if a.stalled {
		return 0, errFolderStalled
	}
	return a.file.Seek(offset, whence)
}

// Close leaves a stalled read to close the file whenever it returns.
func (a *Archive) Close() error {
	a.once.Do(func() {
		a.done()
		close(a.requests)
		if !a.stalled {
			<-a.exited
		}
	})
	return nil
}

func NewStorage(roots []string, own *self.Container) *Storage {
	return &Storage{Roots: roots, Own: own}
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
		return "", fail("BACKUP_DESTINATION", "Ludock can’t write to the backup folder. Check its mount and directory permissions, then save again.")
	}
	if !docker.ContainerIDPattern.MatchString(s.Own.Name()) {
		return "", fail("BACKUP_MOUNT_UNVERIFIED", "Ludock couldn’t find its own container. Set LUDOCK_SELF_CONTAINER to its container name or ID, then try again.")
	}
	own, err := s.Own.Read(ctx)
	if errors.Is(err, docker.ErrReadTimeout) {
		return "", errDockerStalled
	}
	if err != nil || own.Answer != self.Found {
		return "", fail("BACKUP_MOUNT_UNVERIFIED", "Ludock couldn’t check its backup mount. Set LUDOCK_SELF_CONTAINER to its container name or ID, then try again.")
	}
	var mount *identity.Mount
	for index := range own.Mounts {
		entry := &own.Mounts[index]
		if helper.Within(directory, entry.Destination) && (mount == nil || len(entry.Destination) > len(mount.Destination)) {
			mount = entry
		}
	}
	if mount == nil || !mount.Writable || (mount.Type != "bind" && mount.Type != "volume") {
		return "", fail("BACKUP_MOUNT_UNVERIFIED", "The backup folder must be a writable folder mounted into Ludock.")
	}
	return directory, nil
}

var errDestination = fail("BACKUP_DESTINATION",
	"The backup folder is missing or can’t be read. Create the folder and check its mount and permissions, then try again.")

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
		return "", fail("BACKUP_DESTINATION", "Choose a mounted backup folder inside LUDOCK_BACKUP_ROOTS (starting with /).")
	}
	configured := s.ApprovedRoots()
	if len(configured) == 0 {
		return "", fail("BACKUP_DESTINATION", "No backup folder is allowed by LUDOCK_BACKUP_ROOTS. Set LUDOCK_BACKUP_ROOTS to a mounted backup folder, recreate Ludock, and save again.")
	}
	var roots []string
	for _, value := range configured {
		// Only roots that can hold the directory are resolved, so an
		// unreachable second folder leaves the others usable.
		if !filepath.IsAbs(value) || filepath.Clean(value) == "/" || !helper.Within(filepath.Clean(directory), filepath.Clean(value)) {
			continue
		}
		canonical, err := filepath.EvalSymlinks(value)
		if err != nil {
			return "", err
		}
		if filepath.Clean(value) != canonical {
			return "", fail("BACKUP_DESTINATION", "Paths in LUDOCK_BACKUP_ROOTS can’t go through symbolic links.")
		}
		roots = append(roots, canonical)
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return "", err
	}
	cleaned := filepath.Clean(directory)
	index := slices.IndexFunc(roots, func(root string) bool { return helper.Within(resolved, root) })
	if index < 0 || !helper.Within(cleaned, roots[index]) {
		return "", fail("BACKUP_DESTINATION", "The backup folder must be an existing mounted folder inside LUDOCK_BACKUP_ROOTS, with no symbolic links in its path.")
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
			return "", fail("BACKUP_DESTINATION", "The backup folder’s path can’t go through a symbolic link.")
		}
	}
	for _, path := range []string{root, directory} {
		if info, err := os.Lstat(path); err != nil || !info.IsDir() {
			return "", fail("BACKUP_DESTINATION", "The backup folder must be a folder, not a file.")
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
		return 0, fail("BACKUP_DISK_UNAVAILABLE", "Ludock couldn’t check how much free space the backup folder has. Check that its disk is mounted, then try again.")
	}
	bytes := helper.FreeBytes(&stat)
	if bytes > 1<<53-1 {
		return 0, fail("BACKUP_DISK_UNAVAILABLE", "Ludock couldn’t check how much free space the backup folder has. Check that its disk is mounted, then try again.")
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

// archivePartial is the file a backup's archive is written to before it is
// published. Only one backup writes an ID, so the name is fixed.
func archivePartial(id string) string { return id + ".tar.partial" }

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
	if err := unix.Unlinkat(dir.fd, archivePartial(id), 0); err != nil && !errors.Is(err, unix.ENOENT) {
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
	return newArchive(file, func() { s.endRead(id) }), nil
}

func (s *Storage) openFile(directory, id string) (*os.File, error) {
	return s.openNamed(directory, id+".tar")
}

func (s *Storage) openNamed(directory, name string) (*os.File, error) {
	dir, err := s.pin(directory)
	if err != nil {
		return nil, err
	}
	defer dir.close()
	fd, err := unix.Openat(dir.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fail("INVALID_BACKUP", "The backup archive is missing or unreadable.")
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, fail("INVALID_BACKUP", "The backup archive isn’t a normal file.")
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
		// As present reports it, so removing the backup from Ludock is offered.
		return errFolderAway
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fail("INVALID_BACKUP", "Ludock won’t delete this backup because its archive isn’t a normal file.")
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

// present reports whether a published archive is in directory, intact as
// far as its size tells, or errFolderAway when directory can't be opened or
// the file can't be checked.
func (s *Storage) present(directory, id string, size int64) (bool, error) {
	if directory == "" || checkID(id) != nil {
		return false, nil
	}
	dir, err := s.pin(directory)
	if err != nil {
		return false, errFolderAway
	}
	defer dir.close()
	var stat unix.Stat_t
	if err := unix.Fstatat(dir.fd, id+".tar", &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, errFolderAway
	}
	return intact(&stat, size), nil
}

// intact is whether stat is an archive Ludock published at size bytes. A
// file of another size was cut short or replaced, and can't be restored.
func intact(stat *unix.Stat_t, size int64) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Size == size
}

// archives lists the intact-looking archives in directory: each regular
// .tar file's size by name.
func (s *Storage) archives(directory string) (map[string]int64, error) {
	dir, err := s.pin(directory)
	if err != nil {
		return nil, err
	}
	defer dir.close()
	listing, err := unix.Dup(dir.fd)
	if err != nil {
		return nil, err
	}
	folder := os.NewFile(uintptr(listing), ".")
	defer folder.Close()
	names, err := folder.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sizes := map[string]int64{}
	for _, name := range names {
		if !strings.HasSuffix(name, ".tar") {
			continue
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(dir.fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
			continue
		} else if err != nil {
			return nil, err
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFREG {
			sizes[name] = stat.Size
		}
	}
	return sizes, nil
}

// reachable is errFolderAway when directory can't be opened.
func (s *Storage) reachable(directory string) error {
	dir, err := s.pin(directory)
	if err != nil {
		return errFolderAway
	}
	dir.close()
	return nil
}
