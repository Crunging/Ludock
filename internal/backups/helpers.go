package backups

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/self"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/tarstream"
)

// Plan is the helper layout for a server's data roots.
type Plan struct {
	Roots   []Root
	Mounts  []docker.MountSettings
	Aliases []string
	Checked []identity.Mount
}

// PlanRoots maps each data root to a private alias. Roots that contain a
// nested mount or overlap on the Docker host are refused: they would collide
// during restore staging.
func PlanRoots(context *servers.Context, own self.Info, readOnly bool) (*Plan, error) {
	plan := &Plan{}
	for _, root := range context.Container.FileRoots {
		plan.Roots = append(plan.Roots, Root{ID: root.ID, Path: root.Path})
	}
	if len(plan.Roots) == 0 {
		return nil, fail("NO_BACKUP_ROOTS", "Ludock found no data folder it can back up for this server. Its game data must be in a writable volume or folder mounted into its container.")
	}
	var physicalRoots []string
	for _, root := range plan.Roots {
		mounts := context.Observation.Mounts
		if files.HoldsMount(root.Path, mounts) {
			return nil, fail("NESTED_BACKUP_MOUNT", "One of the server’s data folders has another mount inside it, which Ludock can’t back up as one folder. List folders without one inside in the ludock.files label, then try again.")
		}
		source := writableOwnerMount(mounts, root.Path)
		if source == nil {
			return nil, fail("BACKUP_ROOT_CHANGED", "One of the server’s data folders is no longer mounted, or no longer writable. Check how the server is defined, such as its Compose file, then try again.")
		}
		if !strings.HasPrefix(source.Source, "/") {
			return nil, fail("BACKUP_ROOT_CHANGED", "Ludock couldn’t tell where one of the server’s data folders is on the host. Check how the server is defined, such as its Compose file, then try again.")
		}
		physical := path.Clean(path.Join(source.Source, source.Subpath) + root.Path[len(source.Destination):])
		if slices.ContainsFunc(physicalRoots, func(other string) bool { return overlaps(physical, other) }) {
			return nil, fail("OVERLAPPING_BACKUP_ROOTS", "Two of the server’s data folders are the same folder on the host, or one is inside the other. List each folder once in the ludock.files label, then try again.")
		}
		physicalRoots = append(physicalRoots, physical)
		alias := helperBase(root)
		plan.Aliases = append(plan.Aliases, alias+root.Path[len(source.Destination):])
		mountSource := source.Source
		if source.Type == "volume" && source.Name != "" {
			mountSource = source.Name
		}
		settings := docker.MountSettings{Type: source.Type, Source: mountSource, Target: alias, ReadOnly: readOnly}
		// NoCopy: Docker never fills an empty folder from the helper's image.
		if source.Type == "volume" {
			settings.VolumeOptions = &docker.VolumeOptions{NoCopy: true, Subpath: source.Subpath}
		}
		plan.Mounts = append(plan.Mounts, settings)
		checked := identity.Mount{Type: source.Type, Source: mountSource, Destination: alias, Writable: true, Subpath: source.Subpath}
		if source.Type == "volume" {
			checked.Name = mountSource
		}
		plan.Checked = append(plan.Checked, checked)
		if !files.SafeWritableDataMount(checked, own) {
			return nil, fail("UNSAFE_BACKUP_ROOT", "One of the server’s data folders isn’t a kind Ludock can safely read or change, such as a volume with special options.")
		}
	}
	return plan, nil
}

// ErrHostMount is a data folder that the host, not Docker, has another disk
// or share mounted on or inside.
var ErrHostMount = fail("NESTED_HOST_MOUNT", "One of the server’s data folders has another disk or network share mounted on or inside it on the host, which Ludock can’t back up or restore as one folder. List folders without one inside in the ludock.files label, or back that folder up separately, then try again.")

// helperBase is the helper's mount that holds a root.
func helperBase(root Root) string { return "/mounts/" + root.ID }

// HelperBaseOf is the helper's mount that holds a HelperRoot path.
func HelperBaseOf(helperPath string) string {
	parts := strings.SplitN(helperPath, "/", 4)
	if len(parts) < 3 || parts[0] != "" || parts[1] != "mounts" {
		return ""
	}
	return "/mounts/" + parts[2]
}

