package files

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
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
	roots := Roots("minecraft", nil, []Mount{{Type: "bind", Source: data, Destination: data, RW: true}})
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

func TestListSortsFoldersFirstIgnoringCase(t *testing.T) {
	f := setup(t)
	listing, err := f.service.List(ctx, f.target, "root-0", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range listing.Entries {
		names = append(names, entry.Name)
	}
	if strings.Join(names, ",") != "alpha,World,Banned.json,big.log,level.dat,server.properties" {
		t.Fatal(names)
	}
	f.helpersLeft(t)
	for _, bad := range []string{"../etc", "/etc", "a/../../b", "x\x00"} {
		if _, err := f.service.List(ctx, f.target, "root-0", bad, nil); !apperr.Is(err, "INVALID_PATH") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := f.service.List(ctx, f.target, "root-9", "", nil); !apperr.Is(err, "ROOT_NOT_FOUND") {
		t.Fatal(err)
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

func TestMutationsUploadAndDownload(t *testing.T) {
	f := setup(t)
	if err := f.service.Mkdir(ctx, f.target, "root-0", "", "plugins", nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", ".", "..", "a/b", " padded", "bad\nname", strings.Repeat("n", 256)} {
		if err := f.service.Mkdir(ctx, f.target, "root-0", "", name, nil); !apperr.Is(err, "INVALID_NAME") {
			t.Errorf("%q: %v", name, err)
		}
	}
	body := strings.NewReader("new contents")
	if err := f.service.Upload(ctx, f.target, "root-0", "plugins", "a.txt", int64(body.Len()), body, nil); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(f.data, "plugins", "a.txt")); string(data) != "new contents" {
		t.Fatal(string(data))
	}
	// A body longer than declared is refused and leaves no file.
	if err := f.service.Upload(ctx, f.target, "root-0", "", "long.txt", 2, strings.NewReader("abc"), nil); err == nil {
		t.Fatal("accepted a body longer than declared")
	}
	if _, err := os.Stat(filepath.Join(f.data, "long.txt")); !os.IsNotExist(err) {
		t.Fatal("partial upload published")
	}
	if err := f.service.Rename(ctx, f.target, "root-0", "plugins/a.txt", "b.txt", nil); err != nil {
		t.Fatal(err)
	}
	download, err := f.service.OpenDownload(ctx, f.target, "root-0", "plugins", nil)
	if err != nil {
		t.Fatal(err)
	}
	if download.Name != "plugins.tar" || download.Type != "directory" {
		t.Fatal(download.Name, download.Type)
	}
	reader := tar.NewReader(download)
	found := map[string]bool{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found[header.Name] = true
	}
	download.Close()
	if !found["plugins/b.txt"] {
		t.Fatal(found)
	}
	if err := f.service.Delete(ctx, f.target, "root-0", "", nil); !apperr.Is(err, "ROOT_MUTATION") {
		t.Fatal(err)
	}
	if err := f.service.Delete(ctx, f.target, "root-0", "plugins", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.data, "plugins")); !os.IsNotExist(err) {
		t.Fatal("not deleted")
	}
	f.helpersLeft(t)
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
