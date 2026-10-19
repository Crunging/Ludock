package launch

import (
	"bufio"
	"bytes"
	"context"
	"errors"
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
)

var ctx = context.Background()

const pinnedImage = "ghcr.io/crunging/ludock@sha256:" + "abababababababababababababababababababababababababababababababab"

// runHelpers executes `ludock helper` commands in-process, as a real helper
// container would.
func runHelpers(_ *fakedocker.Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int {
	if len(cmd) < 3 || cmd[0] != helper.Binary || cmd[1] != "helper" {
		return 127
	}
	var out, errs bytes.Buffer
	code := helper.Main(cmd[2:], stdin, &out, &errs)
	if out.Len() > 0 {
		stdout(out.Bytes())
	}
	if errs.Len() > 0 {
		stderr(errs.Bytes())
	}
	return code
}

func setup(t *testing.T) (*fakedocker.Engine, *docker.Client, string) {
	t.Helper()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	engine.Exec = runHelpers
	id := engine.AddContainer(fakedocker.GameOptions{Name: "helper", Image: pinnedImage, Running: true})
	return engine, docker.New(engine.Socket), id
}

func TestImageResolution(t *testing.T) {
	engine, client, _ := setup(t)
	if _, err := (&Images{Docker: client, Configured: "ghcr.io/crunging/ludock:latest"}).Resolve(ctx); !errors.Is(err, ErrUnpinnedImage) {
		t.Fatal(err)
	}
	if image, err := (&Images{Docker: client, Configured: pinnedImage}).Resolve(ctx); err != nil || image != pinnedImage {
		t.Fatal(image, err)
	}
	self := engine.AddContainer(fakedocker.GameOptions{Name: "ludock", Image: "ghcr.io/crunging/ludock:latest", Running: true})
	images := &Images{Docker: client, Self: "ludock"}
	if image, err := images.Resolve(ctx); err != nil || image != engine.Container(self).Image {
		t.Fatal(image, err)
	}
	if _, err := (&Images{Docker: client, Self: "native-host"}).Resolve(ctx); !apperr.Is(err, "HELPER_IMAGE_UNAVAILABLE") {
		t.Fatal(err)
	}
	engine.Fail = 500
	if _, err := (&Images{Docker: client, Self: "other"}).Resolve(ctx); !apperr.Is(err, "HELPER_IMAGE_UNAVAILABLE") {
		t.Fatal(err)
	}
	for _, image := range []string{"alpine@sha256:" + strings.Repeat("a", 64), "localhost:5000/x/y:tag@sha256:" + strings.Repeat("b", 64)} {
		if _, err := ValidateImage(image); err != nil {
			t.Errorf("%s: %v", image, err)
		}
	}
	for _, image := range []string{"Alpine@sha256:" + strings.Repeat("a", 64), "alpine @sha256:" + strings.Repeat("a", 64), "alpine@sha256:abc"} {
		if _, err := ValidateImage(image); err == nil {
			t.Errorf("accepted %s", image)
		}
	}
}

func fileCommand(root, operation, path string, extra map[string]any) []string {
	request := map[string]any{"operation": operation, "root": root, "path": path}
	for k, v := range extra {
		request[k] = v
	}
	return helper.Command("file", request)
}

func TestExecRunsHelpersAndCommitsUploadsOnlyAfterTrailer(t *testing.T) {
	_, client, id := setup(t)
	root := t.TempDir()
	out, err := Exec(ctx, client, id, ExecOptions{Cmd: fileCommand(root, "check", "", nil)})
	if err != nil || string(out) != `{"safe":true}` {
		t.Fatal(string(out), err)
	}
	if _, err := Exec(ctx, client, id, ExecOptions{Cmd: fileCommand(root, "list", "missing", nil)}); !errors.Is(err, ErrUnsafe) {
		t.Fatal(err)
	}
	uploadID := "6f9619ff-8b86-4011-b42d-00c04fc964ff"
	upload := fileCommand(root, "upload", "world.dat", map[string]any{"size": 5, "uploadId": uploadID})
	if _, err := Exec(ctx, client, id, ExecOptions{Cmd: upload, Input: strings.NewReader("hello"), Trailer: []byte(uploadID)}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "world.dat")); string(data) != "hello" {
		t.Fatal(string(data))
	}
	// Access revoked mid-upload: the trailer is never sent, so nothing commits.
	checks := 0
	_, err = Exec(ctx, client, id, ExecOptions{Cmd: upload, Input: strings.NewReader("HELLO"), Trailer: []byte(uploadID),
		AssertAccess: func() error {
			checks++
			if checks > 2 {
				return apperr.New("FORBIDDEN", 403, "Insufficient server permissions")
			}
			return nil
		}})
	if !apperr.Is(err, "FORBIDDEN") {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "world.dat")); string(data) != "hello" {
		t.Fatal("revoked upload committed")
	}
}

