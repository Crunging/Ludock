package files

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/datasize"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/textutil"
)

var log = logging.New("files")

var messages = map[string]string{
	"INVALID_PATH":                "Invalid file path",
	"INVALID_NAME":                "Invalid file or folder name",
	"ROOT_NOT_FOUND":              "File root not found",
	"ROOT_MUTATION":               "The configured root cannot be changed",
	"ROOT_DOWNLOAD":               "Choose a file or folder to download",
	"UNSAFE_OR_CHANGED_FILE_PATH": "The path changed, is unsafe, or cannot be accessed",
	"FILE_TARGET_CHANGED":         "The server changed while preparing file access. Refresh and try again.",
	"FILE_ROOT_CHANGED":           "The file root changed. Refresh and try again.",
	"FILE_OPERATION_TIMEOUT":      "The file change took too long. Refresh and try again.",
	"INVALID_FILE_VERSION":        "Reopen the file before saving it",
	"INVALID_SELECTION":           "Choose fewer items, up to 500 in one folder",
	"MOVE_INTO_ITSELF":            "A folder can’t be moved into itself",
	"MOVE_SAME_FOLDER":            "Choose a different folder to move to",
}

// Error builds a file error with its public message.
func Error(code string, status int) *apperr.Error {
	message, ok := messages[code]
	if !ok {
		message = "The file change failed"
	}
	return apperr.New(code, status, message)
}

// PreviewLimit is the largest file shown inline; larger or binary files are
// downloaded instead.
const PreviewLimit = 512 * 1024

type Target struct {
	// ServerID is the logical server, which records outlive containers by.
	ServerID    string
	ContainerID string
	Name        string
	GameType    string
	Roots       []Root
}

// Service accesses server files through helper containers.
type Service struct {
	DB     *db.DB
	Docker *docker.Client
	Images *launch.Images
	// Managed reports whether Ludock manages a container, including an
	// administrator's choice; nil reads its labels and image alone.
	Managed func(ctx context.Context, info *docker.ContainerInspect) bool
	// ListerIdle is how long a listing helper is kept after its last use; zero
	// means 30 seconds.
	ListerIdle time.Duration

	mu sync.Mutex
	// listers are idle read-only helpers kept for the next listing of the
	// same root, by container and root path.
	listers map[string]*lister
}

const listerIdle = 30 * time.Second

type lister struct {
	access *Access
	timer  *time.Timer
}

func NormalizeRelativePath(value string) (string, error) {
	if strings.Contains(value, "\x00") || strings.HasPrefix(value, "/") || slices.Contains(strings.Split(value, "/"), "..") {
		return "", Error("INVALID_PATH", 400)
	}
	if value == "" {
		value = "."
	}
	normalized := path.Clean(value)
	if normalized == "." {
		normalized = ""
	}
	if normalized == ".." || strings.HasPrefix(normalized, "../") {
		return "", Error("INVALID_PATH", 400)
	}
	return normalized, nil
}

// ValidateName rejects names that are empty, padded, contain separators or
// control characters (which also forge terminal output in logs), or are
// longer than 255 characters.
func ValidateName(name string) error {
	if name == "" || textutil.Length(name) > 255 || name == "." || name == ".." || strings.Contains(name, "/") ||
		strings.TrimSpace(name) != name || strings.IndexFunc(name, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return Error("INVALID_NAME", 400)
	}
	return nil
}

func joinRelative(parent, name string) string {
	if parent == "" {
		return name
	}
	return path.Join(parent, name)
}

func resolve(target Target, rootID, relative string) (Root, string, error) {
	index := slices.IndexFunc(target.Roots, func(root Root) bool { return root.ID == rootID })
	if index < 0 {
		return Root{}, "", Error("ROOT_NOT_FOUND", 404)
	}
	normalized, err := NormalizeRelativePath(relative)
	return target.Roots[index], normalized, err
}

// Access is a running file helper bound to one root.
type Access struct {
	service      *Service
	container    string
	blocked      []string
	mounts       []string
	assertAccess func() error
	cleanupOnce  sync.Once
	cleanupErr   error
	proof        *launch.Proof
	// signature is the game container the helper's mounts were proven for.
	signature string
}

