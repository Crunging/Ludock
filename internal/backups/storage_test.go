package backups

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/self"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/tarstream"
)

var ctx = context.Background()

const image = "ghcr.io/crunging/ludock@sha256:abababababababababababababababababababababababababababababababab"

// startHelpers runs helper commands in-process.
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
	engine.RunHelpers(nil)
	return engine, docker.New(engine.Socket)
}

var notInContainer = self.Info{Answer: self.NotFound}

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
	storage := &Storage{Roots: []string{root}}
	if got, err := storage.ApprovedDirectory(filepath.Join(root, "ludock")); err != nil || got != filepath.Join(root, "ludock") {
		t.Fatal(got, err)
	}
	for _, directory := range []string{filepath.Join(root, "link"), base, "relative", filepath.Join(root, "missing"), root + "/ludock/../../"} {
		if _, err := storage.ApprovedDirectory(directory); !apperr.Is(err, "BACKUP_DESTINATION") {
			t.Errorf("%s: %v", directory, err)
		}
	}
	if _, err := (&Storage{Roots: []string{filepath.Join(base, "rootlink")}}).ApprovedDirectory(filepath.Join(base, "rootlink")); !apperr.Is(err, "BACKUP_DESTINATION") {
		t.Fatal("symlinked root accepted", err)
	}
	if _, err := (&Storage{}).ApprovedDirectory(root); !apperr.Is(err, "BACKUP_DESTINATION") {
		t.Fatal("no roots accepted")
	}
}

func TestPlanRefusesNestedAndOverlappingRoots(t *testing.T) {
	context := serverContext("/srv/game")
	context.Observation.Mounts = append(context.Observation.Mounts, identity.Mount{Type: "bind", Source: "/srv/other", Destination: "/data/nested", Writable: true})
	if _, err := PlanRoots(context, notInContainer, true); !apperr.Is(err, "NESTED_BACKUP_MOUNT") {
		t.Fatal(err)
	}
	context = serverContext("/srv/game")
	context.Container.FileRoots = append(context.Container.FileRoots, files.Root{ID: "root-1", Path: "/alias"})
	context.Observation.Mounts = append(context.Observation.Mounts, identity.Mount{Type: "bind", Source: "/srv/game/world", Destination: "/alias", Writable: true})
	if _, err := PlanRoots(context, notInContainer, true); !apperr.Is(err, "OVERLAPPING_BACKUP_ROOTS") {
		t.Fatal(err)
	}
	plan, err := PlanRoots(serverContext("/srv/game"), notInContainer, true)
	if err != nil || plan.Mounts[0].Target != "/mounts/root-0" || plan.Aliases[0] != "/mounts/root-0" || !plan.Mounts[0].ReadOnly {
		t.Fatal(plan, err)
	}
}