type blockingReader struct{ release chan struct{} }

func (b blockingReader) Read([]byte) (int, error) { <-b.release; return 0, io.EOF }

func TestExecCancellationDoesNotWaitForInput(t *testing.T) {
	engine, client, id := setup(t)
	engine.Exec = func(_ *fakedocker.Container, _ []string, stdin *bufio.Reader, _, _ func([]byte)) int {
		io.Copy(io.Discard, stdin)
		return 1
	}
	release := make(chan struct{})
	defer close(release)
	cancelled, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := Exec(cancelled, client, id, ExecOptions{Cmd: []string{"x"}, Input: blockingReader{release}})
	if err == nil || time.Since(started) > 7*time.Second {
		t.Fatal(err, time.Since(started))
	}
}

func TestExecOutputLimit(t *testing.T) {
	engine, client, id := setup(t)
	engine.Exec = func(_ *fakedocker.Container, _ []string, _ *bufio.Reader, stdout, _ func([]byte)) int {
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		for range 5 {
			stdout(chunk)
		}
		return 0
	}
	if _, err := Exec(ctx, client, id, ExecOptions{Cmd: []string{"x"}}); !errors.Is(err, ErrOutputLimit) {
		t.Fatal(err)
	}
}

func TestMountProof(t *testing.T) {
	engine, client, helperID := setup(t)
	host := t.TempDir()
	os.MkdirAll(filepath.Join(host, "srv", "game"), 0o755)
	helper.HostRoot = host
	defer func() { helper.HostRoot = "/host" }()
	engine.OnStart = func(c *fakedocker.Container) {
		if c.CreateRequest.Labels["ludock.internal"] != "mount-validator" {
			return
		}
		var out bytes.Buffer
		args := c.CreateRequest.Cmd
		helper.RunMountProof(args[2], "1", &out)
		engine.Update(c.ID, func(c *fakedocker.Container) { c.Logs = []string{strings.TrimSpace(out.String())} })
	}
	engine.AddImage(pinnedImage)
	images := &Images{Docker: client, Configured: pinnedImage}
	proof, err := MountProof(ctx, client, images, []Mount{{Type: "bind", Source: "/srv/game/", Destination: "/data/"}}, "")
	if err != nil || proof.Identities["/data"].Ino == "" {
		t.Fatal(proof, err)
	}
	proof.Cleanup()
	if _, err := MountProof(ctx, client, images, []Mount{{Type: "bind", Source: "/srv/missing", Destination: "/data"}}, ""); !errors.Is(err, ErrUnverifiedMount) {
		t.Fatal(err)
	}
	// Remote or remapped volumes are refused.
	if _, err := MountProof(ctx, client, images, []Mount{{Type: "volume", Name: "unknown", Destination: "/data"}}, ""); !errors.Is(err, ErrUnverifiedMount) {
		t.Fatal(err)
	}
	client.CreateVolume(ctx, "world")
	if proof, err := MountProof(ctx, client, images, []Mount{{Type: "volume", Name: "world", Destination: "/data"}}, ""); err != nil || len(proof.Identities) != 0 {
		t.Fatal(err)
	}
	// The identity check inside a data helper walks the projected paths.
	identities := map[string]helper.MountIdentity{filepath.Join(host, "srv", "game"): {Dev: "1", Ino: "1"}}
	if err := AssertMountIdentities(ctx, client, helperID, identities); !errors.Is(err, ErrUnverifiedMount) {
		t.Fatal(err)
	}
	// Validators are removed after cleanup.
	for _, id := range engine.SortedIDs() {
		if c := engine.Container(id); c != nil && c.Config.Labels["ludock.internal"] == "mount-validator" {
			t.Fatal("validator left behind")
		}
	}
}