// Cleanup removes the helper and its validator. retry keeps trying until
// Docker confirms removal, for work that may still be writing.
func (a *Access) Cleanup(retry bool) error {
	a.cleanupOnce.Do(func() {
		defer a.proof.Cleanup()
		if retry {
			a.service.Docker.RemoveOperationHelper(a.container)
			return
		}
		a.cleanupErr = a.service.Docker.RemoveHelper(context.Background(), a.container)
	})
	return a.cleanupErr
}

func (a *Access) request(ctx context.Context, request helper.FileRequest, input io.Reader, trailer []byte) ([]byte, error) {
	request.Blocked, request.Mounts = a.blocked, a.mounts
	return launch.Exec(ctx, a.service.Docker, a.container, launch.ExecOptions{Cmd: helper.Command("file", request),
		Input: input, Trailer: trailer, AssertAccess: a.assertAccess})
}

// signature is what must not change while file access is prepared.
func signature(info *docker.ContainerInspect) string {
	labels := make([][2]string, 0, len(info.Config.Labels))
	for key, value := range info.Config.Labels {
		labels = append(labels, [2]string{key, value})
	}
	sort.Slice(labels, func(i, j int) bool { return labels[i][0] < labels[j][0] })
	mounts := slices.Clone(info.Mounts)
	sort.SliceStable(mounts, func(i, j int) bool { return mounts[i].Destination < mounts[j].Destination })
	encoded, _ := json.Marshal([]any{info.ID, info.Name, info.Config.Image, labels, mounts})
	return string(encoded)
}

// OwnerMount is the deepest mount containing rootPath.
func OwnerMount(rootPath string, mounts []identity.Mount) *identity.Mount {
	var owner *identity.Mount
	for index := range mounts {
		mount := &mounts[index]
		if Within(rootPath, normalizedMountPath(mount.Destination)) &&
			(owner == nil || textutil.Length(mount.Destination) > textutil.Length(owner.Destination)) {
			owner = mount
		}
	}
	return owner
}

func (s *Service) managed(ctx context.Context, info *docker.ContainerInspect) bool {
	if s.Managed != nil {
		return s.Managed(ctx, info)
	}
	return games.Evaluate(info.Config.Image, info.Config.Labels).Eligible
}

// Acquire starts a helper with only the root's own mount and its safe
// nested mounts, read-only when readOnly. Unsafe nested mounts are blocked.
// After the helper's mounts are pinned it proves the game still owns the
// exact binding that was inspected, since an external manager could have
// replaced it and reused its volume names meanwhile.
func (s *Service) Acquire(ctx context.Context, target Target, root Root, readOnly bool, assertAccess func() error) (*Access, error) {
	inspection, err := s.Docker.Inspect(ctx, target.ContainerID)
	if err != nil {
		return nil, err
	}
	if inspection.ID != target.ContainerID || strings.TrimPrefix(inspection.Name, "/") != target.Name ||
		!s.managed(ctx, inspection) {
		return nil, Error("FILE_TARGET_CHANGED", 409)
	}
	mounts := identity.DockerMounts(inspection.Mounts)
	fresh := Roots(target.GameType, inspection.Config.Labels, mounts)
	if !slices.ContainsFunc(fresh, func(candidate Root) bool { return candidate.Path == root.Path }) {
		return nil, Error("FILE_ROOT_CHANGED", 409)
	}
	owner := OwnerMount(root.Path, mounts)
	if owner == nil || !SafeWritableDataMount(*owner) {
		return nil, Error("FILE_ROOT_CHANGED", 409)
	}
	var children []identity.Mount
	for _, mount := range mounts {
		if mount != *owner && Within(normalizedMountPath(mount.Destination), root.Path) {
			children = append(children, mount)
		}
	}
	var blocked []string
	for _, mount := range children {
		if !SafeWritableDataMount(mount) {
			blocked = append(blocked, normalizedMountPath(mount.Destination))
		}
	}
	selected := []identity.Mount{*owner}
	var nested []string
	for _, mount := range children {
		destination := normalizedMountPath(mount.Destination)
		if SafeWritableDataMount(mount) && !slices.ContainsFunc(blocked, func(b string) bool { return Within(destination, b) }) {
			selected = append(selected, mount)
			nested = append(nested, destination)
		}
	}
	var projections []docker.MountSettings
	var proofMounts []identity.Mount
	for _, mount := range selected {
		source := mount.Source
		if mount.Type == "volume" {
			if mount.Name != "" {
				source = mount.Name
			}
			if strings.Contains(source, "/") {
				return nil, Error("UNIDENTIFIED_VOLUME", 409)
			}
		}
		projection := docker.MountSettings{Type: mount.Type, Source: source, Target: normalizedMountPath(mount.Destination), ReadOnly: readOnly}
		if mount.Type == "bind" {
			projection.BindOptions = &docker.BindOptions{Propagation: "rprivate", NonRecursive: true}
		}
		projections = append(projections, projection)
		proofMounts = append(proofMounts, identity.Mount{Type: mount.Type, Source: mount.Source, Destination: mount.Destination, Name: mount.Name})
	}
	image, err := s.Images.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	proof, err := launch.MountProof(ctx, s.Docker, s.Images, proofMounts, "")
	if err != nil {
		return nil, err
	}
	capabilities := []string{"DAC_OVERRIDE", "CHOWN"}
	if readOnly {
		capabilities = []string{"DAC_OVERRIDE"}
	}
	id, err := s.Docker.CreateHelper(ctx, docker.ContainerCreate{
		Image: image, Entrypoint: []string{helper.Binary}, Cmd: []string{"helper", "idle", "2100000"}, User: "0",
		Labels: map[string]string{"ludock.enable": "false", "ludock.internal": "file-helper"}, Env: launch.HelperEnv,
		HostConfig: &docker.HostConfig{Mounts: projections, NetworkMode: "none", ReadonlyRootfs: true, AutoRemove: true,
			CapDrop: []string{"ALL"}, CapAdd: capabilities, SecurityOpt: []string{"no-new-privileges"},
			PidsLimit: 64, Memory: 256 * 1024 * 1024, NanoCpus: 1_000_000_000, LogConfig: launch.NoLogs},
	})
	if err != nil {
		proof.Cleanup()
		return nil, err
	}
	access := &Access{service: s, container: id, blocked: blocked, mounts: nested, assertAccess: assertAccess, proof: proof}
	if err := s.prepare(ctx, access, root, inspection); err != nil {
		access.Cleanup(false)
		return nil, err
	}
	access.signature = signature(inspection)
	return access, nil
}

// acquireLister reuses an idle read-only helper for the root when the game
// container is still exactly the one its mounts were proven for, or
// acquires a new one. release returns it for reuse after a success.
func (s *Service) acquireLister(ctx context.Context, target Target, root Root, assertAccess func() error) (*Access, func(ok bool), error) {
	key := target.ContainerID + "\x00" + root.Path
	s.mu.Lock()
	idle := s.listers[key]
	delete(s.listers, key)
	s.mu.Unlock()
	if idle != nil {
		// Taken from the map, the helper is this caller's to reuse or remove.
		if idle.timer.Stop() && s.reusable(ctx, target, root, idle.access) {
			idle.access.assertAccess = assertAccess
			return idle.access, s.releaser(key, idle.access), nil
		}
		idle.access.Cleanup(false)
	}
	access, err := s.Acquire(ctx, target, root, true, assertAccess)
	if err != nil {
		return nil, nil, err
	}
	return access, s.releaser(key, access), nil
}

func (s *Service) reusable(ctx context.Context, target Target, root Root, access *Access) bool {
	inspection, err := s.Docker.Inspect(ctx, target.ContainerID)
	if err != nil || inspection.ID != target.ContainerID || strings.TrimPrefix(inspection.Name, "/") != target.Name ||
		!s.managed(ctx, inspection) || signature(inspection) != access.signature {
		return false
	}
	fresh := Roots(target.GameType, inspection.Config.Labels, identity.DockerMounts(inspection.Mounts))
	if !slices.ContainsFunc(fresh, func(candidate Root) bool { return candidate.Path == root.Path }) {
		return false
	}
	helper, err := s.Docker.Inspect(ctx, access.container)
	return err == nil && helper.State.Running
}

func (s *Service) releaser(key string, access *Access) func(ok bool) {
	return func(ok bool) {
		access.assertAccess = nil
		s.mu.Lock()
		if !ok || s.listers[key] != nil {
			s.mu.Unlock()
			access.Cleanup(false)
			return
		}
		if s.listers == nil {
			s.listers = map[string]*lister{}
		}
		entry := &lister{access: access}
		idle := s.ListerIdle
		if idle <= 0 {
			idle = listerIdle
		}
		entry.timer = time.AfterFunc(idle, func() {
			s.mu.Lock()
			current := s.listers[key] == entry
			if current {
				delete(s.listers, key)
			}
			s.mu.Unlock()
			if current {
				access.Cleanup(false)
			}
		})
		s.listers[key] = entry
		s.mu.Unlock()
	}
}

func (s *Service) Close() {
	s.mu.Lock()
	listers := s.listers
	s.listers = nil
	s.mu.Unlock()
	for _, entry := range listers {
		entry.timer.Stop()
		entry.access.Cleanup(false)
	}
}

// RemoveLeftovers removes file helpers left by an earlier process, which may
// still be writing, and their mount validators, which can read the whole host.
// One Ludock runs per Docker host, so at startup every one is left over.
// Startup keeps the lock fence until this succeeds.
func (s *Service) RemoveLeftovers(ctx context.Context) error {
	for _, kind := range []string{"file-helper", "mount-validator"} {
		helpers, err := s.Docker.ListContainers(ctx, true, docker.Filters{"label": {"ludock.internal=" + kind}})
		if err != nil {
			return err
		}
		for _, candidate := range helpers {
			if candidate.Labels["ludock.internal"] != kind {
				continue
			}
			if err := s.Docker.RemoveHelper(ctx, candidate.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) prepare(ctx context.Context, access *Access, root Root, inspection *docker.ContainerInspect) error {
	if err := s.Docker.Start(ctx, access.container); err != nil {
		return err
	}
	if err := launch.AssertMountIdentities(ctx, s.Docker, access.container, access.proof.Identities); err != nil {
		return err
	}
	if _, err := access.request(ctx, helper.FileRequest{Operation: "check", Root: root.Path}, nil, nil); err != nil {
		return err
	}
	fresh, err := s.Docker.Inspect(ctx, inspection.ID)
	if err != nil || signature(fresh) != signature(inspection) {
		return Error("FILE_TARGET_CHANGED", 409)
	}
	return nil
}

type Listing struct {
	Root    Root
	Path    string
	Entries []helper.FileEntry
}

// List lists a directory in the helper's order; callers sort what they show.
func (s *Service) List(ctx context.Context, target Target, rootID, relative string, assertAccess func() error) (*Listing, error) {
	root, normalized, err := resolve(target, rootID, relative)
	if err != nil {
		return nil, err
	}
	access, release, err := s.acquireLister(ctx, target, root, assertAccess)
	if err != nil {
		return nil, err
	}
	out, err := access.request(ctx, helper.FileRequest{Operation: "list", Root: root.Path, Path: normalized}, nil, nil)
	release(err == nil)
	if err != nil {
		return nil, err
	}
	if assertAccess != nil {
		if err := assertAccess(); err != nil {
			return nil, err
		}
	}
	var entries []helper.FileEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, Error("FILE_OPERATION_FAILED", 409)
	}
	return &Listing{Root: root, Path: normalized, Entries: entries}, nil
}

// Space measures the filesystem under a root, reusing an idle listing
// helper when one is kept for it.
func (s *Service) Space(ctx context.Context, target Target, rootID string, assertAccess func() error) (*helper.FileSpace, error) {
	root, _, err := resolve(target, rootID, "")
	if err != nil {
		return nil, err
	}
	access, release, err := s.acquireLister(ctx, target, root, assertAccess)
	if err != nil {
		return nil, err
	}
	out, err := access.request(ctx, helper.FileRequest{Operation: "space", Root: root.Path}, nil, nil)
	release(err == nil)
	if err != nil {
		return nil, err
	}
	var space helper.FileSpace
	if err := json.Unmarshal(out, &space); err != nil || space.Total <= 0 {
		return nil, Error("FILE_OPERATION_FAILED", 409)
	}
	return &space, nil
}

// Mkdir creates a folder owned like its parent.
func (s *Service) Mkdir(ctx context.Context, target Target, rootID, parent, name string, assertAccess func() error) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	root, normalized, err := resolve(target, rootID, joinRelative(parent, name))
	if err != nil {
		return err
	}
	access, err := s.Acquire(ctx, target, root, false, assertAccess)
	if err != nil {
		return err
	}
	_, err = access.request(ctx, helper.FileRequest{Operation: "mkdir", Root: root.Path, Path: normalized}, nil, nil)
	access.Cleanup(err != nil)
	return err
}

// Delete removes a file or folder. A failed change, here and in Mkdir and
// Rename, may still be running when Docker is unavailable, so its helper is
// removed before returning.
func (s *Service) Delete(ctx context.Context, target Target, rootID, relative string, assertAccess func() error) error {
	root, normalized, err := resolve(target, rootID, relative)
	if err != nil {
		return err
	}
	if normalized == "" {
		return Error("ROOT_MUTATION", 400)
	}
	access, err := s.Acquire(ctx, target, root, false, assertAccess)
	if err != nil {
		return err
	}
	_, err = access.request(ctx, helper.FileRequest{Operation: "delete", Root: root.Path, Path: normalized}, nil, nil)
	access.Cleanup(err != nil)
	return err
}

// Rename renames an entry within its folder.
func (s *Service) Rename(ctx context.Context, target Target, rootID, relative, newName string, assertAccess func() error) error {
	if err := ValidateName(newName); err != nil {
		return err
	}
	root, normalized, err := resolve(target, rootID, relative)
	if err != nil {
		return err
	}
	if normalized == "" {
		return Error("ROOT_MUTATION", 400)
	}
	access, err := s.Acquire(ctx, target, root, false, assertAccess)
	if err != nil {
		return err
	}
	dir := path.Dir(normalized)
	if dir == "." {
		dir = ""
	}
	_, err = access.request(ctx, helper.FileRequest{Operation: "rename", Root: root.Path, Path: normalized,
		Destination: joinRelative(dir, newName)}, nil, nil)
	access.Cleanup(err != nil)
	return err
}

// Upload writes a file of exactly size bytes. The helper commits only after
// the upload ID trailer, which is sent after the complete body and the
// last access check. Staging cleanup has no user-controlled input and runs
// even after cancellation or revocation.
func (s *Service) Upload(ctx context.Context, target Target, rootID, parent, name string, size int64, body io.Reader, assertAccess func() error) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if size < 0 || size > 1<<53-1 {
		return Error("INVALID_UPLOAD_SIZE", 400)
	}
	root, normalized, err := resolve(target, rootID, joinRelative(parent, name))
	if err != nil {
		return err
	}
	return s.write(ctx, target, root, normalized, size, body, nil, true, assertAccess)
}

