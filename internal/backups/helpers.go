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
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/tarstream"
)

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
		physical := path.Clean(source.Source + root.Path[len(source.Destination):])
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

type DataHelper struct {
	Container string
	Roots     []Root
	Cleanup   func()
}

type Helpers struct {
	Docker  *docker.Client
	Images  *launch.Images
	Storage *Storage
	// pollInterval and openLimit are shortened in tests.
	pollInterval time.Duration
	openLimit    time.Duration
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
	// The server is stopped and its locks are held, so a start Docker never
	// answers must not wait for the operation's lifetime.
	limit := time.Minute
	if h.openLimit > 0 {
		limit = h.openLimit
	}
	opening, cancel := context.WithTimeoutCause(ctx, limit, errTimeout)
	conn, err := h.start(opening, id, execID)
	cancel()
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
	// buffer coalesces small writes; chunk carries file bodies for every entry.
	buffer, chunk := make([]byte, 0, 64*1024), make([]byte, 64*1024)
	stillAllowed := launch.Periodic(assertAccess)
	flush := func() error {
		if len(buffer) == 0 {
			return nil
		}
		_, err := file.Write(buffer)
		buffer = buffer[:0]
		return err
	}
	write := func(chunk []byte) error {
		if stillAllowed != nil {
			if err := stillAllowed(); err != nil {
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
		if len(buffer) == 0 && len(chunk) >= cap(buffer) {
			_, err := file.Write(chunk)
			return err
		}
		buffer = append(buffer, chunk...)
		if len(buffer) >= cap(buffer) {
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

var errStagingCapacity = fail("RESTORE_CAPACITY", "A data root lacks space for restore staging and the configured reserve.")

type StageTarget struct {
	Root Root
	Path string
}

// ExtractToStage streams an archive into the helper's restore stage of
// every target in one pass, each root through its own extraction. The
// checksum covers the whole archive, so a changed archive is detected before
// any game data is replaced.
func (h *Helpers) ExtractToStage(ctx context.Context, directory, id, helperID string, targets []StageTarget, stage string,
	roots []Root, maxBytes, reserveBytes int64, checksum string, assertAccess func() error) (err error) {
	restoreTimeout := fail("RESTORE_TIMEOUT", "Restore extraction exceeded its 30 minute execution limit.")
	deadline, cancel := context.WithTimeoutCause(context.WithoutCancel(ctx), 30*time.Minute, restoreTimeout)
	defer cancel()
	expired := func() bool { return deadline.Err() != nil }
	type extraction struct {
		conn      *docker.Conn
		completed chan error
	}
	var mu sync.Mutex
	extractions := map[string]*extraction{}
	closeAll := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, extraction := range extractions {
			extraction.conn.Close()
		}
	}
	// wait collects every extraction once; a helper that stopped at the
	// reserve is why writing to it failed.
	waited, extracted := false, error(nil)
	wait := func() error {
		if !waited {
			waited = true
			for _, extraction := range extractions {
				err := <-extraction.completed
				if errors.Is(err, errStagingCapacity) || (extracted == nil && err != nil) {
					extracted = err
				}
			}
		}
		return extracted
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
		request := map[string]any{"root": target.Path, "stage": stage, "maxBytes": maxBytes, "reserveBytes": reserveBytes}
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
		conn, err := h.start(deadline, helperID, execID)
		if err != nil {
			return err
		}
		extraction := &extraction{conn: conn, completed: make(chan error, 1)}
		mu.Lock()
		extractions[target.Root.ID] = extraction
		mu.Unlock()
		go func() {
			err := docker.Demux(conn, nil, nil)
			result := h.waitForExit(helperID, execID, expired)
			switch {
			case result != nil && *result.ExitCode == helper.ExtractFullStatus:
				err = errStagingCapacity
			case err != nil || result == nil || *result.ExitCode != 0:
				err = fail("RESTORE_EXTRACTION", "Restore extraction rejected a changed or unsafe data path.")
			}
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
		_, err := conn.Write(data)
		return err
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
		if names[name] || len(names) >= 100000 {
			return fail("UNSAFE_ARCHIVE", "The archive contains duplicate paths or too many entries.")
		}
		names[name] = true
		rootID, entry, found := strings.Cut(strings.TrimPrefix(name, "snapshot/"), "/")
		extraction := extractions[rootID]
		if !found || extraction == nil {
			return nil
		}
		uid, gid, mtime, err := EntryMetadata(header)
		if err != nil {
			return err
		}
		record, _ := json.Marshal(map[string]any{"name": entry, "type": header.Type, "mode": header.Mode,
			"uid": uid, "gid": gid, "mtime": mtime, "size": header.Size})
		if err := write(extraction.conn, append(record, '\n')); err != nil {
			return err
		}
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
			return fail("BACKUP_CAPACITY", "The archive exceeds its configured size limit.")
		}
		hash.Write(chunk)
		return nil
	})
	if err == nil && hex.EncodeToString(hash.Sum(nil)) != checksum {
		err = fail("BACKUP_CHECKSUM", "The archive changed while it was being staged. No existing game data was replaced.")
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
	return wait()
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
