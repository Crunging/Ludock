package files

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
)

var ctx = context.Background()

const image = "ghcr.io/crunging/ludock@sha256:abababababababababababababababababababababababababababababababab"

func runHelpers(_ *fakedocker.Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int {
	if len(cmd) < 3 || cmd[0] != helper.Binary {
		return 127
	}
	var out, errs bytes.Buffer
	code := helper.Main(cmd[2:], stdin, &out, &errs)
	stdout(out.Bytes())
	stderr(errs.Bytes())
	return code
}

type fixture struct {
	engine  *fakedocker.Engine
	service *Service
	target  Target
	data    string
	game    string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	engine.Exec = runHelpers
	helper.HostRoot = "/"
	t.Cleanup(func() { helper.HostRoot = "/host" })
	engine.OnStart = func(c *fakedocker.Container) {
		if c.CreateRequest.Labels["ludock.internal"] == "mount-validator" {
			var out bytes.Buffer
			helper.RunMountProof(c.CreateRequest.Cmd[2], "1", &out)
			engine.Update(c.ID, func(c *fakedocker.Container) { c.Logs = []string{strings.TrimSpace(out.String())} })
		}
	}
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
	service := &Service{Docker: client, Images: &launch.Images{Docker: client, Configured: image}}
	roots := Roots("minecraft", nil, []identity.Mount{{Type: "bind", Source: data, Destination: data, Writable: true}})
	if len(roots) != 1 {
		t.Fatal(roots)
	}
	return &fixture{engine: engine, service: service, target: Target{ContainerID: game, Name: "mc", GameType: "minecraft", Roots: roots}, data: data, game: game}
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
		preview, err := f.service.Preview(ctx, f.target, "root-0", name, nil)
		if err != nil || preview.Kind != want {
			t.Fatalf("%s: %+v %v", name, preview, err)
		}
		if want == PreviewText && preview.Text != "motd=hello\n" {
			t.Fatal(preview.Text)
		}
	}
	// A file that grew past the limit after listing is still cut off.
	kind, _, _ := ReadPreview(io.LimitReader(zeroFree{}, PreviewLimit+10))
	if kind != PreviewLarge {
		t.Fatal(kind)
	}
	if kind, _, _ := ReadPreview(strings.NewReader("\xff\xfe")); kind != PreviewBinary {
		t.Fatal("invalid UTF-8 shown as text")
	}
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
	if _, err := f.service.List(ctx, f.target, "root-0", "", func() error { return denied }); !apperr.Is(err, "FORBIDDEN") {
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
	if _, err := f.service.List(ctx, f.target, "root-0", "", nil); !apperr.Is(err, "FILE_TARGET_CHANGED") {
		t.Fatal(err)
	}
	f.helpersLeft(t)
	f.engine.OnStart = previous
	// The root's mount became read-only.
	f.engine.Update(f.game, func(game *fakedocker.Container) { game.Mounts[0].RW = false })
	if _, err := f.service.List(ctx, f.target, "root-0", "", nil); !apperr.Is(err, "FILE_ROOT_CHANGED") {
		t.Fatal(err)
	}
	f.engine.Update(f.game, func(game *fakedocker.Container) { game.Mounts[0].RW = true; game.Name = "/renamed" })
	if _, err := f.service.List(ctx, f.target, "root-0", "", nil); !apperr.Is(err, "FILE_TARGET_CHANGED") {
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
		if _, err := f.service.List(ctx, f.target, "root-0", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if started != 1 {
		t.Fatal("started", started, "helpers for two listings")
	}
	f.engine.Update(f.game, func(game *fakedocker.Container) { game.Config.Labels = map[string]string{"changed": "yes"} })
	if _, err := f.service.List(ctx, f.target, "root-0", "", nil); err != nil {
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

// A listing helper left idle for ListerIdle is removed, with its mount
// proof, without another listing or Close.
func TestIdleListingHelperIsRemoved(t *testing.T) {
	f := setup(t)
	f.service.ListerIdle = 50 * time.Millisecond
	if _, err := f.service.List(ctx, f.target, "root-0", "", nil); err != nil {
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

// Failed changes remove their helper before returning.
func TestFailedChangeRemovesHelper(t *testing.T) {
	f := setup(t)
	for name, change := range map[string]func() error{
		"mkdir": func() error { return f.service.Mkdir(ctx, f.target, "root-0", "", "World", nil) },
		"rename": func() error {
			return f.service.Rename(ctx, f.target, "root-0", "missing.txt", "other.txt", nil)
		},
		"upload": func() error {
			return f.service.Upload(ctx, f.target, "root-0", "", "short.txt", 10, strings.NewReader("short"), nil)
		},
	} {
		// The changed data may still be written, so removal is retried until
		// Docker confirms it, even after failures.
		f.engine.FailRemoves = 2
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
	f.engine.FailRemoves = 100
	if err := f.service.RemoveLeftovers(ctx); err == nil {
		t.Fatal("reported a helper removed that is still there")
	}
	f.engine.FailRemoves = 0
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