func TestSnapshotValidateAndStageRoundTrip(t *testing.T) {
	engine, client := startHelpers(t)
	base := t.TempDir()
	data := filepath.Join(base, "game")
	os.MkdirAll(filepath.Join(data, "world"), 0o755)
	os.WriteFile(filepath.Join(data, "world", "level.dat"), []byte("level"), 0o644)
	os.WriteFile(filepath.Join(data, "server.properties"), []byte("motd=hi"), 0o640)
	destination := filepath.Join(base, "backups")
	os.MkdirAll(destination, 0o755)
	storage := &Storage{Roots: []string{destination}, Own: self.New(nil, "")}
	images := &launch.Images{Configured: image}
	helpers := &Helpers{Docker: client, Images: images, Storage: storage, pollInterval: 1}
	context := serverContext(data)
	dataHelper, err := helpers.NewDataHelper(ctx, context, true, "op-1")
	if err != nil {
		t.Fatal(err)
	}
	if logging := engine.HelperLogging(); logging["backup-helper"] != "none" {
		t.Fatal(logging)
	}
	settings := Settings{Destination: destination, MaxBytes: 1 << 30, ReserveBytes: 0}
	// A folder holding the archive's name fails the rename that publishes it.
	taken := "5f9619ff-8b86-4011-b42d-00c04fc964ff"
	os.MkdirAll(filepath.Join(destination, taken+".tar", "kept"), 0o755)
	if _, _, err := helpers.WriteSnapshot(ctx, context, dataHelper, settings, taken, 1<<30, func() error { return nil }, nil); err == nil {
		t.Fatal("published over a folder")
	}
	if _, err := os.Stat(filepath.Join(destination, taken+".tar.partial")); !os.IsNotExist(err) {
		t.Fatal("partial left behind after the rename failed")
	}
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
	if _, err := storage.Validate(destination, id, roots, 1<<30, checksum, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Validate(destination, id, roots, 1<<30, strings.Repeat("0", 64), nil); !apperr.Is(err, "BACKUP_CHECKSUM") {
		t.Fatal(err)
	}
	if _, err := storage.Validate(destination, id, []Root{{ID: "root-0"}, {ID: "root-1"}}, 1<<30, checksum, nil); !apperr.Is(err, "INVALID_BACKUP") {
		t.Fatal(err)
	}
	// An archive larger than its recorded size was changed.
	if _, err := storage.Validate(destination, id, roots, 100, checksum, nil); !apperr.Is(err, "INVALID_BACKUP") {
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
	if err := helpers.ExtractToStage(ctx, destination, id, writer.Container, []StageTarget{{Root: roots[0], Path: rootPath}}, stage, roots, 1<<30, 0, checksum, nil); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(data, stage, "new", "world", "level.dat")); string(data) != "level" {
		t.Fatal(string(data))
	}
	if info, _ := os.Stat(filepath.Join(data, stage, "new", "server.properties")); info == nil || info.Mode().Perm() != 0o640 {
		t.Fatal("mode not restored")
	}
	if err := helpers.ExtractToStage(ctx, destination, id, writer.Container, []StageTarget{{Root: roots[0], Path: rootPath}}, ".ludock-restore-1f9619ff-8b86-4011-b42d-00c04fc964ff", roots, 1<<30, 0, strings.Repeat("1", 64), nil); err == nil {
		t.Fatal("changed checksum accepted")
	}
	// However small the estimate, extraction never writes into the reserve.
	full := ".ludock-restore-2f9619ff-8b86-4011-b42d-00c04fc964ff"
	if _, err := helpers.Exec(ctx, writer.Container, helper.Command("restore", helper.RestoreRequest{Root: rootPath, Stage: full, Operation: "stage"}), nil); err != nil {
		t.Fatal(err)
	}
	if err := helpers.ExtractToStage(ctx, destination, id, writer.Container, []StageTarget{{Root: roots[0], Path: rootPath}}, full, roots, 1<<30, 1<<52, checksum, nil); !apperr.Is(err, "RESTORE_CAPACITY") {
		t.Fatal("extracted into the reserve:", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(data, full, "new")); len(entries) != 0 {
		t.Fatal("wrote past the reserve", entries)
	}
	// An archive that ends inside a file fails as one, though its helper
	// reached the reserve on that file, and the unpack returns only once
	// that helper, slow to exit, has.
	truncated := "3f9619ff-8b86-4011-b42d-00c04fc964ff"
	var archive []byte
	for _, header := range []tarstream.Header{{Name: "snapshot/", Type: "directory", Mode: 0o755}, {Name: "snapshot/root-0/", Type: "directory", Mode: 0o755},
		{Name: "snapshot/root-0/world.dat", Type: "file", Mode: 0o644, Size: 1 << 20}} {
		encoded, _ := tarstream.EncodeHeader(header)
		archive = append(archive, encoded...)
	}
	os.WriteFile(filepath.Join(destination, truncated+".tar"), archive, 0o600)
	cut := ".ludock-restore-3f9619ff-8b86-4011-b42d-00c04fc964ff"
	if _, err := helpers.Exec(ctx, writer.Container, helper.Command("restore", helper.RestoreRequest{Root: rootPath, Stage: cut, Operation: "stage"}), nil); err != nil {
		t.Fatal(err)
	}
	var exited atomic.Bool
	engine.RunHelpers(func(_ *fakedocker.Container, _, args []string, stdin *bufio.Reader, stdout, stderr func([]byte)) (int, bool) {
		if len(args) == 0 || args[0] != "restore-extract" {
			return 0, false
		}
		var out, errs bytes.Buffer
		code := helper.Main(args, stdin, &out, &errs)
		if errs.Len() > 0 {
			stderr(errs.Bytes())
		}
		time.Sleep(100 * time.Millisecond)
		exited.Store(true)
		return code, true
	})
	err = helpers.ExtractToStage(ctx, destination, truncated, writer.Container, []StageTarget{{Root: roots[0], Path: rootPath}}, cut, roots, 1<<30, 1<<52, checksum, nil)
	if !apperr.Is(err, "INVALID_BACKUP") || !exited.Load() {
		t.Fatal(err, exited.Load())
	}
	engine.RunHelpers(nil)
	// A check of the requester's access that fails is reported as itself.
	refused := ".ludock-restore-4f9619ff-8b86-4011-b42d-00c04fc964ff"
	if _, err := helpers.Exec(ctx, writer.Container, helper.Command("restore", helper.RestoreRequest{Root: rootPath, Stage: refused, Operation: "stage"}), nil); err != nil {
		t.Fatal(err)
	}
	unreachable, checks := errors.New("database unreachable"), 0
	err = helpers.ExtractToStage(ctx, destination, id, writer.Container, []StageTarget{{Root: roots[0], Path: rootPath}}, refused, roots, 1<<30, 0, checksum,
		func() error {
			if checks++; checks > 1 {
				return unreachable
			}
			return nil
		})
	if err != unreachable {
		t.Fatal(err)
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
	storage := &Storage{Roots: []string{destination}, Own: self.New(nil, "")}
	helpers := &Helpers{Docker: client, Images: &launch.Images{Configured: image}, Storage: storage, pollInterval: 1}
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

// Docker accepts the archive command's start and never answers it while the
// server is stopped and the backup holds its locks.
func TestStalledArchiveStart(t *testing.T) {
	engine, client := startHelpers(t)
	base := t.TempDir()
	data := filepath.Join(base, "game")
	os.MkdirAll(data, 0o755)
	destination := filepath.Join(base, "backups")
	os.MkdirAll(destination, 0o755)
	storage := &Storage{Roots: []string{destination}, Own: self.New(nil, "")}
	helpers := &Helpers{Docker: client, Images: &launch.Images{Configured: image}, Storage: storage,
		pollInterval: 1, openLimit: 200 * time.Millisecond}
	context := serverContext(data)
	dataHelper, err := helpers.NewDataHelper(ctx, context, true, "op-4")
	if err != nil {
		t.Fatal(err)
	}
	defer dataHelper.Cleanup()
	engine.HangExecStarts = 1
	done := make(chan error, 1)
	go func() {
		_, _, err := helpers.WriteSnapshot(ctx, context, dataHelper, Settings{Destination: destination, MaxBytes: 1 << 30},
			"8f9619ff-8b86-4011-b42d-00c04fc964ff", 1<<30, func() error { return nil }, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if !apperr.Is(err, "BACKUP_TIMEOUT") {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the backup kept waiting for Docker to start its archive command")
	}
	if entries, _ := os.ReadDir(destination); len(entries) != 0 {
		t.Fatal("left an archive behind")
	}
}

// A root still being read at the backup's time limit fails as a timeout, not
// as an archive of unsupported files.
func TestSnapshotTimeLimit(t *testing.T) {
	_, client := startHelpers(t)
	base := t.TempDir()
	data := filepath.Join(base, "game")
	os.MkdirAll(data, 0o755)
	os.WriteFile(filepath.Join(data, "world.dat"), make([]byte, 1<<20), 0o644)
	destination := filepath.Join(base, "backups")
	os.MkdirAll(destination, 0o755)
	storage := &Storage{Roots: []string{destination}, Own: self.New(nil, "")}
	helpers := &Helpers{Docker: client, Images: &launch.Images{Configured: image}, Storage: storage,
		pollInterval: 1, readLimit: time.Nanosecond}
	context := serverContext(data)
	dataHelper, err := helpers.NewDataHelper(ctx, context, true, "op-5")
	if err != nil {
		t.Fatal(err)
	}
	defer dataHelper.Cleanup()
	_, _, err = helpers.WriteSnapshot(ctx, context, dataHelper, Settings{Destination: destination, MaxBytes: 1 << 30},
		"9f9619ff-8b86-4011-b42d-00c04fc964ff", 1<<30, func() error { return nil }, nil)
	if public, ok := apperr.As(err); !ok || public.Code != "BACKUP_TIMEOUT" || !strings.Contains(public.Message, "4 hours") {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(destination); len(entries) != 0 {
		t.Fatal("left an archive behind")
	}
}

// Entries are counted across all roots, as restore counts them, so a backup
// restore would refuse is never recorded.
func TestSnapshotEntryLimitSpansRoots(t *testing.T) {
	_, client := startHelpers(t)
	base := t.TempDir()
	destination := filepath.Join(base, "backups")
	os.MkdirAll(destination, 0o755)
	context := &servers.Context{}
	for index, name := range []string{"one", "two"} {
		data := filepath.Join(base, name)
		os.MkdirAll(data, 0o755)
		for file := range 2 {
			os.WriteFile(filepath.Join(data, strconv.Itoa(file)), []byte("x"), 0o644)
		}
		root := "/" + name
		context.Container.FileRoots = append(context.Container.FileRoots, files.Root{ID: "root-" + strconv.Itoa(index), Path: root})
		context.Observation.Mounts = append(context.Observation.Mounts, identity.Mount{Type: "bind", Source: data, Destination: root, Writable: true})
	}
	previous := maxEntries
	// The header and each root's folder and two files: seven in all.
	maxEntries = 6
	t.Cleanup(func() { maxEntries = previous })
	helpers := &Helpers{Docker: client, Images: &launch.Images{Configured: image}, Storage: &Storage{Roots: []string{destination}, Own: self.New(nil, "")},
		pollInterval: 1}
	dataHelper, err := helpers.NewDataHelper(ctx, context, true, "op-6")
	if err != nil {
		t.Fatal(err)
	}
	defer dataHelper.Cleanup()
	_, _, err = helpers.WriteSnapshot(ctx, context, dataHelper, Settings{Destination: destination, MaxBytes: 1 << 30},
		"af9619ff-8b86-4011-b42d-00c04fc964ff", 1<<30, func() error { return nil }, nil)
	if !apperr.Is(err, "UNSAFE_ARCHIVE") || !strings.Contains(err.Error(), "too many entries") {
		t.Fatal(err)
	}
}

// A snapshot is written as it streams. A real Valheim install, whose files
// reach 740 MB, made the old buffer grow with every chunk after the first
// header until Ludock ran out of memory.
func TestCoalescerNeverGrows(t *testing.T) {
	var written bytes.Buffer
	largest := 0
	out := newCoalescer(writerFunc(func(p []byte) (int, error) {
		largest = max(largest, len(p))
		return written.Write(p)
	}))
	var want bytes.Buffer
	for index, size := range []int{512, 65536, 65536, 100, 512, 70000, 65536, 3, 65536} {
		chunk := bytes.Repeat([]byte{byte(index)}, size)
		want.Write(chunk)
		if err := out.write(chunk); err != nil {
			t.Fatal(err)
		}
		if cap(out.buffer) != 64*1024 {
			t.Fatalf("buffer grew to %d", cap(out.buffer))
		}
	}
	if err := out.flush(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written.Bytes(), want.Bytes()) || largest > 70000 {
		t.Fatal("written out of order or in one piece", written.Len(), largest)
	}
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }
