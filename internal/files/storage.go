package files

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Crunging/Ludock/internal/apperr"
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
	"FILE_OPERATION_TIMEOUT":      "The file operation timed out. Refresh and try again.",
}

// Error builds a file error with its public message.
func Error(code string, status int) *apperr.Error {
	message, ok := messages[code]
	if !ok {
		message = "File operation failed"
	}
	return apperr.New(code, status, message)
}

// PreviewLimit is the largest file shown inline; larger or binary files are
// downloaded instead.
const PreviewLimit = 512 * 1024

// Target is the server whose files are accessed.
type Target struct {
	ContainerID string
	Name        string
	GameType    string
	Roots       []Root
}

// Service accesses server files through helper containers.
type Service struct {
	Docker *docker.Client
	Images *launch.Images
}

// NormalizeRelativePath validates a path relative to a root.
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
	assertAccess func() error
	cleanupOnce  sync.Once
	cleanupErr   error
	proof        *launch.Proof
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
	request.Blocked = a.blocked
	return launch.Exec(ctx, a.service.Docker, a.container, launch.ExecOptions{Cmd: helper.Command("file", request),
		Input: input, Trailer: trailer, AssertAccess: a.assertAccess})
}

func toMounts(points []docker.MountPoint) []Mount {
	var mounts []Mount
	for _, point := range points {
		mounts = append(mounts, Mount{Type: point.Type, Source: point.Source, Destination: point.Destination, RW: point.RW, Name: point.Name})
	}
	return mounts
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
func OwnerMount(rootPath string, mounts []Mount) *Mount {
	var owner *Mount
	for index := range mounts {
		mount := &mounts[index]
		if Within(rootPath, normalizedMountPath(mount.Destination)) &&
			(owner == nil || textutil.Length(mount.Destination) > textutil.Length(owner.Destination)) {
			owner = mount
		}
	}
	return owner
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
		!games.Evaluate(inspection.Config.Image, inspection.Config.Labels).Eligible {
		return nil, Error("FILE_TARGET_CHANGED", 409)
	}
	mounts := toMounts(inspection.Mounts)
	fresh := Roots(target.GameType, inspection.Config.Labels, mounts)
	if !slices.ContainsFunc(fresh, func(candidate Root) bool { return candidate.Path == root.Path }) {
		return nil, Error("FILE_ROOT_CHANGED", 409)
	}
	owner := OwnerMount(root.Path, mounts)
	if owner == nil || !SafeWritableDataMount(*owner) {
		return nil, Error("FILE_ROOT_CHANGED", 409)
	}
	var children []Mount
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
	selected := []Mount{*owner}
	for _, mount := range children {
		destination := normalizedMountPath(mount.Destination)
		if SafeWritableDataMount(mount) && !slices.ContainsFunc(blocked, func(b string) bool { return Within(destination, b) }) {
			selected = append(selected, mount)
		}
	}
	var projections []docker.MountSettings
	var proofMounts []launch.Mount
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
		proofMounts = append(proofMounts, launch.Mount{Type: mount.Type, Source: mount.Source, Destination: mount.Destination, Name: mount.Name})
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
			PidsLimit: 64, Memory: 256 * 1024 * 1024, NanoCpus: 1_000_000_000},
	})
	if err != nil {
		proof.Cleanup()
		return nil, err
	}
	access := &Access{service: s, container: id, blocked: blocked, assertAccess: assertAccess, proof: proof}
	if err := s.prepare(ctx, access, root, inspection); err != nil {
		access.Cleanup(false)
		return nil, err
	}
	return access, nil
}