// Save replaces an existing file's content only while it still matches
// version, the Version its preview reported.
func (s *Service) Save(ctx context.Context, target Target, rootID, relative string, content []byte, version string, assertAccess func() error) error {
	expected, ok := parseVersion(version)
	if !ok {
		return Error("INVALID_FILE_VERSION", 400)
	}
	root, normalized, err := resolve(target, rootID, relative)
	if err != nil {
		return err
	}
	if normalized == "" {
		return Error("ROOT_MUTATION", 400)
	}
	return s.write(ctx, target, root, normalized, int64(len(content)), bytes.NewReader(content), &expected, false, assertAccess)
}

// UploadSpare is the free space an upload leaves on the server's disk: 1 GiB,
// or a twentieth of a smaller disk, so the game can still save.
func UploadSpare(total int64) int64 { return min(1<<30, total/20) }

func (s *Service) write(ctx context.Context, target Target, root Root, normalized string, size int64, body io.Reader,
	expected *helper.FileVersion, checkSpace bool, assertAccess func() error) error {
	access, err := s.Acquire(ctx, target, root, false, assertAccess)
	if err != nil {
		return err
	}
	if checkSpace {
		// Uploads have no size limit by default, so one that would fill the
		// disk is refused before anything is written.
		folder := path.Dir(normalized)
		if folder == "." {
			folder = ""
		}
		out, err := access.request(ctx, helper.FileRequest{Operation: "space", Root: root.Path, Path: folder}, nil, nil)
		var space helper.FileSpace
		if err == nil && (json.Unmarshal(out, &space) != nil || space.Total <= 0) {
			err = Error("FILE_OPERATION_FAILED", 409)
		}
		if err == nil && size > space.Available-UploadSpare(space.Total) {
			err = apperr.New("NOT_ENOUGH_SPACE", 507, fmt.Sprintf("Not enough free space: the file needs %s, and %s is free on the server’s disk, of which Ludock leaves %s for the game.",
				datasize.Format(size), datasize.Format(space.Available), datasize.Format(UploadSpare(space.Total))))
		}
		if err != nil {
			access.Cleanup(false)
			return err
		}
	}
	uploadID := identity.NewUUID()
	_, err = access.request(ctx, helper.FileRequest{Operation: "upload", Root: root.Path, Path: normalized, Size: &size,
		UploadID: uploadID, Expected: expected}, io.LimitReader(body, size+1), []byte(uploadID))
	// A helper that finished removed its own staging file; one cut off may not
	// have, so only a failed upload sends the cleanup request.
	if err != nil {
		cleanupAccess := &Access{service: s, container: access.container, blocked: access.blocked}
		if _, cleanupErr := cleanupAccess.request(context.WithoutCancel(ctx), helper.FileRequest{Operation: "upload-cleanup",
			Root: root.Path, Path: normalized, UploadID: uploadID}, nil, nil); cleanupErr != nil {
			log.Warn("Failed to clean up an upload temporary file", logging.F("container", target.ContainerID[:min(12, len(target.ContainerID))]))
		}
	}
	// A failed upload may still be writing, so its helper is removed before
	// the locks are released, as for Delete.
	access.Cleanup(err != nil)
	return err
}

