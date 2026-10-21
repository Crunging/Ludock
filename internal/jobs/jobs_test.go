package jobs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/console"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/monitor"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
)

var ctx = context.Background()

const image = "ghcr.io/crunging/ludock@sha256:abababababababababababababababababababababababababababababababab"

type fixture struct {
	engine   *fakedocker.Engine
	jobs     *Jobs
	runner   *ops.Runner
	serverID string
	game     string
	data     string
	backupTo string
	admin    *db.SessionUser
	// adminSession is the admin's token hash.
	adminSession string
	mu           sync.Mutex
	commands     []string
	failStep     string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	f := &fixture{engine: engine, admin: &db.SessionUser{ID: "admin", Username: "admin", Role: db.RoleAdmin}}
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
		f.mu.Lock()
		f.commands = append(f.commands, strings.Join(cmd, " "))
		failStep := f.failStep
		f.mu.Unlock()
		if len(cmd) > 2 && cmd[0] == helper.Binary {
			args := append([]string(nil), cmd[2:]...)
			if failStep != "" && strings.Contains(strings.Join(args, " "), `"operation":"`+failStep+`"`) {
				return 1
			}
			for _, mount := range c.CreateRequest.HostConfig.Mounts {
				for index := range args {
					args[index] = strings.ReplaceAll(args[index], `"`+mount.Target, `"`+mount.Source)
				}
			}
			var out, errs bytes.Buffer
			code := helper.Main(args, stdin, &out, &errs)
			stdout(out.Bytes())
			return code
		}
		// Game console commands behind the bounded wrapper succeed.
		return 0
	}
	base := t.TempDir()
	f.data = filepath.Join(base, "game")
	os.MkdirAll(filepath.Join(f.data, "world"), 0o755)
	os.WriteFile(filepath.Join(f.data, "world", "level.dat"), []byte("original"), 0o644)
	f.backupTo = filepath.Join(base, "backups")
	os.MkdirAll(f.backupTo, 0o755)
	f.game = engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true,
		Mounts: []docker.MountPoint{{Type: "bind", Source: f.data, Destination: f.data, RW: true}}})
	engine.AddContainer(fakedocker.GameOptions{ID: "ludock", Name: "ludock", Image: image, Running: true,
		Mounts: []docker.MountPoint{{Type: "bind", Source: f.backupTo, Destination: f.backupTo, RW: true}}})
	d := dbtest.Open(t)
	d.CreateUser(ctx, db.UserRecord{User: db.User{ID: "admin", Username: "admin", Role: db.RoleAdmin, CreatedAt: 1}, PasswordHash: "x"})
	client := docker.New(engine.Socket)
	table := locks.New()
	store := &identity.Store{DB: d}
	authorization := &authz.Service{DB: d, Servers: store}
	observer := &servers.Observer{Docker: client}
	service := &servers.Service{DB: d, Identity: store, Authz: authorization, Observer: observer}
	images := &launch.Images{Docker: client, Configured: image}
	storage := &backups.Storage{Docker: client, Roots: f.backupTo, Self: "ludock"}
	backupService := &backups.Service{DB: d, Storage: storage, Docker: client, Observer: observer, Locks: table,
		Helpers: &backups.Helpers{Docker: client, Images: images, Storage: storage}}
	f.runner = &ops.Runner{DB: d, Locks: table}
	f.jobs = &Jobs{DB: d, Authz: authorization, Identity: store, Servers: service, Docker: client, Locks: table,
		Monitor: &monitor.Service{DB: d, Locks: table, Store: store}, Console: &console.Runtime{Docker: client},
		Backups: backupService, SaveSettle: time.Millisecond, Poll: 20 * time.Millisecond}
	f.jobs.Register(f.runner)
	f.adminSession = dbtest.Session(t, d, f.admin.ID)
	views, err := service.List(ctx, f.admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		if view.Name == "mc" {
			f.serverID = view.ID
		}
	}
	if _, err := backupService.SaveSettings(ctx, backups.Settings{Destination: f.backupTo, RetentionCount: 5, MaxBytes: 1 << 30},
		f.admin, f.adminSession); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.runner.Stop)
	return f
}

func (f *fixture) run(t *testing.T, kind string, input map[string]any) *ops.Job {
	t.Helper()
	server, _ := f.jobs.Identity.Get(ctx, f.serverID)
	job, err := f.runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: "admin", Kind: kind,
		BindingRevision: server.BindingRevision, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		current, _ := f.runner.Get(ctx, job.ID)
		if current.Status != ops.Queued && current.Status != ops.Running {
			return current
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return nil
}

func (f *fixture) running() bool { return f.engine.Container(f.game).State.Running }

func TestRecoveryStopsWhenCancelled(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.jobs.Poll = time.Hour
	stopping := &ops.Context{Job: &ops.Job{ID: "interrupted", Recovery: map[string]any{"stopStarted": true,
		"stopRecoveryDeadlineAt": float64(time.Now().Add(time.Hour).UnixMilli())}}}
	for name, wait := range map[string]func() error{
		"helpers": func() error {
			return f.jobs.removeOperationHelpers(ctx, &ops.Context{Job: &ops.Job{ID: "interrupted"}})
		},
		"stop": func() error { return f.jobs.waitForStopDeadline(ctx, stopping) },
	} {
		done := make(chan error, 1)
		go func() { done <- wait() }()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(name, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal(name, "recovery kept waiting after cancellation")
		}
	}
}

func TestBackupSettingsNeedTheAdminSessionStillValid(t *testing.T) {
	f := setup(t)
	if err := f.jobs.DB.DeleteUserSessions(ctx, f.admin.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.jobs.Backups.SaveSettings(ctx, backups.Settings{Destination: f.backupTo, RetentionCount: 1, MaxBytes: 1 << 30},
		f.admin, f.adminSession)
	if !apperr.Is(err, "AUTHENTICATION_REQUIRED") {
		t.Fatal(err)
	}
	if saved, _, _ := f.jobs.Backups.SavedSettings(ctx); saved == nil || saved.RetentionCount != 5 {
		t.Fatal(saved)
	}
}

func TestRevokedOwnerCannotRun(t *testing.T) {
	f := setup(t)
	f.jobs.DB.UpdateUserAccess(ctx, "admin", db.RoleAdmin, true)
	job := f.run(t, "stop", nil)
	if job.Status != ops.Failed || job.Error != "The operation owner no longer has access" || !f.running() {
		t.Fatalf("%+v", job)
	}
}

func TestFailedRestoreRollsBack(t *testing.T) {
	f := setup(t)
	backup := f.run(t, "backup", nil)
	backupID := backup.Result["backupId"].(string)
	os.WriteFile(filepath.Join(f.data, "world", "level.dat"), []byte("newer"), 0o644)
	f.mu.Lock()
	f.failStep = "moveNew"
	f.mu.Unlock()
	restore := f.run(t, "restore", map[string]any{"backupId": backupID, "confirmation": "mc"})
	if restore.Status != ops.Failed {
		t.Fatalf("%+v", restore)
	}
	if data, _ := os.ReadFile(filepath.Join(f.data, "world", "level.dat")); string(data) != "newer" {
		t.Fatal("rollback lost the current data:", string(data))
	}
	entries, _ := os.ReadDir(f.data)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".ludock-restore-") {
			t.Fatal("stage left behind")
		}
	}
	if !f.running() {
		t.Fatal("server not returned to running after a safe rollback")
	}
	if wrong := f.run(t, "restore", map[string]any{"backupId": backupID, "confirmation": "wrong"}); wrong.Status != ops.Failed {
		t.Fatal("restore without confirmation ran")
	}
}
