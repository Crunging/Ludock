// Package backups stores, validates, and restores server data archives.
// Backups and restores keep the server stopped, then return it to its
// initial running state.
package backups

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/jscompat"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/tarstream"
)

// Settings is the backup configuration.
type Settings struct {
	Destination    string `json:"destination"`
	RetentionCount int    `json:"retentionCount"`
	MaxBytes       int64  `json:"maxBytes"`
	ReserveBytes   int64  `json:"reserveBytes"`
}

// Default limits for new settings.
const (
	DefaultRetention = 10
	DefaultMaxBytes  = 100 << 30
	DefaultReserve   = 5 << 30
)

// Root is a recorded data root.
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
	// Roots is LUDOCK_BACKUP_ROOTS; Self names Ludock's own container.
	Roots, Self string
}

// NewStorage reads the environment.
func NewStorage(client *docker.Client) *Storage {
	self := os.Getenv("LUDOCK_SELF_CONTAINER")
	if self == "" {
		self = os.Getenv("HOSTNAME")
	}
	return &Storage{Docker: client, Roots: os.Getenv("LUDOCK_BACKUP_ROOTS"), Self: self}
}

// ValidateSettings checks limits, the destination, and that it is a
// writable mount of Ludock's own container.
func (s *Storage) ValidateSettings(ctx context.Context, settings Settings) (Settings, error) {
	if settings.Destination == "" || jscompat.Length(settings.Destination) > 1024 || settings.RetentionCount < 1 ||
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
	available := int64(stat.Bavail)*int64(stat.Bsize) - reserve
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
	bytes := uint64(stat.Bavail) * uint64(stat.Bsize)
	if bytes > 1<<53-1 {
		return 0, fail("BACKUP_DISK_UNAVAILABLE", "Available disk space could not be measured for the backup destination.")
	}
	return int64(bytes), nil
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

// Open opens a published archive for reading.
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

// EntryMetadata returns validated ownership and modification time.
func EntryMetadata(header *tarstream.Header) (uid, gid int64, mtime float64, err error) {
	uid, gid, mtime = header.UID, header.GID, header.MtimeSeconds
	if header.PAX != nil {
		var parseErr error
		parse := func(key string, target *float64) {
			if value, ok := header.PAX[key]; ok {
				if *target, parseErr = parseNumber(value); parseErr != nil {
					err = parseErr
				}
			}
		}
		u, g, m := float64(uid), float64(gid), mtime
		parse("uid", &u)
		parse("gid", &g)
		parse("mtime", &m)
		if u != math.Trunc(u) || g != math.Trunc(g) {
			err = errors.New("fractional owner")
		}
		uid, gid, mtime = int64(u), int64(g), m
	}
	if err != nil || uid < 0 || uid > 4294967294 || gid < 0 || gid > 4294967294 || math.IsNaN(mtime) || math.Abs(mtime) > 8.64e12 {
		return 0, 0, 0, fail("UNSAFE_ARCHIVE", "The archive contains invalid ownership or timestamp metadata.")
	}
	return uid, gid, mtime, nil
}

func parseNumber(value string) (float64, error) {
	var number float64
	if err := json.Unmarshal([]byte(strings.TrimSpace(value)), &number); err != nil {
		return 0, err
	}
	return number, nil
}

// MappedHeader renames an entry with fresh, numeric-only PAX metadata. This
// keeps large Linux IDs and dates past 2038 without retaining source path or
// link overrides.
func MappedHeader(header *tarstream.Header, name string) (*tarstream.Header, error) {
	uid, gid, mtime, err := EntryMetadata(header)
	if err != nil {
		return nil, err
	}
	return &tarstream.Header{Name: name, Type: header.Type, Size: header.Size, Mode: header.Mode, UID: uid, GID: gid,
		MtimeSeconds: mtime, PAX: map[string]string{"uid": tarstream.FormatSeconds(float64(uid)),
			"gid": tarstream.FormatSeconds(float64(gid)), "mtime": tarstream.FormatSeconds(mtime)}}, nil
}

// ValidateEntry accepts only regular files and directories inside the
// recorded roots.
func ValidateEntry(header *tarstream.Header, roots []Root) error {
	name := strings.TrimSuffix(header.Name, "/")
	parts := strings.Split(name, "/")
	if _, _, _, err := EntryMetadata(header); err != nil {
		return err
	}
	if strings.Contains(header.Name, "\x00") || strings.Contains(header.Name, "\\") || strings.HasPrefix(header.Name, "/") ||
		slices.ContainsFunc(parts, func(part string) bool { return part == ".." || part == "." || part == "" }) ||
		len(parts) > 64 || jscompat.Length(name) > 4096 {
		return fail("UNSAFE_ARCHIVE", "The archive contains an unsafe path.")
	}
	if name != "snapshot" && (!strings.HasPrefix(name, "snapshot/") || !slices.ContainsFunc(roots, func(root Root) bool { return parts[1] == root.ID })) {
		return fail("UNSAFE_ARCHIVE", "The archive contains a path outside its recorded roots.")
	}
	if header.Type != "file" && header.Type != "directory" {
		return fail("UNSAFE_ARCHIVE", "Backups support regular files and directories. Remove symbolic links, hard links, and special files from the selected data roots before retrying.")
	}
	if slices.ContainsFunc(parts, func(part string) bool { return strings.HasPrefix(part, ".ludock-restore-") }) {
		return fail("RESTORE_RECOVERY_REQUIRED", "A previous restore has staging data in this root. Complete its recovery before making a backup.")
	}
	if header.Size < 0 || header.Size > 1<<53-1 {
		return fail("UNSAFE_ARCHIVE", "The archive has an invalid entry size.")
	}
	if (name == "snapshot" || len(parts) == 2) && header.Type != "directory" {
		return fail("BACKUP_ROOT_NOT_DIRECTORY", "Backup roots must be mounted directories; individual file mounts are not supported.")
	}
	return nil
}

var errCapacity = fail("BACKUP_CAPACITY", "The archive exceeds the configured size limit.")

// ValidateStream checks a whole archive and every recorded root.
func ValidateStream(input io.Reader, roots []Root, maxBytes int64, onChunk func([]byte)) error {
	names := map[string]bool{}
	found := map[string]bool{}
	var total, read int64
	err := tarstream.Walk(input, func(header *tarstream.Header, _ io.Reader) error {
		if err := ValidateEntry(header, roots); err != nil {
			return err
		}
		name := strings.TrimSuffix(header.Name, "/")
		if names[name] || len(names) >= 100000 {
			return fail("UNSAFE_ARCHIVE", "The archive has duplicate paths or too many entries.")
		}
		names[name] = true
		total += header.Size
		if total > maxBytes {
			return errCapacity
		}
		if parts := strings.Split(name, "/"); len(parts) == 2 {
			found[parts[1]] = true
		}
		return nil
	}, func(chunk []byte) error {
		read += int64(len(chunk))
		if read > maxBytes {
			return errCapacity
		}
		if onChunk != nil {
			onChunk(chunk)
		}
		return nil
	})
	if err != nil {
		if _, ok := apperr.As(err); ok {
			return err
		}
		return fail("INVALID_BACKUP", "The backup archive is invalid or truncated.")
	}
	for _, root := range roots {
		if !found[root.ID] {
			return fail("INVALID_BACKUP", "The archive is missing a recorded data root.")
		}
	}
	return nil
}

// Validate checks a published archive and its checksum.
func (s *Storage) Validate(directory, id string, roots []Root, maxBytes int64, checksum string) error {
	file, err := s.Open(directory, id)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if err := ValidateStream(file, roots, maxBytes, func(chunk []byte) { hash.Write(chunk) }); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != checksum {
		return fail("BACKUP_CHECKSUM", "The backup checksum does not match. Restore was stopped before changing game data.")
	}
	return nil
}

// Plan is the helper layout for a server's data roots.
type Plan struct {
	Roots   []Root
	Mounts  []docker.MountSettings
	Aliases []string
	Checked []launch.Mount
}

// PlanRoots maps each data root to a private alias. Roots that contain a
// nested mount or overlap on the Docker host are refused: they would collide
// during restore staging.
func PlanRoots(context *servers.Context, readOnly bool) (*Plan, error) {
	plan := &Plan{}
	for _, root := range context.Container.FileRoots {
		plan.Roots = append(plan.Roots, Root{ID: root.ID, Path: root.Path})
	}
	if len(plan.Roots) == 0 {
		return nil, fail("NO_BACKUP_ROOTS", "This server has no approved writable data roots.")
	}
	var physicalRoots []string
	for _, root := range plan.Roots {
		mounts := context.Observation.Mounts
		if slices.ContainsFunc(mounts, func(mount identity.Mount) bool { return strings.HasPrefix(mount.Destination, root.Path+"/") }) {
			return nil, fail("NESTED_BACKUP_MOUNT", "A selected root contains a nested mount. Restrict ludock.files to a data subdirectory without nested mounts before backing up.")
		}
		source := ownerMount(mounts, root.Path)
		if source == nil {
			return nil, fail("BACKUP_ROOT_CHANGED", "A selected data root no longer belongs to a writable mount.")
		}
		if !strings.HasPrefix(source.Source, "/") {
			return nil, fail("BACKUP_ROOT_CHANGED", "The physical source of a selected data root could not be verified.")
		}
		physical := jscompat.NormalizePosix(source.Source + root.Path[len(source.Destination):])
		if slices.ContainsFunc(physicalRoots, func(other string) bool {
			return physical == other || strings.HasPrefix(physical, other+"/") || strings.HasPrefix(other, physical+"/")
		}) {
			return nil, fail("OVERLAPPING_BACKUP_ROOTS", "Selected data roots overlap on the Docker host. Restrict ludock.files to one path for each distinct data subtree.")
		}
		physicalRoots = append(physicalRoots, physical)
		alias := "/mounts/" + root.ID
		plan.Aliases = append(plan.Aliases, alias+root.Path[len(source.Destination):])
		mountSource := source.Source
		if source.Type == "volume" && source.Name != "" {
			mountSource = source.Name
		}
		plan.Mounts = append(plan.Mounts, docker.MountSettings{Type: source.Type, Source: mountSource, Target: alias, ReadOnly: readOnly})
		checked := launch.Mount{Type: source.Type, Source: mountSource, Destination: alias}
		if source.Type == "volume" {
			checked.Name = mountSource
		}
		plan.Checked = append(plan.Checked, checked)
		if !files.SafeWritableDataMount(files.Mount{Type: checked.Type, Source: checked.Source, Destination: checked.Destination, RW: true, Name: checked.Name}) {
			return nil, fail("UNSAFE_BACKUP_ROOT", "A selected data mount is unsafe.")
		}
	}
	return plan, nil
}

// HelperRoot is a root's path inside the data helper.
func HelperRoot(context *servers.Context, root Root) (string, error) {
	mount := ownerMount(context.Observation.Mounts, root.Path)
	if mount == nil {
		return "", fail("BACKUP_ROOT_CHANGED", "The selected data root changed.")
	}
	return "/mounts/" + root.ID + root.Path[len(mount.Destination):], nil
}

// DataHelper is a helper container with a server's data roots.
type DataHelper struct {
	Container string
	Roots     []Root
	Cleanup   func()
}

// Helpers creates and drives data helpers.
type Helpers struct {
	Docker  *docker.Client
	Images  *launch.Images
	Storage *Storage
	// pollInterval is shortened in tests.
	pollInterval time.Duration
}

// NewDataHelper starts a helper labelled with the operation, so recovery can
// find it after a restart, and proves its mounts. Cleanup retries until
// Docker confirms removal.
func (h *Helpers) NewDataHelper(ctx context.Context, context *servers.Context, readOnly bool, operationID string) (*DataHelper, error) {
	plan, err := PlanRoots(context, readOnly)
	if err != nil {
		return nil, err
	}
	image, err := h.Images.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	proof, err := launch.MountProof(ctx, h.Docker, h.Images, plan.Checked, operationID)
	if err != nil {
		return nil, err
	}
	capabilities := []string{"DAC_OVERRIDE", "CHOWN", "FOWNER"}
	if readOnly {
		capabilities = []string{"DAC_OVERRIDE"}
	}
	id, err := h.Docker.CreateHelper(ctx, docker.ContainerCreate{Image: image, User: "0", Entrypoint: []string{helper.Binary},
		Cmd: []string{"helper", "idle", "0"}, Env: launch.HelperEnv,
		Labels: map[string]string{"ludock.enable": "false", "ludock.internal": "backup-helper", "ludock.operation": operationID},
		HostConfig: &docker.HostConfig{Mounts: plan.Mounts, NetworkMode: "none", ReadonlyRootfs: true, PidsLimit: 64,
			Memory: 256 * 1024 * 1024, NanoCpus: 1_000_000_000, CapDrop: []string{"ALL"}, CapAdd: capabilities,
			SecurityOpt: []string{"no-new-privileges"}}})
	if err != nil {
		proof.Cleanup()
		return nil, err
	}
	cleanup := func() {
		defer proof.Cleanup()
		h.Docker.RemoveOperationHelper(id)
	}
	if err := h.prepare(ctx, id, plan, proof); err != nil {
		cleanup()
		return nil, err
	}
	return &DataHelper{Container: id, Roots: plan.Roots, Cleanup: sync.OnceFunc(cleanup)}, nil
}

func (h *Helpers) prepare(ctx context.Context, id string, plan *Plan, proof *launch.Proof) error {
	if err := h.Docker.Start(ctx, id); err != nil {
		return err
	}
	if err := launch.AssertMountIdentities(ctx, h.Docker, id, proof.Identities); err != nil {
		return err
	}
	// Resolve label-selected subdirectories without following symbolic links.
	for _, alias := range plan.Aliases {
		if _, err := h.Exec(ctx, id, helper.Command("file", helper.FileRequest{Operation: "check", Root: alias, Blocked: []string{}}), nil); err != nil {
			return err
		}
	}
	return nil
}

// terminate stops a helper. Docker cannot signal an exec; stopping the
// helper ends its PID namespace, so nothing can still run inside it.
func (h *Helpers) terminate(id string) bool {
	ctx := context.Background()
	zero := 0
	if err := h.Docker.Stop(ctx, id, &zero); err != nil {
		if status := docker.StatusCode(err); status == 404 {
			return true
		} else if status != 304 {
			return false
		}
	}
	info, err := h.Docker.Inspect(ctx, id)
	if docker.IsNotFound(err) {
		return true
	}
	return err == nil && !info.State.Running
}

// waitForExit waits for an exec to finish. Losing the stream does not end
// the process, so the next step must not write concurrently with it; once
// expired, the helper is stopped so a hang cannot hold the operation.
func (h *Helpers) waitForExit(id, execID string, expired func() bool) *docker.ExecInspect {
	interval := h.pollInterval
	if interval == 0 {
		interval = 250 * time.Millisecond
	}
	for {
		result, err := h.Docker.InspectExec(context.Background(), execID)
		if err == nil && !result.Running && result.ExitCode != nil {
			return result
		}
		if docker.IsNotFound(err) {
			return nil
		}
		if expired() && h.terminate(id) {
			return nil
		}
		time.Sleep(interval)
	}
}

func (h *Helpers) start(ctx context.Context, id, execID string) (*docker.Conn, error) {
	conn, err := h.Docker.StartExec(ctx, execID)
	if err != nil {
		var open *docker.StreamOpenError
		if errors.As(err, &open) && open.RequestStarted {
			h.waitForExit(id, execID, func() bool { return ctx.Err() != nil })
		}
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		return nil, err
	}
	return conn, nil
}

var (
	errTimeout = fail("BACKUP_TIMEOUT", "A data operation timed out. Review the operation recovery state.")
	errFailed  = fail("DATA_OPERATION_FAILED", "A data operation failed. The server remains stopped until its data is safe.")
)

// Exec runs a short helper command (two-minute limit) and returns stdout.
func (h *Helpers) Exec(ctx context.Context, id string, command []string, assertAccess func() error) ([]byte, error) {
	execID, err := h.Docker.CreateExec(ctx, id, docker.ExecCreate{Cmd: command, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return nil, err
	}
	if assertAccess != nil {
		if err := assertAccess(); err != nil {
			return nil, err
		}
	}
	deadline, cancel := context.WithTimeoutCause(context.WithoutCancel(ctx), 2*time.Minute, errTimeout)
	defer cancel()
	conn, err := h.start(deadline, id, execID)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(deadline, func() { conn.Close() })
	defer stop()
	var output limited
	demuxErr := docker.Demux(conn, &output, nil)
	conn.Close()
	result := h.waitForExit(id, execID, func() bool { return deadline.Err() != nil })
	if demuxErr != nil {
		if errors.Is(demuxErr, errOutput) {
			return nil, errOutput
		}
		if deadline.Err() != nil {
			return nil, errTimeout
		}
		return nil, errFailed
	}
	if result == nil || *result.ExitCode != 0 {
		return nil, errFailed
	}
	return output.data, nil
}

var errOutput = fail("HELPER_OUTPUT", "A data operation produced too much output.")

type limited struct{ data []byte }

func (l *limited) Write(p []byte) (int, error) {
	if len(l.data)+len(p) > 4*1024*1024 {
		return 0, errOutput
	}
	l.data = append(l.data, p...)
	return len(p), nil
}

// IsRunning reports whether a reused restore helper is still running.
func (h *Helpers) IsRunning(ctx context.Context, id string) (bool, error) {
	info, err := h.Docker.Inspect(ctx, id)
	if err != nil {
		return false, err
	}
	return info.State.Running, nil
}

// archive streams one root as tar from the helper.
func (h *Helpers) archive(ctx context.Context, id, root string, assertAccess func() error) (io.ReadCloser, func() error, error) {
	request := helper.FileRequest{Operation: "backup", Root: root, Blocked: []string{}}
	execID, err := h.Docker.CreateExec(ctx, id, docker.ExecCreate{Cmd: helper.Command("file", request), AttachStdout: true, AttachStderr: true})
	if err != nil {
		return nil, nil, err
	}
	if assertAccess != nil {
		if err := assertAccess(); err != nil {
			return nil, nil, err
		}
	}
	conn, err := h.Docker.StartExec(ctx, execID)
	if err != nil {
		return nil, nil, err
	}
	timer := time.AfterFunc(30*time.Minute, func() { conn.Close() })
	reader := docker.Stdout(conn)
	closer := &closeFunc{Reader: reader, close: func() error { timer.Stop(); conn.Close(); return reader.Close() }}
	complete := func() error {
		result, err := h.Docker.InspectExec(context.WithoutCancel(ctx), execID)
		if err != nil || result.Running || result.ExitCode == nil || *result.ExitCode != 0 {
			return fail("UNSAFE_ARCHIVE", "Archive reading failed. Backups require regular files and directories without symbolic links, hard links, special files, or inaccessible paths.")
		}
		return nil
	}
	return closer, complete, nil
}

type closeFunc struct {
	io.Reader
	close func() error
}

func (c *closeFunc) Close() error { return c.close() }

// WriteSnapshot repacks every root under stable root IDs into
// <id>.tar.partial, then publishes it. assertStopped runs between roots and
// before publishing.
func (h *Helpers) WriteSnapshot(ctx context.Context, context *servers.Context, data *DataHelper, settings Settings, id string,
	availableBytes int64, assertStopped func() error, assertAccess func() error) (int64, string, error) {
	if err := checkID(id); err != nil {
		return 0, "", err
	}
	dir, err := h.Storage.pin(settings.Destination)
	if err != nil {
		return 0, "", err
	}
	defer dir.close()
	diskBudget, err := budget(dir, settings.ReserveBytes, 0)
	if err != nil {
		return 0, "", err
	}
	partial := id + ".tar.partial"
	fd, err := unix.Openat(dir.fd, partial, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return 0, "", err
	}
	file := os.NewFile(uintptr(fd), partial)
	published := false
	defer func() {
		if !published {
			file.Close()
			unix.Unlinkat(dir.fd, partial, 0)
		}
	}()
	hash := sha256.New()
	var size, checked int64
	buffer := make([]byte, 0, 64*1024)
	flush := func() error {
		if len(buffer) == 0 {
			return nil
		}
		_, err := file.Write(buffer)
		buffer = buffer[:0]
		return err
	}
	write := func(chunk []byte) error {
		if assertAccess != nil {
			if err := assertAccess(); err != nil {
				return err
			}
		}
		size += int64(len(chunk))
		if size > availableBytes {
			return fail("BACKUP_CAPACITY", "The backup exceeds the global byte limit. Remove older backups or increase the limit.")
		}
		if size > diskBudget {
			return fail("BACKUP_CAPACITY", "The backup destination does not have enough free space above its configured reserve.")
		}
		if size-checked > 16*1024*1024 {
			checked = size
			if _, err := budget(dir, settings.ReserveBytes, int64(len(chunk))); err != nil {
				return err
			}
		}
		hash.Write(chunk)
		buffer = append(buffer, chunk...)
		if len(buffer) >= 64*1024 {
			return flush()
		}
		return nil
	}
	header, _ := tarstream.EncodeHeader(tarstream.Header{Name: "snapshot/", Type: "directory", Mode: 0o755})
	if err := write(header); err != nil {
		return 0, "", err
	}
	for _, root := range data.Roots {
		if err := assertStopped(); err != nil {
			return 0, "", err
		}
		helperPath, err := HelperRoot(context, root)
		if err != nil {
			return 0, "", err
		}
		input, complete, err := h.archive(ctx, data.Container, helperPath, assertAccess)
		if err != nil {
			return 0, "", err
		}
		prefix := ""
		names := map[string]bool{}
		err = tarstream.Walk(input, func(header *tarstream.Header, body io.Reader) error {
			name := strings.TrimSuffix(header.Name, "/")
			if strings.HasPrefix(name, "/") || slices.ContainsFunc(strings.Split(name, "/"), func(part string) bool { return part == "" || part == ".." || part == "." }) {
				return fail("UNSAFE_ARCHIVE", "Docker returned an unsafe archive path.")
			}
			if prefix == "" {
				prefix = strings.Split(name, "/")[0]
			}
			if name != prefix && !strings.HasPrefix(name, prefix+"/") {
				return fail("UNSAFE_ARCHIVE", "Docker returned an archive outside the selected data root.")
			}
			suffix := ""
			if header.Type == "directory" {
				suffix = "/"
			}
			mapped, err := MappedHeader(header, "snapshot/"+root.ID+name[len(prefix):]+suffix)
			if err != nil {
				return err
			}
			if err := ValidateEntry(mapped, data.Roots); err != nil {
				return err
			}
			if names[name] || len(names) >= 100000 {
				return fail("UNSAFE_ARCHIVE", "The selected root has duplicate paths or too many entries.")
			}
			names[name] = true
			encoded, err := tarstream.EncodeHeader(*mapped)
			if err != nil {
				return err
			}
			if err := write(encoded); err != nil {
				return err
			}
			chunk := make([]byte, 64*1024)
			for {
				n, readErr := body.Read(chunk)
				if n > 0 {
					if err := write(chunk[:n]); err != nil {
						return err
					}
				}
				if readErr == io.EOF {
					break
				}
				if readErr != nil {
					return readErr
				}
			}
			if padding := tarstream.Padding(mapped.Size); len(padding) > 0 {
				return write(padding)
			}
			return nil
		}, nil)
		if err == nil {
			err = complete()
		}
		input.Close()
		if err != nil {
			if _, ok := apperr.As(err); !ok {
				err = fail("UNSAFE_ARCHIVE", "Archive reading failed. Backups require regular files and directories without symbolic links, hard links, special files, or inaccessible paths.")
			}
			return 0, "", err
		}
		if prefix == "" || !names[prefix] {
			return 0, "", fail("INVALID_BACKUP", "The archive is missing a recorded data root.")
		}
		if err := assertStopped(); err != nil {
			return 0, "", err
		}
	}
	if err := write(tarstream.End()); err != nil {
		return 0, "", err
	}
	if err := flush(); err != nil {
		return 0, "", err
	}
	if _, err := budget(dir, settings.ReserveBytes, 0); err != nil {
		return 0, "", err
	}
	if err := assertStopped(); err != nil {
		return 0, "", err
	}
	if err := file.Sync(); err != nil {
		return 0, "", err
	}
	file.Close()
	if err := unix.Renameat(dir.fd, partial, dir.fd, id+".tar"); err != nil {
		unix.Unlinkat(dir.fd, partial, 0)
		published = true
		return 0, "", err
	}
	published = true
	if err := unix.Fsync(dir.fd); err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

// ExtractToStage streams one root of an archive into the helper's restore
// stage. The checksum covers the whole archive, so a changed archive is
// detected before any game data is replaced.
func (h *Helpers) ExtractToStage(ctx context.Context, directory, id, helperID string, root Root, rootPath, stage string,
	roots []Root, maxBytes int64, checksum string, assertAccess func() error) error {
	request := map[string]any{"root": rootPath, "stage": stage, "maxBytes": maxBytes}
	execID, err := h.Docker.CreateExec(ctx, helperID, docker.ExecCreate{Cmd: helper.Command("restore-extract", request),
		AttachStdin: true, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return err
	}
	if assertAccess != nil {
		if err := assertAccess(); err != nil {
			return err
		}
	}
	restoreTimeout := fail("RESTORE_TIMEOUT", "Restore extraction exceeded its 30 minute execution limit.")
	deadline, cancel := context.WithTimeoutCause(context.WithoutCancel(ctx), 30*time.Minute, restoreTimeout)
	defer cancel()
	conn, err := h.start(deadline, helperID, execID)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(deadline, func() { conn.Close() })
	defer stop()
	expired := func() bool { return deadline.Err() != nil }
	completed := make(chan error, 1)
	go func() {
		err := docker.Demux(conn, nil, nil)
		result := h.waitForExit(helperID, execID, expired)
		if err == nil && (result == nil || *result.ExitCode != 0) {
			err = fail("RESTORE_EXTRACTION", "Restore extraction rejected a changed or unsafe data path.")
		} else if err != nil {
			err = fail("RESTORE_EXTRACTION", "Restore extraction rejected a changed or unsafe data path.")
		}
		completed <- err
	}()
	archive, err := h.Storage.Open(directory, id)
	if err != nil {
		conn.Close()
		<-completed
		return err
	}
	defer archive.Close()
	write := func(data []byte) error {
		if assertAccess != nil {
			if err := assertAccess(); err != nil {
				return err
			}
		}
		_, err := conn.Write(data)
		return err
	}
	names := map[string]bool{}
	hash := sha256.New()
	var total int64
	prefix := "snapshot/" + root.ID + "/"
	err = tarstream.Walk(archive, func(header *tarstream.Header, body io.Reader) error {
		if err := ValidateEntry(header, roots); err != nil {
			return err
		}
		name := strings.TrimSuffix(header.Name, "/")
		if names[name] || len(names) >= 100000 {
			return fail("UNSAFE_ARCHIVE", "The archive contains duplicate paths or too many entries.")
		}
		names[name] = true
		if !strings.HasPrefix(name, prefix) {
			return nil
		}
		uid, gid, mtime, err := EntryMetadata(header)
		if err != nil {
			return err
		}
		record, _ := json.Marshal(map[string]any{"name": name[len(prefix):], "type": header.Type, "mode": header.Mode,
			"uid": uid, "gid": gid, "mtime": mtime, "size": header.Size})
		if err := write(append(record, '\n')); err != nil {
			return err
		}
		chunk := make([]byte, 64*1024)
		for {
			n, readErr := body.Read(chunk)
			if n > 0 {
				if err := write(chunk[:n]); err != nil {
					return err
				}
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}, func(chunk []byte) error {
		total += int64(len(chunk))
		if total > maxBytes {
			return fail("BACKUP_CAPACITY", "The archive exceeds its configured size limit.")
		}
		hash.Write(chunk)
		return nil
	})
	if err == nil && hex.EncodeToString(hash.Sum(nil)) != checksum {
		err = fail("BACKUP_CHECKSUM", "The archive changed while it was being staged. No existing game data was replaced.")
	}
	if err != nil {
		conn.Close()
		<-completed
		if _, ok := apperr.As(err); !ok {
			err = fail("INVALID_BACKUP", "The backup archive is invalid or truncated.")
		}
		return err
	}
	conn.CloseWrite()
	return <-completed
}

// ownerMount is the deepest writable bind or volume containing rootPath.
func ownerMount(mounts []identity.Mount, rootPath string) *identity.Mount {
	var owner *identity.Mount
	for index := range mounts {
		mount := &mounts[index]
		if mount.Writable && (mount.Type == "bind" || mount.Type == "volume") &&
			(rootPath == mount.Destination || strings.HasPrefix(rootPath, mount.Destination+"/")) &&
			(owner == nil || len(mount.Destination) > len(owner.Destination)) {
			owner = mount
		}
	}
	return owner
}