// HelperRoot is a root's path inside the data helper.
func HelperRoot(context *servers.Context, root Root) (string, error) {
	mount := writableOwnerMount(context.Observation.Mounts, root.Path)
	if mount == nil {
		return "", fail("BACKUP_ROOT_CHANGED", "One of the server’s data folders changed. Reload the page and try again.")
	}
	return helperBase(root) + root.Path[len(mount.Destination):], nil
}

// AssertOwnMounts refuses, through a running helper, roots that hold or are
// reached through a mount the host added and Docker doesn't show, or, with
// item, such a mount on the item's path or inside it.
func (h *Helpers) AssertOwnMounts(ctx context.Context, context *servers.Context, data *DataHelper, item string, assertAccess func() error) error {
	for _, root := range data.Roots {
		path, err := HelperRoot(context, root)
		if err != nil {
			return err
		}
		request := helper.FileRequest{Operation: "mounts", Root: path, Path: item, Base: helperBase(root), Blocked: []string{}}
		if _, err := h.Exec(ctx, data.Container, helper.Command("file", request), assertAccess); err != nil {
			return err
		}
	}
	return nil
}

type DataHelper struct {
	Container string
	Roots     []Root
	Cleanup   func()
}

type Helpers struct {
	Docker  *docker.Client
	Images  *launch.Images
	Storage *Storage
	// pollInterval, openLimit, and readLimit are shortened in tests.
	pollInterval time.Duration
	openLimit    time.Duration
	readLimit    time.Duration
}

func (h *Helpers) backupLimit() time.Duration {
	if h.readLimit > 0 {
		return h.readLimit
	}
	return helper.BackupLimit
}

// NewDataHelper starts a helper labelled with the operation, so recovery can
// find it after a restart, and proves its mounts. Cleanup retries until
// Docker confirms removal.
func (h *Helpers) NewDataHelper(ctx context.Context, context *servers.Context, readOnly bool, operationID string) (*DataHelper, error) {
	own, err := h.Storage.Own.Read(ctx)
	if err != nil {
		return nil, err
	}
	plan, err := PlanRoots(context, own, readOnly)
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
	// FSETID keeps a restored root's setgid bit, which chmod by a helper
	// outside the root's group would otherwise clear.
	capabilities := []string{"DAC_OVERRIDE", "CHOWN", "FOWNER", "FSETID"}
	if readOnly {
		capabilities = []string{"DAC_OVERRIDE"}
	}
	labels := launch.HelperLabels(launch.BackupHelper)
	labels["ludock.operation"] = operationID
	host := launch.HelperHost(plan.Mounts, capabilities...)
	// Only its operation removes it, or recovery after a restart.
	host.AutoRemove = false
	id, err := h.Docker.CreateHelper(ctx, docker.ContainerCreate{Image: image, User: "0", Entrypoint: []string{helper.Binary},
		Cmd: []string{"helper", "idle", "0"}, Env: launch.HelperEnv, Labels: labels, HostConfig: host})
	if err != nil {
		proof.Cleanup()
		return nil, launch.HelperFailed(err)
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
		return launch.HelperFailed(err)
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

var (
	errTimeout       = fail("BACKUP_TIMEOUT", "Copying the server’s data took too long and was stopped.")
	errBackupTimeout = fail("BACKUP_TIMEOUT", "Reading a data folder for the backup took longer than 4 hours, so the backup was stopped. Exclude large folders with ludock.files, or back up through the tool that created the server.")
	errFailed        = fail("DATA_OPERATION_FAILED", "Copying the server’s data failed. The server stays stopped until its data is safe.")
)

// Exec runs a short helper command (two-minute limit) and returns stdout.
func (h *Helpers) Exec(ctx context.Context, id string, command []string, assertAccess func() error) ([]byte, error) {
	return h.ExecWithin(ctx, id, command, assertAccess, 2*time.Minute)
}

// ExecWithin is Exec with its own time limit, for commands that walk a whole
// data root.
func (h *Helpers) ExecWithin(ctx context.Context, id string, command []string, assertAccess func() error, limit time.Duration) ([]byte, error) {
	return launch.Exec(ctx, h.Docker, id, launch.ExecOptions{Cmd: command, AssertAccess: assertAccess, Limit: limit,
		Errors: helperErrors, Settle: true, Poll: h.pollInterval})
}

var helperErrors = &launch.ExecErrors{Timeout: errTimeout, Failed: errFailed, Unsafe: errFailed,
	OutputLimit: fail("HELPER_OUTPUT", "Ludock’s helper sent back more than expected, so the work was stopped."),
	Exits:       map[int]*apperr.Error{helper.FileMounted: ErrHostMount}}

// IsRunning reports whether a reused restore helper is still running.
func (h *Helpers) IsRunning(ctx context.Context, id string) (bool, error) {
	info, err := h.Docker.Inspect(ctx, id)
	if err != nil {
		return false, err
	}
	return info.State.Running, nil
}

// archive streams one root as tar from the helper.
func (h *Helpers) archive(ctx context.Context, id, base, root string, assertAccess func() error) (io.ReadCloser, func() error, error) {
	request := helper.FileRequest{Operation: "backup", Root: root, Base: base, Blocked: []string{}}
	var execID string
	err := launch.Step(ctx, "creating a helper command", func(ctx context.Context) (err error) {
		execID, err = h.Docker.CreateExec(ctx, id, docker.ExecCreate{Cmd: helper.Command("file", request), AttachStdout: true, AttachStderr: true})
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	if assertAccess != nil {
		if err := assertAccess(); err != nil {
			return nil, nil, err
		}
	}
	// The server is stopped and its locks are held, so a start Docker never
	// answers must not wait for the operation's lifetime.
	limit := time.Minute
	if h.openLimit > 0 {
		limit = h.openLimit
	}
	opening, cancel := context.WithTimeoutCause(ctx, limit, errTimeout)
	conn, err := launch.Start(opening, h.Docker, id, execID, h.pollInterval)
	cancel()
	if err != nil {
		return nil, nil, err
	}
	var expired atomic.Bool
	timer := time.AfterFunc(h.backupLimit(), func() { expired.Store(true); conn.Close() })
	stderr := &launch.FirstBytes{Limit: 4096}
	reader, writer := io.Pipe()
	go func() { writer.CloseWithError(docker.Demux(conn, writer, stderr)) }()
	closer := &closeFunc{Reader: reader, close: func() error { timer.Stop(); conn.Close(); return reader.Close() }}
	complete := func() error {
		if expired.Load() {
			return errBackupTimeout
		}
		// Docker away as the stream ends means it cut the copy short, even if
		// the exec it kept then exits with an error once Docker is back.
		code, err := launch.FileExit(ctx, h.Docker, execID, stderr)
		if err != nil {
			return err
		}
		if code == helper.FileMounted {
			return ErrHostMount
		}
		if code != 0 {
			return fail("UNSAFE_ARCHIVE", "Ludock couldn’t read everything in the server’s data folders, so the backup was stopped. A backup takes only ordinary files and folders that Ludock can read, not links (symbolic or hard) or device files.")
		}
		return nil
	}
	return closer, complete, nil
}

// smallestArchive is the size of WriteSnapshot's archive of roots when each
// is an empty folder: the headers and end blocks every backup of them has.
func smallestArchive(roots []Root) int64 {
	header, _ := tarstream.EncodeHeader(tarstream.Header{Name: "snapshot/", Type: "directory", Mode: 0o755})
	size := int64(len(header) + len(tarstream.End()))
	for _, root := range roots {
		header, _ := tarstream.EncodeHeader(tarstream.Header{Name: "snapshot/" + root.ID + "/", Type: "directory"})
		size += int64(len(header))
	}
	return size
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
	partial := archivePartial(id)
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
	// out coalesces small writes; chunk carries file bodies for every entry.
	out, chunk := newCoalescer(file), make([]byte, 64*1024)
	stillAllowed := launch.Periodic(assertAccess)
	write := func(chunk []byte) error {
		if stillAllowed != nil {
			if err := stillAllowed(); err != nil {
				return err
			}
		}
		size += int64(len(chunk))
		if size > availableBytes {
			return CapacityError(codeLimit, 0)
		}
		if size > diskBudget {
			return CapacityError(codeDisk, 0)
		}
		if size-checked > 16*1024*1024 {
			checked = size
			if _, err := budget(dir, settings.ReserveBytes, int64(len(chunk))); err != nil {
				return err
			}
		}
		hash.Write(chunk)
		return out.write(chunk)
	}
	header, _ := tarstream.EncodeHeader(tarstream.Header{Name: "snapshot/", Type: "directory", Mode: 0o755})
	if err := write(header); err != nil {
		return 0, "", err
	}
	// Restore refuses an archive of more than maxEntries entries, counting
	// this header, so a backup it would refuse is refused while written.
	entries := 1
	for _, root := range data.Roots {
		if err := assertStopped(); err != nil {
			return 0, "", err
		}
		helperPath, err := HelperRoot(context, root)
		if err != nil {
			return 0, "", err
		}
		input, complete, err := h.archive(ctx, data.Container, helperBase(root), helperPath, assertAccess)
		if err != nil {
			return 0, "", err
		}
		prefix := ""
		names := map[string]bool{}
		err = tarstream.Walk(input, func(header *tarstream.Header, body io.Reader) error {
			name := strings.TrimSuffix(header.Name, "/")
			if strings.HasPrefix(name, "/") || slices.ContainsFunc(strings.Split(name, "/"), func(part string) bool { return part == "" || part == ".." || part == "." }) {
				return fail("UNSAFE_ARCHIVE", "A file in the server’s data has a name Ludock can’t store safely, so the backup was stopped.")
			}
			if prefix == "" {
				prefix = strings.Split(name, "/")[0]
			}
			if name != prefix && !strings.HasPrefix(name, prefix+"/") {
				return fail("UNSAFE_ARCHIVE", "Ludock was handed a file from outside the server’s data folders, so the backup was stopped.")
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
			if names[name] || entries >= maxEntries {
				return fail("UNSAFE_ARCHIVE", "The server’s data folders have duplicate paths or too many entries.")
			}
			names[name] = true
			entries++
			encoded, err := tarstream.EncodeHeader(*mapped)
			if err != nil {
				return err
			}
			if err := write(encoded); err != nil {
				return err
			}
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
		// A stream cut off at the time limit or by Docker going away fails as
		// a broken archive; the limit or Docker is the reason.
		if completeErr := complete(); err == nil || errors.Is(completeErr, errBackupTimeout) || errors.Is(completeErr, ErrHostMount) ||
			docker.Unavailable(completeErr) {
			err = completeErr
		}
		input.Close()
		if err != nil {
			if _, ok := apperr.As(err); !ok && !docker.Unavailable(err) {
				err = fail("UNSAFE_ARCHIVE", "Ludock couldn’t read everything in the server’s data folders, so the backup was stopped. A backup takes only ordinary files and folders that Ludock can read, not links (symbolic or hard) or device files.")
			}
			return 0, "", err
		}
		if prefix == "" || !names[prefix] {
			return 0, "", fail("INVALID_BACKUP", "The archive is missing one of the server’s data folders.")
		}
		if err := assertStopped(); err != nil {
			return 0, "", err
		}
	}
	if err := write(tarstream.End()); err != nil {
		return 0, "", err
	}
	if err := out.flush(); err != nil {
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

// coalescer joins small writes into ones of up to its buffer's size, which
// never grows: an archive is written as it streams, never held in memory.
type coalescer struct {
	file   io.Writer
	buffer []byte
}

func newCoalescer(file io.Writer) *coalescer {
	return &coalescer{file: file, buffer: make([]byte, 0, 64*1024)}
}

func (c *coalescer) write(chunk []byte) error {
	if len(c.buffer)+len(chunk) > cap(c.buffer) {
		if err := c.flush(); err != nil {
			return err
		}
	}
	if len(chunk) >= cap(c.buffer) {
		_, err := c.file.Write(chunk)
		return err
	}
	c.buffer = append(c.buffer, chunk...)
	return nil
}

func (c *coalescer) flush() error {
	if len(c.buffer) == 0 {
		return nil
	}
	_, err := c.file.Write(c.buffer)
	c.buffer = c.buffer[:0]
	return err
}

var errStagingCapacity = fail("RESTORE_CAPACITY", "The server’s disk doesn’t have room to unpack the backup next to the current data and still keep its minimum free space. Free up space on that disk, then try again.")

type StageTarget struct {
	Root Root
	Path string
	// Item, when set, stages only that path below the root and what it holds.
	Item string
}

var (
	errExtractCut = errors.New("restore extraction stream closed")
	errExtraction = fail("RESTORE_EXTRACTION", "A folder in the server’s data changed, or isn’t safe to write to, while the backup was unpacking, so the restore was stopped.")
	// errInputCut is a helper that saw its input end early, and errExitUnknown
	// one whose exit Docker didn't report: neither refused anything.
	errInputCut    = errors.New("restore extraction input ended early")
	errExitUnknown = errors.New("restore extraction exit unknown")
	// ErrStreamCut is an unpack whose stream to the helper was cut while
	// Docker kept answering, as when a proxy in front of its socket reloads.
	ErrStreamCut = fail("RESTORE_STREAM_CUT", "The connection to Docker dropped while the backup was unpacking, so the restore was stopped. Nothing was replaced.")
)

// ExtractToStage streams an archive into the helper's restore stage of
// every target in one pass, each root through its own extraction. The
// checksum covers the whole archive, so a changed archive is detected before
// any game data is replaced.
func (h *Helpers) ExtractToStage(ctx context.Context, directory, id, helperID string, targets []StageTarget, stage string,
	roots []Root, maxBytes, reserveBytes int64, checksum string, assertAccess func() error) (err error) {
	restoreTimeout := fail("RESTORE_TIMEOUT", "Unpacking the backup took longer than 30 minutes, so the restore was stopped.")
	deadline, cancel := context.WithTimeoutCause(context.WithoutCancel(ctx), 30*time.Minute, restoreTimeout)
	defer cancel()
	expired := func() bool { return deadline.Err() != nil }
	type extraction struct {
		conn      *docker.Conn
		completed chan error
		// sent counts the records written; report is what the helper printed,
		// how many it read, set before completed.
		sent   int
		report string
	}
	var mu sync.Mutex
	extractions := map[string]*extraction{}
	items := map[string]string{}
	closeAll := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, extraction := range extractions {
			extraction.conn.Close()
		}
	}
	// wait collects every extraction once. A helper that stopped at the
	// reserve, or refused what it was sent, is why writing to it failed; one
	// that only saw its input end, or whose exit is unknown, isn't.
	waited, extracted := false, error(nil)
	wait := func() error {
		if !waited {
			waited = true
			rank := func(err error) int {
				switch {
				case errors.Is(err, errStagingCapacity):
					return 3
				case errors.Is(err, errExtraction):
					return 2
				case err != nil:
					return 1
				}
				return 0
			}
			for _, extraction := range extractions {
				if err := <-extraction.completed; rank(err) > rank(extracted) {
					extracted = err
				}
			}
		}
		return extracted
	}
	// cut explains a stream that ended early: Docker away as it ended, or
	// the connection alone.
	ping := func() error { return h.Docker.Ping(docker.Promptly(context.WithoutCancel(ctx))) }
	cut := func(pingErr error) error {
		if docker.Unavailable(pingErr) {
			return pingErr
		}
		return ErrStreamCut
	}
	defer func() {
		if err != nil {
			closeAll()
			if extracted := wait(); errors.Is(extracted, errStagingCapacity) {
				err = extracted
			}
		}
	}()
	for _, target := range targets {
		request := helper.ExtractRequest{MaxBytes: maxBytes, ReserveBytes: &reserveBytes, Root: target.Path, Stage: stage}
		var execID string
		err := launch.Step(ctx, "creating a helper command", func(ctx context.Context) (err error) {
			execID, err = h.Docker.CreateExec(ctx, helperID, docker.ExecCreate{Cmd: helper.Command("restore-extract", request),
				AttachStdin: true, AttachStdout: true, AttachStderr: true})
			return err
		})
		if err != nil {
			return err
		}
		if assertAccess != nil {
			if err := assertAccess(); err != nil {
				return err
			}
		}
		conn, err := launch.Start(deadline, h.Docker, helperID, execID, h.pollInterval)
		if err != nil {
			return err
		}
		extraction := &extraction{conn: conn, completed: make(chan error, 1)}
		mu.Lock()
		extractions[target.Root.ID] = extraction
		items[target.Root.ID] = target.Item
		mu.Unlock()
		go func() {
			out := &launch.FirstBytes{Limit: 64}
			err := docker.Demux(conn, out, nil)
			result := launch.WaitForExit(h.Docker, helperID, execID, h.pollInterval, expired)
			switch {
			case result == nil:
				err = errExitUnknown
			case *result.ExitCode == helper.ExtractFullStatus:
				err = errStagingCapacity
			case *result.ExitCode == helper.ExtractCutStatus || (*result.ExitCode == 0 && err != nil):
				err = errInputCut
			case *result.ExitCode != 0:
				err = errExtraction
			}
			extraction.report = out.String()
			extraction.completed <- err
		}()
	}
	defer closeAll()
	stop := context.AfterFunc(deadline, closeAll)
	defer stop()
	archive, err := h.Storage.Open(directory, id)
	if err != nil {
		return err
	}
	defer archive.Close()
	stillAllowed := launch.Periodic(assertAccess)
	write := func(conn *docker.Conn, data []byte) error {
		if stillAllowed != nil {
			if err := stillAllowed(); err != nil {
				return err
			}
		}
		if _, err := conn.Write(data); err != nil {
			return errExtractCut
		}
		return nil
	}
	names := map[string]bool{}
	hash := sha256.New()
	var total int64
	chunk := make([]byte, 64*1024)
	err = tarstream.Walk(archive, func(header *tarstream.Header, body io.Reader) error {
		if err := ValidateEntry(header, roots); err != nil {
			return err
		}
		name := strings.TrimSuffix(header.Name, "/")
		if names[name] || len(names) >= maxEntries {
			return fail("UNSAFE_ARCHIVE", "The archive contains duplicate paths or too many entries.")
		}
		names[name] = true
		rootID, entry, _ := strings.Cut(strings.TrimPrefix(name, "snapshot/"), "/")
		extraction := extractions[rootID]
		if extraction == nil {
			return nil
		}
		item := items[rootID]
		if item != "" && entry != item && !strings.HasPrefix(entry, item+"/") {
			return nil
		}
		// A whole root's own entry, which comes first and which validation
		// requires, gives the root its owner and permissions as it commits.
		uid, gid, mtime, err := EntryMetadata(header)
		if err != nil {
			return err
		}
		record, _ := json.Marshal(helper.ExtractRecord{GID: &gid, Mode: &header.Mode, Mtime: &mtime, Name: entry, Size: &header.Size,
			Type: header.Type, UID: &uid})
		if err := write(extraction.conn, append(record, '\n')); err != nil {
			return err
		}
		mu.Lock()
		extraction.sent++
		mu.Unlock()
		for {
			n, readErr := body.Read(chunk)
			if n > 0 {
				if err := write(extraction.conn, chunk[:n]); err != nil {
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
			return errGrown
		}
		hash.Write(chunk)
		return nil
	})
	if err == nil && hex.EncodeToString(hash.Sum(nil)) != checksum {
		err = fail("BACKUP_CHECKSUM", "The backup archive changed while it was being unpacked. No game data was replaced.")
	}
	if errors.Is(err, errExtractCut) {
		// The archive is fine; the stream to a helper broke. A helper that
		// refused what it was sent closed it; otherwise it was cut. Docker is
		// asked first, since waiting for the helpers waits for it to answer.
		pingErr := ping()
		closeAll()
		extracted := wait()
		if expired() {
			return context.Cause(deadline)
		}
		if errors.Is(extracted, errStagingCapacity) || errors.Is(extracted, errExtraction) {
			return extracted
		}
		return cut(pingErr)
	}
	if err != nil {
		if _, ok := apperr.As(err); !ok {
			err = fail("INVALID_BACKUP", "The backup archive is invalid or truncated.")
		}
		return err
	}
	for _, extraction := range extractions {
		extraction.conn.CloseWrite()
	}
	if err := wait(); expired() {
		return context.Cause(deadline)
	} else if errors.Is(err, errInputCut) || errors.Is(err, errExitUnknown) {
		return cut(ping())
	} else if err != nil {
		return err
	}
	// Every record was sent: a helper that read fewer saw the stream end
	// cleanly between two.
	for _, extraction := range extractions {
		if extraction.report != "ok "+strconv.Itoa(extraction.sent) {
			return cut(ping())
		}
	}
	return nil
}

// writableOwnerMount is the deepest writable bind or volume containing rootPath.
func writableOwnerMount(mounts []identity.Mount, rootPath string) *identity.Mount {
	var owner *identity.Mount
	for index := range mounts {
		mount := &mounts[index]
		if mount.Writable && (mount.Type == "bind" || mount.Type == "volume") &&
			helper.Within(rootPath, mount.Destination) &&
			(owner == nil || len(mount.Destination) > len(owner.Destination)) {
			owner = mount
		}
	}
	return owner
}