// Download is an open download stream. Read returns an error instead of
// EOF if the helper did not finish cleanly; Close removes the helper.
type Download struct {
	Name string
	Type string
	Size int64

	reader   io.ReadCloser
	conn     *docker.Conn
	complete func() error
	cleanup  func()
	once     sync.Once
	timer    *time.Timer
	// unwatch stops closing the stream when the caller's context ends.
	unwatch func() bool
	done    bool
}

func (d *Download) Read(p []byte) (int, error) {
	n, err := d.reader.Read(p)
	if err == io.EOF && !d.done {
		d.done = true
		if completeErr := d.complete(); completeErr != nil {
			return n, completeErr
		}
	}
	if err != nil && err != io.EOF {
		if errors.Is(err, docker.ErrIncompleteStream) {
			return n, Error("INCOMPLETE_DOWNLOAD_STREAM", 409)
		}
		if _, ok := apperr.As(err); !ok {
			return n, Error("FILE_DOWNLOAD_FAILED", 409)
		}
	}
	return n, err
}

func (d *Download) Close() error {
	d.once.Do(func() {
		d.unwatch()
		d.timer.Stop()
		d.conn.Close()
		d.reader.Close()
		d.cleanup()
	})
	return nil
}

// OpenDownload streams a file, or a folder as tar.
func (s *Service) OpenDownload(ctx context.Context, target Target, rootID, relative string, assertAccess func() error) (*Download, error) {
	root, normalized, err := resolve(target, rootID, relative)
	if err != nil {
		return nil, err
	}
	if normalized == "" {
		return nil, Error("ROOT_DOWNLOAD", 400)
	}
	access, err := s.Acquire(ctx, target, root, true, assertAccess)
	if err != nil {
		return nil, err
	}
	name := path.Base(normalized)
	download, err := s.openDownload(ctx, access, helper.FileRequest{Operation: "download", Root: root.Path, Path: normalized}, name, assertAccess, target)
	if err != nil {
		access.Cleanup(false)
		return nil, err
	}
	return download, nil
}

// openDownload starts a download request; a folder's name gains .tar.
func (s *Service) openDownload(ctx context.Context, access *Access, request helper.FileRequest, name string, assertAccess func() error, target Target) (*Download, error) {
	request.Blocked = access.blocked
	execID, err := s.Docker.CreateExec(ctx, access.container, docker.ExecCreate{Cmd: helper.Command("file", request), AttachStdout: true, AttachStderr: true})
	if err != nil {
		return nil, err
	}
	if assertAccess != nil {
		if err := assertAccess(); err != nil {
			return nil, err
		}
	}
	opening, cancel := context.WithTimeout(ctx, time.Minute)
	conn, err := s.Docker.StartExec(opening, execID)
	cancel()
	if err != nil {
		return nil, err
	}
	// The helper sends the opened file's type and size first. Until the
	// stream is handed over, revocation or a silent helper ends the wait.
	stdout := docker.Stdout(conn)
	stream := bufio.NewReaderSize(stdout, 64*1024)
	stopWaiting := context.AfterFunc(ctx, func() { conn.Close() })
	waiting := time.AfterFunc(time.Minute, func() { conn.Close() })
	line, err := stream.ReadSlice('\n')
	stopWaiting()
	waiting.Stop()
	var info struct {
		Type string `json:"type"`
		Size int64  `json:"size"`
	}
	if err != nil || json.Unmarshal(line, &info) != nil || (info.Type != "file" && info.Type != "directory") {
		conn.Close()
		return nil, Error("UNSAFE_OR_CHANGED_FILE_PATH", 409)
	}
	if info.Type == "directory" {
		name += ".tar"
	}
	download := &Download{Name: name, Type: info.Type, Size: info.Size, reader: bufferedStream{stream, stdout}, conn: conn,
		complete: func() error {
			status, err := s.Docker.InspectExec(context.WithoutCancel(ctx), execID)
			if err != nil || status.Running || status.ExitCode == nil || *status.ExitCode != 0 {
				return Error("UNSAFE_OR_CHANGED_FILE_PATH", 409)
			}
			return nil
		},
		cleanup: func() {
			if err := access.Cleanup(false); err != nil {
				log.Warn("Failed to remove download helper", logging.F("container", target.ContainerID[:min(12, len(target.ContainerID))]))
			}
		}}
	download.timer = time.AfterFunc(30*time.Minute, func() { conn.Close() })
	// The started stream no longer follows ctx, so revoked access or a closed
	// request must end it explicitly.
	download.unwatch = context.AfterFunc(ctx, func() { conn.Close() })
	return download, nil
}