// RemoveLeftovers removes file helpers left by an earlier process: one
// interrupted mid-command may still be writing. One Ludock runs per Docker
// host, so at startup every file helper is left over. Startup keeps the lock
// fence until this succeeds.
func (s *Service) RemoveLeftovers(ctx context.Context) error {
	helpers, err := s.Docker.ListContainers(ctx, true, docker.Filters{"label": {"ludock.internal=file-helper"}})
	if err != nil {
		return err
	}
	for _, candidate := range helpers {
		if candidate.Labels["ludock.internal"] != "file-helper" {
			continue
		}
		if err := s.Docker.RemoveHelper(ctx, candidate.ID); err != nil {
			return err
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

// Listing is a directory listing.
type Listing struct {
	Root    Root
	Path    string
	Entries []helper.FileEntry
}

// List lists a directory: folders first, then names ignoring case and
// accents.
func (s *Service) List(ctx context.Context, target Target, rootID, relative string, assertAccess func() error) (*Listing, error) {
	root, normalized, err := resolve(target, rootID, relative)
	if err != nil {
		return nil, err
	}
	access, err := s.Acquire(ctx, target, root, true, assertAccess)
	if err != nil {
		return nil, err
	}
	defer access.Cleanup(false)
	out, err := access.request(ctx, helper.FileRequest{Operation: "list", Root: root.Path, Path: normalized}, nil, nil)
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
	sort.SliceStable(entries, func(i, j int) bool {
		left, right := entries[i].Type == "directory", entries[j].Type == "directory"
		if left != right {
			return left
		}
		return textutil.CompareFold(entries[i].Name, entries[j].Name) < 0
	})
	return &Listing{Root: root, Path: normalized, Entries: entries}, nil
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
	access, err := s.Acquire(ctx, target, root, false, assertAccess)
	if err != nil {
		return err
	}
	uploadID := identity.NewUUID()
	_, err = access.request(ctx, helper.FileRequest{Operation: "upload", Root: root.Path, Path: normalized, Size: &size,
		UploadID: uploadID}, io.LimitReader(body, size+1), []byte(uploadID))
	cleanupAccess := &Access{service: s, container: access.container, blocked: access.blocked}
	if _, cleanupErr := cleanupAccess.request(context.WithoutCancel(ctx), helper.FileRequest{Operation: "upload-cleanup",
		Root: root.Path, Path: normalized, UploadID: uploadID}, nil, nil); cleanupErr != nil {
		log.Warn("Failed to clean up an upload temporary file", logging.F("container", target.ContainerID[:min(12, len(target.ContainerID))]))
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

// Close ends the stream and removes the helper.
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
	download, err := s.openDownload(ctx, access, root, normalized, assertAccess, target)
	if err != nil {
		access.Cleanup(false)
		return nil, err
	}
	return download, nil
}

func (s *Service) openDownload(ctx context.Context, access *Access, root Root, normalized string, assertAccess func() error, target Target) (*Download, error) {
	request := helper.FileRequest{Operation: "download", Root: root.Path, Path: normalized, Blocked: access.blocked}
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
	name := path.Base(normalized)
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

// bufferedStream reads a stream through its buffer and closes the stream.
type bufferedStream struct {
	*bufio.Reader
	io.Closer
}

// PreviewKind says how a file can be shown.
type PreviewKind string

const (
	PreviewText   PreviewKind = "text"
	PreviewLarge  PreviewKind = "large"
	PreviewBinary PreviewKind = "binary"
)

// Preview is a file's inline preview.
type Preview struct {
	Kind PreviewKind
	Text string
	Name string
	Size int64
}

// ReadPreview reads at most PreviewLimit bytes. A file over the limit (even
// one that grew after listing) is "large"; NUL bytes or invalid UTF-8 make it
// "binary".
func ReadPreview(source io.Reader) (PreviewKind, string, error) {
	data, err := io.ReadAll(io.LimitReader(source, PreviewLimit+1))
	if err != nil {
		return "", "", err
	}
	if len(data) > PreviewLimit {
		return PreviewLarge, "", nil
	}
	if slices.Contains(data, 0) || !utf8.Valid(data) {
		return PreviewBinary, "", nil
	}
	return PreviewText, strings.TrimPrefix(string(data), "\ufeff"), nil
}

// Preview opens a file for inline display without reading past the limit.
func (s *Service) Preview(ctx context.Context, target Target, rootID, relative string, assertAccess func() error) (*Preview, error) {
	download, err := s.OpenDownload(ctx, target, rootID, relative, assertAccess)
	if err != nil {
		return nil, err
	}
	defer download.Close()
	preview := &Preview{Name: download.Name, Size: download.Size}
	if download.Type != "file" {
		return nil, Error("UNSAFE_OR_CHANGED_FILE_PATH", 409)
	}
	if download.Size > PreviewLimit {
		preview.Kind = PreviewLarge
		return preview, nil
	}
	preview.Kind, preview.Text, err = ReadPreview(download)
	return preview, err
}
