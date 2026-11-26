package files

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/self"
	"golang.org/x/sys/unix"
)

var ctx = context.Background()

const image = "ghcr.io/crunging/ludock@sha256:abababababababababababababababababababababababababababababababab"

type fixture struct {
	engine  *fakedocker.Engine
	service *Service
	target  Target
	data    string
	game    string
	root    string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	engine.RunHelpers(nil)
	helper.HostRoot = "/"
	t.Cleanup(func() { helper.HostRoot = "/host" })
	engine.AddImage(image)
	data := filepath.Join(t.TempDir(), "data")
	os.MkdirAll(filepath.Join(data, "World"), 0o755)
	os.MkdirAll(filepath.Join(data, "alpha"), 0o755)
	os.WriteFile(filepath.Join(data, "server.properties"), []byte("motd=hello\n"), 0o644)
	os.WriteFile(filepath.Join(data, "Banned.json"), []byte("[]"), 0o644)
	os.WriteFile(filepath.Join(data, "level.dat"), []byte{0x1f, 0x8b, 0, 1}, 0o644)
	os.WriteFile(filepath.Join(data, "big.log"), bytes.Repeat([]byte("x"), PreviewLimit+1), 0o644)
	game := engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true,
		Mounts: []docker.MountPoint{{Type: "bind", Source: data, Destination: data, RW: true}}})
	client := docker.New(engine.Socket)
	service := &Service{Docker: client, Images: &launch.Images{Configured: image}, Own: self.New(nil, "")}
	roots := Roots("minecraft", nil, []identity.Mount{{Type: "bind", Source: data, Destination: data, Writable: true}}, notInContainer)
	if len(roots) != 1 {
		t.Fatal(roots)
	}
	return &fixture{engine: engine, service: service, target: Target{ContainerID: game, Name: "mc", GameType: "minecraft", Roots: roots}, data: data, game: game, root: roots[0].ID}
}

func (f *fixture) helpersLeft(t *testing.T) {
	t.Helper()
	for _, id := range f.engine.SortedIDs() {
		if c := f.engine.Container(id); c != nil && c.Config.Labels["ludock.internal"] != "" {
			t.Fatalf("helper %s left behind", c.Config.Labels["ludock.internal"])
		}
	}
}

func TestPreviewCapAndBinaryDetection(t *testing.T) {
	f := setup(t)
	for name, want := range map[string]PreviewKind{"server.properties": PreviewText, "level.dat": PreviewBinary, "big.log": PreviewLarge} {
		preview, err := f.service.Preview(ctx, f.target, f.root, name, nil)
		if err != nil || preview.Kind != want {
			t.Fatalf("%s: %+v %v", name, preview, err)
		}
		if want == PreviewText && preview.Text != "motd=hello\n" {
			t.Fatal(preview.Text)
		}
	}
	// A file that grew past the limit after listing is still cut off.
	preview, _ := ReadPreview(io.LimitReader(zeroFree{}, PreviewLimit+10))
	if preview.Kind != PreviewLarge {
		t.Fatal(preview.Kind)
	}
	if preview, _ := ReadPreview(strings.NewReader("\xff\xfe")); preview.Kind != PreviewBinary {
		t.Fatal("invalid UTF-8 shown as text")
	}
	// A preview read to its end leaves its helper idle for the next request.
	f.service.Close()
	f.helpersLeft(t)
}

// Editor text arrives with LF or CRLF breaks and no BOM; a save writes the
// file's own breaks and BOM back, and files whose breaks a browser can't
// keep are not editable.
func TestEditedContentKeepsLineEndings(t *testing.T) {
	for _, test := range []struct {
		file, typed, want string
		editable          bool
	}{
		{"a\nb", "a\r\nb\r\nc", "a\nb\nc", true},
		{"a\r\nb\r\n", "a\nb\nc", "a\r\nb\r\nc", true},
		{"a\r\nb\r\n", "a\r\nb\r\nc\r\n", "a\r\nb\r\nc\r\n", true},
		{"\ufeffkey=1\r\n", "key=2\n", "\ufeffkey=2\r\n", true},
		{"\ufeff\ufeffx", "\ufeffy", "\ufeff\ufeffy", true},
		{"", "new", "new", true},
		{"a\r\nb\n", "", "", false},
		{"a\rb", "", "", false},
	} {
		preview, err := ReadPreview(strings.NewReader(test.file))
		if err != nil || preview.Kind != PreviewText || preview.Editable != test.editable {
			t.Fatalf("%q: %+v %v", test.file, preview, err)
		}
		if !test.editable {
			continue
		}
		if got := string(EditedContent(test.typed, preview.CRLF, preview.BOM)); got != test.want {
			t.Fatalf("%q typed %q: got %q", test.file, test.typed, got)
		}
	}
}

