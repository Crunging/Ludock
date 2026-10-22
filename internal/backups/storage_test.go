package backups

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/servers"
)

var ctx = context.Background()

const image = "ghcr.io/crunging/ludock@sha256:abababababababababababababababababababababababababababababababab"

// startHelpers runs helper commands in-process. Helper containers see data
// under /mounts/<root>; the fake maps those targets back to host sources.
func startHelpers(t *testing.T) (*fakedocker.Engine, *docker.Client) {
	t.Helper()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	helper.HostRoot = "/"
	t.Cleanup(func() { helper.HostRoot = "/host" })
	engine.AddImage(image)
	engine.OnStart = func(c *fakedocker.Container) {
		if c.CreateRequest.Labels["ludock.internal"] == "mount-validator" {
			var out bytes.Buffer
			helper.RunMountProof(c.CreateRequest.Cmd[2], "1", &out)
			engine.Update(c.ID, func(c *fakedocker.Container) { c.Logs = []string{strings.TrimSpace(out.String())} })
		}
	}
	engine.Exec = func(c *fakedocker.Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int {
		args := append([]string(nil), cmd[2:]...)
		if c.CreateRequest.HostConfig != nil {
			for _, mount := range c.CreateRequest.HostConfig.Mounts {
				for index := range args {
					args[index] = strings.ReplaceAll(args[index], `"`+mount.Target, `"`+mount.Source)
				}
			}
		}
		var out, errs bytes.Buffer
		code := helper.Main(args, stdin, &out, &errs)
		stdout(out.Bytes())
		stderr(errs.Bytes())
		return code
	}
	return engine, docker.New(engine.Socket)
}

func serverContext(data string) *servers.Context {
	return &servers.Context{
		Container:   servers.Container{FileRoots: []files.Root{{ID: "root-0", Name: "data", Path: "/data"}}},
		Observation: identity.Observation{Mounts: []identity.Mount{{Type: "bind", Source: data, Destination: "/data", Writable: true}}},
	}
}

func TestApprovedDirectories(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "backups")
	os.MkdirAll(filepath.Join(root, "ludock"), 0o755)
	os.Symlink(base, filepath.Join(root, "link"))
	os.Symlink(root, filepath.Join(base, "rootlink"))
	storage := &Storage{Roots: root}
	if got, err := storage.ApprovedDirectory(filepath.Join(root, "ludock")); err != nil || got != filepath.Join(root, "ludock") {
		t.Fatal(got, err)
	}
	for _, directory := range []string{filepath.Join(root, "link"), base, "relative", filepath.Join(root, "missing"), root + "/ludock/../../"} {
		if _, err := storage.ApprovedDirectory(directory); !apperr.Is(err, "BACKUP_DESTINATION") {
			t.Errorf("%s: %v", directory, err)
		}
	}
	if _, err := (&Storage{Roots: filepath.Join(base, "rootlink")}).ApprovedDirectory(filepath.Join(base, "rootlink")); !apperr.Is(err, "BACKUP_DESTINATION") {
		t.Fatal("symlinked root accepted", err)
	}
	if _, err := (&Storage{}).ApprovedDirectory(root); !apperr.Is(err, "BACKUP_DESTINATION") {
		t.Fatal("no roots accepted")
	}
}

func TestPlanRefusesNestedAndOverlappingRoots(t *testing.T) {
	context := serverContext("/srv/game")
	context.Observation.Mounts = append(context.Observation.Mounts, identity.Mount{Type: "bind", Source: "/srv/other", Destination: "/data/nested", Writable: true})
	if _, err := PlanRoots(context, true); !apperr.Is(err, "NESTED_BACKUP_MOUNT") {
		t.Fatal(err)
	}
	context = serverContext("/srv/game")
	context.Container.FileRoots = append(context.Container.FileRoots, files.Root{ID: "root-1", Path: "/alias"})
	context.Observation.Mounts = append(context.Observation.Mounts, identity.Mount{Type: "bind", Source: "/srv/game/world", Destination: "/alias", Writable: true})
	if _, err := PlanRoots(context, true); !apperr.Is(err, "OVERLAPPING_BACKUP_ROOTS") {
		t.Fatal(err)
	}
	plan, err := PlanRoots(serverContext("/srv/game"), true)
	if err != nil || plan.Mounts[0].Target != "/mounts/root-0" || plan.Aliases[0] != "/mounts/root-0" || !plan.Mounts[0].ReadOnly {
		t.Fatal(plan, err)
	}
}