type bufferedStream struct {
	*bufio.Reader
	io.Closer
}

type PreviewKind string

const (
	PreviewText   PreviewKind = "text"
	PreviewLarge  PreviewKind = "large"
	PreviewBinary PreviewKind = "binary"
)

type Preview struct {
	Kind PreviewKind
	Text string
	Name string
	Size int64
	// Version identifies the previewed bytes for Save.
	Version string
	// BOM and CRLF record what the text drops or a browser rewrites, so a
	// save writes them back. Editable is false when the line endings are
	// mixed or include lone carriage returns, which a browser can't keep.
	BOM, CRLF, Editable bool
}

func parseVersion(value string) (helper.FileVersion, bool) {
	size, sum, ok := strings.Cut(value, "-")
	parsed, err := strconv.ParseInt(size, 10, 64)
	if !ok || err != nil || parsed < 0 || parsed > PreviewLimit || len(sum) != 64 || strings.Trim(sum, "0123456789abcdef") != "" {
		return helper.FileVersion{}, false
	}
	return helper.FileVersion{Size: parsed, SHA256: sum}, true
}

// ReadPreview reads at most PreviewLimit bytes. A file over the limit (even
// one that grew after listing) is "large"; NUL bytes or invalid UTF-8 make it
// "binary".
func ReadPreview(source io.Reader) (Preview, error) {
	data, err := io.ReadAll(io.LimitReader(source, PreviewLimit+1))
	if err != nil {
		return Preview{}, err
	}
	if len(data) > PreviewLimit {
		return Preview{Kind: PreviewLarge}, nil
	}
	if slices.Contains(data, 0) || !utf8.Valid(data) {
		return Preview{Kind: PreviewBinary}, nil
	}
	text := string(data)
	crlf := strings.Count(text, "\r\n")
	preview := Preview{Kind: PreviewText, Version: fmt.Sprintf("%d-%x", len(data), sha256.Sum256(data)),
		BOM: strings.HasPrefix(text, "\ufeff"), CRLF: crlf > 0,
		Editable: strings.Count(text, "\r") == crlf && (crlf == 0 || strings.Count(text, "\n") == crlf)}
	preview.Text = strings.TrimPrefix(text, "\ufeff")
	return preview, nil
}

// EditedContent turns text submitted from the editor back into the file's
// bytes: browsers submit every line break as CRLF, so breaks are rewritten
// in the file's own style and a leading BOM is restored.
func EditedContent(text string, crlf, bom bool) []byte {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	if crlf {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	if bom {
		text = "\ufeff" + text
	}
	return []byte(text)
}

// Preview opens a file for inline display without reading past the limit.
func (s *Service) Preview(ctx context.Context, target Target, rootID, relative string, assertAccess func() error) (*Preview, error) {
	download, err := s.OpenDownload(ctx, target, rootID, relative, assertAccess)
	if err != nil {
		return nil, err
	}
	defer download.Close()
	if download.Type != "file" {
		return nil, Error("UNSAFE_OR_CHANGED_FILE_PATH", 409)
	}
	preview := Preview{Kind: PreviewLarge}
	if download.Size <= PreviewLimit {
		if preview, err = ReadPreview(download); err != nil {
			return nil, err
		}
	}
	preview.Name, preview.Size = download.Name, download.Size
	return &preview, nil
}