// A save checks the content it replaces: a change made elsewhere after the
// preview, even one keeping the size, refuses the save and keeps that
// change; a current save keeps the file's mode.
func TestSaveRefusesChangedFile(t *testing.T) {
	f := setup(t)
	name := filepath.Join(f.data, "server.properties")
	os.Chmod(name, 0o640)
	preview, err := f.service.Preview(ctx, f.target, f.root, "server.properties", nil)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(name, []byte("motd=other\n"), 0o640)
	if err := f.service.Save(ctx, f.target, f.root, "server.properties", []byte("motd=mine\n"), preview.Version, nil); !apperr.Is(err, "FILE_CHANGED") {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(name); string(data) != "motd=other\n" {
		t.Fatal(string(data))
	}
	preview, _ = f.service.Preview(ctx, f.target, f.root, "server.properties", nil)
	if err := f.service.Save(ctx, f.target, f.root, "server.properties", []byte("motd=mine\n"), preview.Version, nil); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(name)
	if data, _ := os.ReadFile(name); string(data) != "motd=mine\n" || info.Mode().Perm() != 0o640 {
		t.Fatal(string(data), info.Mode())
	}
	os.Remove(name)
	if err := f.service.Save(ctx, f.target, f.root, "server.properties", []byte("x"), preview.Version, nil); !apperr.Is(err, "FILE_CHANGED") {
		t.Fatal("recreated a deleted file", err)
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	f.service.Close()
	f.helpersLeft(t)
}

type zeroFree struct{}

func (zeroFree) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func TestAccessRecheckAndTargetChanges(t *testing.T) {
	f := setup(t)
	denied := apperr.New("FORBIDDEN", 403, "Insufficient server permissions")
	if _, err := f.service.List(ctx, f.target, f.root, "", func() error { return denied }); !apperr.Is(err, "FORBIDDEN") {
		t.Fatal(err)
	}
	f.helpersLeft(t)
	// An external manager replaces the game's labels while the helper starts.
	previous := f.engine.OnStart
	f.engine.OnStart = func(c *fakedocker.Container) {
		previous(c)
		if c.CreateRequest.Labels["ludock.internal"] == "file-helper" {
			f.engine.Update(f.game, func(game *fakedocker.Container) { game.Config.Labels = map[string]string{"changed": "yes"} })
		}
	}
	if _, err := f.service.List(ctx, f.target, f.root, "", nil); !apperr.Is(err, "FILE_TARGET_CHANGED") {
		t.Fatal(err)
	}
	f.helpersLeft(t)
	f.engine.OnStart = previous
	// The root's mount became read-only.
	f.engine.Update(f.game, func(game *fakedocker.Container) { game.Mounts[0].RW = false })
	if _, err := f.service.List(ctx, f.target, f.root, "", nil); !apperr.Is(err, "FILE_ROOT_CHANGED") {
		t.Fatal(err)
	}
	f.engine.Update(f.game, func(game *fakedocker.Container) { game.Mounts[0].RW = true; game.Name = "/renamed" })
	if _, err := f.service.List(ctx, f.target, f.root, "", nil); !apperr.Is(err, "FILE_TARGET_CHANGED") {
		t.Fatal(err)
	}
}

// Listing again reuses the idle helper while the game container is
// unchanged; a changed container gets a new helper, and the old one is
// removed.
func TestListReusesHelperUntilTargetChanges(t *testing.T) {
	f := setup(t)
	started := 0
	previous := f.engine.OnStart
	f.engine.OnStart = func(c *fakedocker.Container) {
		previous(c)
		if c.CreateRequest.Labels["ludock.internal"] == "file-helper" {
			started++
		}
	}
	for range 2 {
		if _, err := f.service.List(ctx, f.target, f.root, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if started != 1 {
		t.Fatal("started", started, "helpers for two listings")
	}
	f.engine.Update(f.game, func(game *fakedocker.Container) { game.Config.Labels = map[string]string{"changed": "yes"} })
	if _, err := f.service.List(ctx, f.target, f.root, "", nil); err != nil {
		t.Fatal(err)
	}
	if started != 2 {
		t.Fatal("reused a helper proven for a different container")
	}
	helpers := 0
	for _, id := range f.engine.SortedIDs() {
		if c := f.engine.Container(id); c != nil && c.Config.Labels["ludock.internal"] == "file-helper" {
			helpers++
		}
	}
	if helpers != 1 {
		t.Fatal(helpers, "file helpers running")
	}
	f.service.Close()
	f.helpersLeft(t)
}

// Browsing a folder, previewing its files, and downloading one share one
// read-only helper and one mount proof. A download cut off partway may
// still be running in its helper, so that helper is removed, as is one
// from the game's previous run and one whose request was refused.
func TestBrowsingSharesOneHelper(t *testing.T) {
	f := setup(t)
	creates := func() int {
		count := 0
		for _, request := range f.engine.Requests {
			if strings.HasSuffix(request, "/containers/create") {
				count++
			}
		}
		return count
	}
	if _, err := f.service.List(ctx, f.target, f.root, "", nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"server.properties", "level.dat", "server.properties"} {
		if _, err := f.service.Preview(ctx, f.target, f.root, name, nil); err != nil {
			t.Fatal(name, err)
		}
	}
	download, err := f.service.OpenDownload(ctx, f.target, f.root, "server.properties", nil)
	if err != nil {
		t.Fatal(err)
	}
	if content, err := io.ReadAll(download); err != nil || string(content) != "motd=hello\n" {
		t.Fatal(string(content), err)
	}
	download.Close()
	if _, err := f.service.List(ctx, f.target, f.root, "World", nil); err != nil {
		t.Fatal(err)
	}
	// The helper and the validator that proved its mount.
	if creates() != 2 {
		t.Fatal(creates(), "helpers for one browsing session")
	}
	download, err = f.service.OpenDownload(ctx, f.target, f.root, "big.log", nil)
	if err != nil {
		t.Fatal(err)
	}
	download.Close()
	if _, err := f.service.Preview(ctx, f.target, f.root, "server.properties", nil); err != nil {
		t.Fatal(err)
	}
	if creates() != 4 {
		t.Fatal("reused the helper of a download that was cut off")
	}
	// A restart may follow a host folder's replacement, which the running
	// helper's bind mount wouldn't see.
	f.engine.Update(f.game, func(game *fakedocker.Container) { game.State.StartedAt = "2026-01-02T03:04:05Z" })
	if _, err := f.service.Preview(ctx, f.target, f.root, "server.properties", nil); err != nil {
		t.Fatal(err)
	}
	if creates() != 6 {
		t.Fatal("reused a helper from the game's previous run")
	}
	// A root inside the data mount still excludes the data mount's other
	// mounts the helper leaves out: storage that matches names
	// case-insensitively could reach them from the root's spelling.
	other := t.TempDir()
	world := f.engine.AddContainer(fakedocker.GameOptions{Name: "world", Image: "itzg/minecraft-server", Running: true,
		Labels: map[string]string{"ludock.files": f.data + "/World"},
		Mounts: []docker.MountPoint{{Type: "bind", Source: f.data, Destination: f.data, RW: true},
			{Type: "bind", Source: other, Destination: f.data + "/alpha", RW: false}}})
	inspection, _ := docker.New(f.engine.Socket).Inspect(ctx, world)
	roots := Roots("minecraft", inspection.Config.Labels, identity.DockerMounts(inspection), notInContainer)
	access, err := f.service.Acquire(ctx, Target{ContainerID: world, Name: "world", GameType: "minecraft", Roots: roots}, roots[0], true, nil)
	if err != nil || !slices.Contains(access.blocked, f.data+"/alpha") {
		t.Fatal("left a mount outside the root unexcluded", err, roots)
	}
	access.Cleanup(false)
	denied := apperr.New("FORBIDDEN", 403, "Insufficient server permissions")
	if _, err := f.service.Preview(ctx, f.target, f.root, "server.properties", func() error { return denied }); !apperr.Is(err, "FORBIDDEN") {
		t.Fatal(err)
	}
	f.helpersLeft(t)
}

// A listing helper left idle for ListerIdle is removed, with its mount
// proof, without another listing or Close.
func TestIdleListingHelperIsRemoved(t *testing.T) {
	f := setup(t)
	f.service.ListerIdle = 50 * time.Millisecond
	if _, err := f.service.List(ctx, f.target, f.root, "", nil); err != nil {
		t.Fatal(err)
	}
	helpers := func(kind string) int {
		count := 0
		for _, id := range f.engine.SortedIDs() {
			if c := f.engine.Container(id); c != nil && c.Config.Labels["ludock.internal"] != "" &&
				(kind == "" || c.Config.Labels["ludock.internal"] == kind) {
				count++
			}
		}
		return count
	}
	if helpers("file-helper") != 1 {
		t.Fatal(helpers("file-helper"), "listing helpers after a listing")
	}
	for deadline := time.Now().Add(5 * time.Second); helpers("") != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(helpers(""), "helpers left after the listing helper went idle")
		}
	}
}

// File helpers keep no Docker logs; the mount validator is read through
// its logs.
func TestHelpersKeepNoLogs(t *testing.T) {
	f := setup(t)
	if err := f.service.Mkdir(ctx, f.target, f.root, "", "logs", nil); err != nil {
		t.Fatal(err)
	}
	if logging := f.engine.HelperLogging(); logging["file-helper"] != "none" || logging["mount-validator"] != "" {
		t.Fatal(logging)
	}
}

// A daemon that never answers starting a helper, creating its command, or
// removing it fails a preview with a plain message in time instead of
// holding the request. A removal that ran out of time counts as failed.
func TestDockerCallsThatNeverAnswerFailInTime(t *testing.T) {
	f := setup(t)
	previous := launch.StepLimit
	launch.StepLimit = 200 * time.Millisecond
	t.Cleanup(func() { launch.StepLimit = previous })
	stall := func(match func(request string) bool) {
		var once atomic.Bool
		f.engine.Hang = func(request string) bool { return match(request) && once.CompareAndSwap(false, true) }
	}
	isStart := func(request string) bool {
		return strings.HasSuffix(request, "/start") && strings.Contains(request, "/containers/")
	}
	isExecCreate := func(request string) bool {
		return strings.HasPrefix(request, "POST ") && strings.HasSuffix(request, "/exec")
	}
	// The first start is the mount validator's, and the first command checks
	// the helper's mounts.
	var calls atomic.Int32
	for name, match := range map[string]func(string) bool{
		"validator start": isStart,
		"helper start":    func(request string) bool { return isStart(request) && calls.Add(1) == 2 },
		"exec create":     func(request string) bool { return isExecCreate(request) && calls.Add(1) == 2 },
	} {
		calls.Store(0)
		stall(match)
		began := time.Now()
		if _, err := f.service.Preview(ctx, f.target, f.root, "server.properties", nil); !errors.Is(err, launch.ErrDockerTimeout) {
			t.Fatal(name, err)
		}
		if time.Since(began) > 10*time.Second {
			t.Fatal(name, "waited too long")
		}
		f.engine.Hang = nil
		f.helpersLeft(t)
	}
	access, err := f.service.Acquire(ctx, f.target, f.target.Roots[0], true, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.engine.Hang = func(request string) bool { return strings.HasPrefix(request, "DELETE ") }
	if err := access.Cleanup(false); err == nil {
		t.Fatal("a removal Docker never answered succeeded")
	}
}

// Failed changes remove their helper before returning.
func TestFailedChangeRemovesHelper(t *testing.T) {
	f := setup(t)
	// The first removal of each helper fails.
	started := f.engine.OnStart
	f.engine.OnStart = func(c *fakedocker.Container) {
		f.engine.AddFault(`^DELETE .*/containers/`+c.ID+`$`, 500, false, 1)
		started(c)
	}
	for name, change := range map[string]func() error{
		"mkdir": func() error { return f.service.Mkdir(ctx, f.target, f.root, "", "World", nil) },
		"rename": func() error {
			return f.service.Rename(ctx, f.target, f.root, "missing.txt", "other.txt", nil)
		},
		"upload": func() error {
			_, err := f.service.UploadFiles(ctx, f.target, f.root, "", []UploadEntry{{Name: "short.txt", Size: 10}}, strings.NewReader("short"), nil)
			return err
		},
	} {
		// The changed data may still be written, so removal is retried until
		// Docker confirms it, even after failures.
		if err := change(); err == nil {
			t.Fatal(name, "succeeded")
		}
		f.helpersLeft(t)
	}
}

func TestLeftoverHelpersAreRemovedAtStartup(t *testing.T) {
	f := setup(t)
	leftover := f.engine.AddContainer(fakedocker.GameOptions{Name: "leftover", Image: image, Running: true,
		Labels: map[string]string{"ludock.enable": "false", "ludock.internal": "file-helper"}})
	// Its mount validator can read the whole host.
	validator := f.engine.AddContainer(fakedocker.GameOptions{Name: "validator", Image: image, Running: true,
		Labels: map[string]string{"ludock.enable": "false", "ludock.internal": "mount-validator"}})
	// Removal that Docker refuses is reported, so startup keeps the fence.
	f.engine.AddFault(`^DELETE .*/containers/[^/]+$`, 500, false, 0)
	if err := f.service.RemoveLeftovers(ctx); err == nil {
		t.Fatal("reported a helper removed that is still there")
	}
	f.engine.ClearFaults()
	if err := f.service.RemoveLeftovers(ctx); err != nil {
		t.Fatal(err)
	}
	if f.engine.Container(leftover) != nil {
		t.Fatal("left a helper that may still be writing")
	}
	if f.engine.Container(validator) != nil {
		t.Fatal("left a mount validator")
	}
	if f.engine.Container(f.game) == nil {
		t.Fatal("removed the game")
	}
}

// With no upload size limit, a file that would leave the disk less than its
// spare space is refused before anything is written.
func TestUploadNeedsFreeSpace(t *testing.T) {
	f := setup(t)
	var stat unix.Statfs_t
	if err := unix.Statfs(f.data, &stat); err != nil {
		t.Fatal(err)
	}
	space := helper.Space(&stat)
	// Within the free space, but not with the spare kept; the margin covers
	// other writers freeing space meanwhile.
	size := space.Available - helper.UploadSpare(space.Total) + 256<<20
	results, err := f.service.UploadFiles(ctx, f.target, f.root, "", []UploadEntry{{Name: "huge.bin", Size: size}}, zeroFree{}, nil)
	if public, ok := apperr.As(results[0]); err != nil || !ok || public.Code != "NOT_ENOUGH_SPACE" || public.Status != 507 {
		t.Fatal(results, err)
	}
	if _, err := os.Stat(filepath.Join(f.data, "huge.bin")); !os.IsNotExist(err) {
		t.Fatal("file written", err)
	}
	if got := helper.UploadSpare(10 << 30); got != 512<<20 {
		t.Fatal("a small disk keeps a twentieth", got)
	}
	if got := helper.UploadSpare(1 << 40); got != 1<<30 {
		t.Fatal("a large disk keeps 1 GiB", got)
	}
}

// A server given one subfolder of a volume (volume-subpath) gets a file
// helper that Docker mounts the same subfolder into, pinned by the mount
// proof, never the whole volume, which would show its siblings.
func TestFileHelperMountsOnlyTheVolumeSubpath(t *testing.T) {
	f := setup(t)
	host := t.TempDir()
	os.MkdirAll(filepath.Join(host, "var", "lib", "docker", "volumes", "shared", "_data", "worlds", "a"), 0o755)
	helper.HostRoot = host
	client := docker.New(f.engine.Socket)
	client.CreateVolume(ctx, "shared")
	game := f.engine.AddContainer(fakedocker.GameOptions{Name: "sub", Image: "itzg/minecraft-server", Running: true,
		Mounts: []docker.MountPoint{{Type: "volume", Name: "shared", Source: "/var/lib/docker/volumes/shared/_data", Destination: "/data", RW: true}}})
	f.engine.Update(game, func(c *fakedocker.Container) {
		c.HostConfig = map[string]any{"Mounts": []map[string]any{{"Type": "volume", "Source": "shared", "Target": "/data",
			"VolumeOptions": map[string]any{"Subpath": "worlds/a"}}}}
	})
	inspection, _ := client.Inspect(ctx, game)
	roots := Roots("minecraft", nil, identity.DockerMounts(inspection), notInContainer)
	if len(roots) != 1 {
		t.Fatal(roots)
	}
	// The helper's own mount isn't on this host, so its identity check fails
	// after Docker was asked for it.
	f.service.Acquire(ctx, Target{ContainerID: game, Name: "sub", GameType: "minecraft", Roots: roots}, roots[0], true, nil)
	var created []docker.MountSettings
	for _, body := range f.engine.Creates {
		if body.Labels["ludock.internal"] == "file-helper" {
			created = body.HostConfig.Mounts
		}
	}
	if len(created) != 1 || created[0].Source != "shared" || created[0].VolumeOptions == nil || created[0].VolumeOptions.Subpath != "worlds/a" {
		t.Fatalf("%+v", created)
	}
	f.helpersLeft(t)
}

// Docker fails a download: it ends the command without running the helper,
// it can't say how the command ended because it stopped answering, or it
// goes away partway through the stream. The
// download reports Docker, not the path; a file that is really missing
// still blames the path, and an exit Docker records late blames nothing.
func TestDownloadBlamesDockerForItsFailures(t *testing.T) {
	f := setup(t)
	f.engine.FailExecStart = func(cmd []string) bool {
		return slices.Contains(cmd, "file") && strings.Contains(strings.Join(cmd, " "), "download")
	}
	if _, err := f.service.Preview(ctx, f.target, f.root, "server.properties", nil); !docker.Unavailable(err) {
		t.Fatal("a command Docker never ran:", err)
	}
	f.engine.FailExecStart = nil
	download, err := f.service.OpenDownload(ctx, f.target, f.root, "server.properties", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.engine.AddFault(`^GET /[^ ]*exec/[^/]+/json$`, 500, false, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(download); !docker.Unavailable(err) {
		t.Fatal("an outcome Docker couldn't report:", err)
	}
	download.Close()
	f.engine.ClearFaults()
	download, err = f.service.OpenDownload(ctx, f.target, f.root, "big.log", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := download.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Outage(100 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if rest, err := io.ReadAll(download); !docker.Unavailable(err) || int64(len(rest))+1 >= download.Size {
		t.Fatal("a stream Docker cut:", len(rest), err)
	}
	download.Close()
	if _, err := f.service.Preview(ctx, f.target, f.root, "missing.txt", nil); !apperr.Is(err, "UNSAFE_OR_CHANGED_FILE_PATH") {
		t.Fatal("a missing file:", err)
	}
	// Docker closing the stream a moment before it records the exit fails
	// nothing.
	f.engine.ExitDelay = 300 * time.Millisecond
	if _, err := f.service.Preview(ctx, f.target, f.root, "server.properties", nil); err != nil {
		t.Fatal("an exit Docker recorded late:", err)
	}
	f.engine.ExitDelay = 0
	// The preview that worked keeps its helper for the next read.
	f.service.Close()
	f.helpersLeft(t)
}

// raceReader runs at once the first n bytes are read, as a game changing
// the folder while an upload streams.
type raceReader struct {
	body string
	n    int
	at   func()
}

func (r *raceReader) Read(p []byte) (int, error) {
	if r.body == "" {
		return 0, io.EOF
	}
	count := copy(p, r.body[:min(len(r.body), max(r.n, 1))])
	r.body, r.n = r.body[count:], r.n-count
	if r.n <= 0 && r.at != nil {
		r.at()
		r.at = nil
	}
	return count, nil
}

// A batch not confirmed to replace writes its new files and refuses each
// name already taken, whether at the start or while the file streamed,
// leaving what is there; confirmed, it replaces.
func TestUploadReplacesOnlyWhenConfirmed(t *testing.T) {
	f := setup(t)
	os.WriteFile(filepath.Join(f.data, "taken.txt"), []byte("old"), 0o644)
	entries := []UploadEntry{{Name: "taken.txt", Size: 3}, {Name: "new.txt", Size: 3}, {Name: "raced.txt", Size: 3}}
	body := &raceReader{body: "abcdefghi", n: 7, at: func() { os.WriteFile(filepath.Join(f.data, "raced.txt"), []byte("game"), 0o644) }}
	results, err := f.service.UploadFiles(ctx, f.target, f.root, "", entries, body, nil)
	if err != nil || !errors.Is(results[0], launch.ErrNameTaken) || results[1] != nil || !errors.Is(results[2], launch.ErrNameTaken) {
		t.Fatal(results, err)
	}
	for name, want := range map[string]string{"taken.txt": "old", "new.txt": "def", "raced.txt": "game"} {
		if data, _ := os.ReadFile(filepath.Join(f.data, name)); string(data) != want {
			t.Errorf("%s: %q", name, data)
		}
	}
	results, err = f.service.UploadFiles(ctx, f.target, f.root, "", []UploadEntry{{Name: "taken.txt", Size: 3, Replace: true}}, strings.NewReader("abc"), nil)
	if data, _ := os.ReadFile(filepath.Join(f.data, "taken.txt")); err != nil || results[0] != nil || string(data) != "abc" {
		t.Fatal(results, err, string(data))
	}
}

// One batch writes files in several folders below its own through one
// helper, making the missing ones. Each file finds its folders again without
// following links: a folder swapped for a link to elsewhere while the batch
// streams refuses its files there and writes nothing through the link, and a
// file where a folder is needed, or a folder where a file is, refuses only
// that file.
func TestUploadNestedBatch(t *testing.T) {
	f := setup(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(f.data, "blocker"), []byte("file"), 0o644)
	os.MkdirAll(filepath.Join(f.data, "mods", "taken"), 0o755)
	os.MkdirAll(filepath.Join(f.data, "mods", "swapped"), 0o755)
	entries := []UploadEntry{{Name: "mods/a.jar", Size: 3}, {Name: "mods/swapped/b.jar", Size: 3}, {Name: "mods/swapped/c.jar", Size: 3},
		{Name: "config/x/y/z.toml", Size: 3}, {Name: "blocker/d.txt", Size: 3}, {Name: "mods/taken", Size: 3}, {Name: "config/x/w.toml", Size: 3}}
	creates := func() (count int) {
		for _, call := range f.engine.Requests {
			if strings.HasSuffix(call, "/containers/create") {
				count++
			}
		}
		return count
	}
	before := creates()
	// Once mods/swapped/b.jar's body is in, the folder becomes a link out.
	swapped := filepath.Join(f.data, "mods", "swapped")
	body := &raceReader{body: "aaabbbcccddddddeeefff", n: 4, at: func() {
		os.Rename(swapped, filepath.Join(outside, "moved"))
		os.Symlink(outside, swapped)
	}}
	results, err := f.service.UploadFiles(ctx, f.target, f.root, "", entries, body, nil)
	if err != nil || results[0] != nil || results[1] == nil || results[2] == nil || results[3] != nil ||
		!apperr.Is(results[4], "NOT_A_FOLDER") || !apperr.Is(results[5], "IS_A_FOLDER") || results[6] != nil {
		t.Fatal(results, err)
	}
	for name, want := range map[string]string{"mods/a.jar": "aaa", "config/x/y/z.toml": "ddd", "config/x/w.toml": "fff", "blocker": "file"} {
		if data, _ := os.ReadFile(filepath.Join(f.data, name)); string(data) != want {
			t.Errorf("%s: %q", name, data)
		}
	}
	if listed, _ := os.ReadDir(outside); len(listed) != 1 {
		t.Fatal("wrote through the link:", listed)
	}
	if moved, _ := os.ReadDir(filepath.Join(outside, "moved")); len(moved) != 0 {
		t.Fatal("published in the moved folder:", moved)
	}
	// The helper and the validator that proved its mount.
	if started := creates() - before; started != 2 {
		t.Fatal(started, "containers started for one batch")
	}
}

// Access is confirmed again before each folder an upload makes, as before
// each file it publishes: once it is revoked, no further folder is made,
// whether for another file or on the way to the same one.
func TestUploadFoldersFollowAccess(t *testing.T) {
	for _, test := range []struct {
		entries []UploadEntry
		first   string
		then    string
	}{
		{[]UploadEntry{{Name: "one/a.txt", Size: 1}, {Name: "two/b.txt", Size: 1}}, "one", "two"},
		{[]UploadEntry{{Name: "deep/er/a.txt", Size: 1}}, "deep", "deep/er"},
	} {
		f := setup(t)
		revoked := func() error {
			if _, err := os.Stat(filepath.Join(f.data, test.first)); err == nil {
				return apperr.New("FORBIDDEN", 403, "You can’t do that")
			}
			return nil
		}
		results, err := f.service.UploadFiles(ctx, f.target, f.root, "", test.entries, strings.NewReader("ab"[:len(test.entries)]), revoked)
		if err == nil || slices.Contains(results, nil) {
			t.Fatal(test.first, results, err)
		}
		if _, err := os.Stat(filepath.Join(f.data, test.first)); err != nil {
			t.Fatal("the first folder wasn't made", err)
		}
		if _, err := os.Stat(filepath.Join(f.data, test.then)); !os.IsNotExist(err) {
			t.Fatal("made", test.then, "after access was revoked", err)
		}
	}
}

// A batch's folders have practical bounds: a file too deep is refused on
// its own, and a batch in too many folders as a whole, before anything is
// made. A folder is made only for a file that fits on its disk, and a helper
// older than the backend's upload protocol is refused before any upload.
func TestUploadFolderLimits(t *testing.T) {
	f := setup(t)
	deep := strings.Repeat("d/", helper.UploadDepth+1) + "deep.txt"
	results, err := f.service.UploadFiles(ctx, f.target, f.root, "", []UploadEntry{{Name: deep, Size: 1}, {Name: "ok.txt", Size: 1}},
		strings.NewReader("ab"), nil)
	if err != nil || !apperr.Is(results[0], "UPLOAD_TOO_DEEP") || results[1] != nil {
		t.Fatal(results, err)
	}
	// A file too deep counts no folders toward the batch's.
	var near []UploadEntry
	for index := range 77 {
		near = append(near, UploadEntry{Name: fmt.Sprintf("g%02d/a/b/c.txt", index), Size: 1})
	}
	near = append(near, UploadEntry{Name: deep, Size: 1})
	results, err = f.service.UploadFiles(ctx, f.target, f.root, "", near, strings.NewReader(strings.Repeat("x", len(near))), nil)
	if err != nil || results[0] != nil || !apperr.Is(results[len(near)-1], "UPLOAD_TOO_DEEP") {
		t.Fatal(results[0], results[len(near)-1], err)
	}
	var spread []UploadEntry
	for index := range 100 {
		spread = append(spread, UploadEntry{Name: fmt.Sprintf("f%03d/a/b/c.txt", index)})
	}
	if _, err := f.service.UploadFiles(ctx, f.target, f.root, "", spread, strings.NewReader(""), nil); !errors.Is(err, ErrBatchTooLarge) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.data, "d")); !os.IsNotExist(err) {
		t.Fatal("made folders for a file too deep", err)
	}
	// A folder the upload made and the game then removed isn't asked for
	// again: only its files are refused, and the rest of the batch goes on.
	removed := false
	gone := func() error {
		if _, err := os.Stat(filepath.Join(f.data, "gone")); err == nil && !removed {
			removed = os.RemoveAll(filepath.Join(f.data, "gone")) == nil
		}
		return nil
	}
	results, err = f.service.UploadFiles(ctx, f.target, f.root, "", []UploadEntry{{Name: "gone/a.txt", Size: 1}, {Name: "gone/b.txt", Size: 1},
		{Name: "kept.txt", Size: 1}}, strings.NewReader("abc"), gone)
	if err != nil || !removed || results[0] == nil || results[1] == nil || results[2] != nil {
		t.Fatal(results, err)
	}
	if _, err := os.Stat(filepath.Join(f.data, "f000")); !os.IsNotExist(err) {
		t.Fatal("made folders for a batch in too many", err)
	}
	var stat unix.Statfs_t
	unix.Statfs(f.data, &stat)
	huge := helper.Space(&stat).Available + 256<<20
	results, err = f.service.UploadFiles(ctx, f.target, f.root, "", []UploadEntry{{Name: "room/for/huge.bin", Size: huge}}, zeroFree{}, nil)
	if !apperr.Is(results[0], "NOT_ENOUGH_SPACE") {
		t.Fatal(results, err)
	}
	if _, err := os.Stat(filepath.Join(f.data, "room")); !os.IsNotExist(err) {
		t.Fatal("made folders for a file that doesn't fit", err)
	}
	// An older helper, chosen with LUDOCK_HELPER_IMAGE, doesn't know the
	// question; asked once per image, it is never driven.
	f.service.Images.Configured = "ghcr.io/crunging/ludock@sha256:" + strings.Repeat("c", 64)
	f.engine.AddImage(f.service.Images.Configured)
	f.engine.RunHelpers(func(_ *fakedocker.Container, _, args []string, _ *bufio.Reader, _, stderr func([]byte)) (int, bool) {
		if len(args) > 1 && strings.Contains(args[1], `"operation":"version"`) {
			stderr([]byte(helper.FileFailure))
			return 1, true
		}
		return 0, false
	})
	results, err = f.service.UploadFiles(ctx, f.target, f.root, "", []UploadEntry{{Name: "late/x.txt", Size: 1}}, strings.NewReader("x"), nil)
	if !errors.Is(err, launch.ErrHelperOutdated) || !errors.Is(results[0], launch.ErrHelperOutdated) {
		t.Fatal(results, err)
	}
	if _, err := os.Stat(filepath.Join(f.data, "late")); !os.IsNotExist(err) {
		t.Fatal("an older helper made a folder", err)
	}
	// Nor does it replace a file saved in the editor.
	os.WriteFile(filepath.Join(f.data, "eula.txt"), []byte("eula=false\n"), 0o644)
	preview, err := f.service.Preview(ctx, f.target, f.root, "eula.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.Save(ctx, f.target, f.root, "eula.txt", []byte("eula=true\n"), preview.Version, nil); !errors.Is(err, launch.ErrHelperOutdated) {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(f.data, "eula.txt")); string(data) != "eula=false\n" {
		t.Fatal("an older helper saved a file:", string(data))
	}
}

// The backend holds the helper to the batch: a folder is approved only for
// the file the helper is on, inside that file's path, and once; a body is
// sent only in its file's turn, and a success counts only after its
// trailer. The fake helper reads the backend's answer to each line before
// the next, and the last line of each case is the one out of place, which
// must end the upload unanswered.
func TestUploadHoldsHelperToItsFiles(t *testing.T) {
	f := setup(t)
	entries := []UploadEntry{{Name: "a/b/x.txt", Size: 1}, {Name: "y.txt", Size: 1}}
	// A line and how many bytes the backend sends back for it: a folder's
	// approval, a 1-byte body, or a 36-byte trailer.
	type line struct {
		text   string
		answer int
	}
	folder, body, ready, result := 1, 1, 36, 0
	for _, test := range [][]line{
		{{`{"file":0,"folder":"c"}`, folder}},
		{{`{"file":0,"folder":"a/b/x.txt"}`, folder}},
		{{`{"file":0,"folder":"a"}`, folder}, {`{"file":0,"folder":"a"}`, folder}},
		{{`{"file":1,"body":true}`, body}},
		{{`{"file":0,"body":true}`, body}, {`{"file":0,"ok":true}`, result}},
		{{`{"file":0,"body":true}`, body}, {`{"file":0,"ready":true}`, ready}, {`{"file":0,"ok":true}`, result}, {`{"file":1,"folder":"a"}`, folder}},
	} {
		answered := make(chan bool, 1)
		f.engine.RunHelpers(func(_ *fakedocker.Container, _, args []string, stdin *bufio.Reader, stdout, _ func([]byte)) (int, bool) {
			if len(args) < 2 || !strings.Contains(args[1], `"operation":"upload-files"`) {
				return 0, false
			}
			var length [8]byte
			io.ReadFull(stdin, length[:])
			io.ReadFull(stdin, make([]byte, binary.BigEndian.Uint64(length[:])))
			last := test[len(test)-1]
			for _, step := range test[:len(test)-1] {
				stdout([]byte(step.text + "\n"))
				if _, err := io.ReadFull(stdin, make([]byte, step.answer)); err != nil {
					answered <- false
					return 1, true
				}
			}
			stdout([]byte(last.text + "\n"))
			// Refused, the input ends with nothing more; accepted, its answer
			// comes.
			got := last.answer > 0
			if got {
				_, err := stdin.ReadByte()
				got = err == nil
			}
			answered <- got
			return 1, true
		})
		results, err := f.service.UploadFiles(ctx, f.target, f.root, "", entries, strings.NewReader("ab"), nil)
		var got bool
		select {
		case got = <-answered:
		case <-time.After(10 * time.Second):
			t.Fatal("the helper never ran", results, err)
		}
		if err == nil || got || (test[len(test)-1].answer == result && results[0] == nil) {
			t.Error(test[len(test)-1].text, results, err, got)
		}
	}
}