func TestSnapshotValidateAndStageRoundTrip(t *testing.T) {
	_, client := startHelpers(t)
	base := t.TempDir()
	data := filepath.Join(base, "game")
	os.MkdirAll(filepath.Join(data, "world"), 0o755)
	os.WriteFile(filepath.Join(data, "world", "level.dat"), []byte("level"), 0o644)
	os.WriteFile(filepath.Join(data, "server.properties"), []byte("motd=hi"), 0o640)
	destination := filepath.Join(base, "backups")
	os.MkdirAll(destination, 0o755)
	storage := &Storage{Docker: client, Roots: destination}
	images := &launch.Images{Docker: client, Configured: image}
	helpers := &Helpers{Docker: client, Images: images, Storage: storage, pollInterval: 1}
	context := serverContext(data)
	dataHelper, err := helpers.NewDataHelper(ctx, context, true, "op-1")
	if err != nil {
		t.Fatal(err)
	}
	settings := Settings{Destination: destination, RetentionCount: 3, MaxBytes: 1 << 30, ReserveBytes: 0}
	id := "6f9619ff-8b86-4011-b42d-00c04fc964ff"
	stoppedChecks := 0
	size, checksum, err := helpers.WriteSnapshot(ctx, context, dataHelper, settings, id, 1<<30,
		func() error { stoppedChecks++; return nil }, nil)
	dataHelper.Cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if stoppedChecks < 3 || size == 0 || len(checksum) != 64 {
		t.Fatal(stoppedChecks, size, checksum)
	}
	if _, err := os.Stat(filepath.Join(destination, id+".tar.partial")); !os.IsNotExist(err) {
		t.Fatal("partial left behind")
	}
	roots := []Root{{ID: "root-0", Path: "/data"}}
	if _, err := storage.Validate(destination, id, roots, 1<<30, checksum); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Validate(destination, id, roots, 1<<30, strings.Repeat("0", 64)); !apperr.Is(err, "BACKUP_CHECKSUM") {
		t.Fatal(err)
	}
	if _, err := storage.Validate(destination, id, []Root{{ID: "root-0"}, {ID: "root-1"}}, 1<<30, checksum); !apperr.Is(err, "INVALID_BACKUP") {
		t.Fatal(err)
	}
	if _, err := storage.Validate(destination, id, roots, 100, checksum); !apperr.Is(err, "BACKUP_CAPACITY") {
		t.Fatal(err)
	}
	// Stage the archive into a restore stage of a writable helper.
	writer, err := helpers.NewDataHelper(ctx, context, false, "op-2")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Cleanup()
	stage := ".ludock-restore-0f9619ff-8b86-4011-b42d-00c04fc964ff"
	rootPath, _ := HelperRoot(context, roots[0])
	if _, err := helpers.Exec(ctx, writer.Container, helper.Command("restore", helper.RestoreRequest{Root: rootPath, Stage: stage, Operation: "stage"}), nil); err != nil {
		t.Fatal(err)
	}
	if err := helpers.ExtractToStage(ctx, destination, id, writer.Container, roots[0], rootPath, stage, roots, 1<<30, 0, checksum, nil); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(data, stage, "new", "world", "level.dat")); string(data) != "level" {
		t.Fatal(string(data))
	}
	if info, _ := os.Stat(filepath.Join(data, stage, "new", "server.properties")); info == nil || info.Mode().Perm() != 0o640 {
		t.Fatal("mode not restored")
	}
	if err := helpers.ExtractToStage(ctx, destination, id, writer.Container, roots[0], rootPath, ".ludock-restore-1f9619ff-8b86-4011-b42d-00c04fc964ff", roots, 1<<30, 0, strings.Repeat("1", 64), nil); err == nil {
		t.Fatal("changed checksum accepted")
	}
	// However small the estimate, extraction never writes into the reserve.
	full := ".ludock-restore-2f9619ff-8b86-4011-b42d-00c04fc964ff"
	if _, err := helpers.Exec(ctx, writer.Container, helper.Command("restore", helper.RestoreRequest{Root: rootPath, Stage: full, Operation: "stage"}), nil); err != nil {
		t.Fatal(err)
	}
	if err := helpers.ExtractToStage(ctx, destination, id, writer.Container, roots[0], rootPath, full, roots, 1<<30, 1<<52, checksum, nil); !apperr.Is(err, "RESTORE_CAPACITY") {
		t.Fatal("extracted into the reserve:", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(data, full, "new")); len(entries) != 0 {
		t.Fatal("wrote past the reserve", entries)
	}
	if err := storage.Remove(destination, id, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, id+".tar")); !os.IsNotExist(err) {
		t.Fatal("not removed")
	}

}

func TestSnapshotRejectsSymlinksInData(t *testing.T) {
	_, client := startHelpers(t)
	base := t.TempDir()
	data := filepath.Join(base, "game")
	os.MkdirAll(data, 0o755)
	os.Symlink("/etc/passwd", filepath.Join(data, "escape"))
	destination := filepath.Join(base, "backups")
	os.MkdirAll(destination, 0o755)
	storage := &Storage{Docker: client, Roots: destination}
	helpers := &Helpers{Docker: client, Images: &launch.Images{Docker: client, Configured: image}, Storage: storage, pollInterval: 1}
	context := serverContext(data)
	dataHelper, err := helpers.NewDataHelper(ctx, context, true, "op-3")
	if err != nil {
		t.Fatal(err)
	}
	defer dataHelper.Cleanup()
	id := "7f9619ff-8b86-4011-b42d-00c04fc964ff"
	_, _, err = helpers.WriteSnapshot(ctx, context, dataHelper, Settings{Destination: destination, MaxBytes: 1 << 30}, id, 1<<30, func() error { return nil }, nil)
	if !apperr.Is(err, "UNSAFE_ARCHIVE") {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(destination)
	if len(entries) != 0 {
		t.Fatal("left an archive behind")
	}

}
