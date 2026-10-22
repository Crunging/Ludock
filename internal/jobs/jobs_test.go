package jobs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	if err := f.runner.Start(ctx, nil); err != nil {
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

// Backup settings need the admin session still valid.
func TestBackupSettingsNeedAdminSession(t *testing.T) {
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

// Recovery keeps a backup whose row cannot be read.
func TestRecoveryWithUnreadableRow(t *testing.T) {
	f := setup(t)
	backup := f.run(t, "backup", nil)
	archives := func() []string {
		var found []string
		filepath.WalkDir(f.backupTo, func(path string, entry os.DirEntry, err error) error {
			if err == nil && !entry.IsDir() {
				found = append(found, path)
			}
			return nil
		})
		return found
	}
	before := archives()
	if _, err := f.jobs.DB.Exec("ALTER TABLE backups RENAME TO backups_unreadable"); err != nil {
		t.Fatal(err)
	}
	job := &ops.Context{Job: &ops.Job{ID: backup.ID, Recovery: map[string]any{"backupId": backup.Result["backupId"], "backupDestination": f.backupTo}}}
	if err := f.jobs.removeUnpublished(ctx, job); err == nil {
		t.Fatal("treated an unreadable backup row as absent")
	}
	if after := archives(); len(before) == 0 || !slices.Equal(before, after) {
		t.Fatal(before, after)
	}
}

// Interrupted pruning is never offered and is retried.
func TestInterruptedPruning(t *testing.T) {
	f := setup(t)
	if _, err := f.jobs.Backups.SaveSettings(ctx, backups.Settings{Destination: f.backupTo, RetentionCount: 1, MaxBytes: 1 << 30},
		f.admin, f.adminSession); err != nil {
		t.Fatal(err)
	}
	old := f.run(t, "backup", nil).Result["backupId"].(string)
	if _, err := f.jobs.DB.Exec(`CREATE TRIGGER no_prune BEFORE DELETE ON backups
		BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	f.run(t, "backup", nil)
	if backup, err := f.jobs.Backups.Get(ctx, f.serverID, old); backup != nil || err == nil {
		t.Fatal("offered a backup whose archive was removed", backup)
	}
	f.jobs.DB.Exec("DROP TRIGGER no_prune")
	f.run(t, "backup", nil)
	var left int
	f.jobs.DB.QueryRow("SELECT COUNT(*) FROM backups WHERE state != 'complete'").Scan(&left)
	if left != 0 {
		t.Fatal(left, "interrupted deletions left")
	}
}

// Backup deletion revoked before the unlink keeps the backup.
func TestRevokedBackupDeletion(t *testing.T) {
	f := setup(t)
	id := f.run(t, "backup", nil).Result["backupId"].(string)
	checks := 0
	err := f.jobs.Backups.Delete(ctx, f.serverID, id, func() error {
		if checks++; checks > 1 {
			return apperr.New("FORBIDDEN", 403, "Forbidden")
		}
		return nil
	})
	if !apperr.Is(err, "FORBIDDEN") || checks != 2 {
		t.Fatal(err, checks)
	}
	if backup, err := f.jobs.Backups.Get(ctx, f.serverID, id); err != nil || backup == nil {
		t.Fatal("lost a backup whose deletion was revoked", err)
	}
	if reader, err := f.jobs.Backups.Open(ctx, f.serverID, id); err != nil {
		t.Fatal(err)
	} else {
		reader.Close()
	}
}

// Unremoved failed archive is removed in the background.
func TestUnremovedArchiveCleanup(t *testing.T) {
	f := setup(t)
	server, _ := f.jobs.Identity.Get(ctx, f.serverID)
	id := "6f9619ff-8b86-4011-b42d-00c04fc964ff"
	if err := os.WriteFile(filepath.Join(f.backupTo, id+".tar"), []byte("unrecorded"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.jobs.Backups.RecordDeleting(ctx, id, f.serverID, server.BindingFingerprint, f.backupTo); err != nil {
		t.Fatal(err)
	}
	if backup, _ := f.jobs.Backups.Get(ctx, f.serverID, id); backup != nil {
		t.Fatal("offered an archive being deleted")
	}
	// No later backup has to succeed first: it may need the space this uses.
	if err := f.jobs.Backups.FinishDeletions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.backupTo, id+".tar")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("archive left behind", err)
	}
	if exists, _ := f.jobs.Backups.Exists(ctx, id, false); exists {
		t.Fatal("row left behind")
	}
}

// Update command stops when the owner loses authority.
func TestUpdateStopsOnLostAuthority(t *testing.T) {
	f := setup(t)
	job := &ops.Context{Job: &ops.Job{ID: "update", ServerID: f.serverID, ActorID: "admin", Kind: "update"}}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.jobs.whileAuthorized(ctx, job, authz.ServerUpdate, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return errors.New("compose was stopped")
		})
	}()
	<-started
	// The owner is demoted while Compose is running.
	if err := f.jobs.DB.UpdateUserAccess(ctx, "admin", db.RoleOperator, false); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if _, public := apperr.As(err); !public {
			t.Fatal("reported", err, "instead of the lost authority")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command kept running after the owner lost authority")
	}
}

// twoRootBackup backs up two 4 MiB data roots with a 1 MiB reserve.
func twoRootBackup(t *testing.T, f *fixture) any {
	t.Helper()
	world, config := filepath.Join(f.data, "world"), filepath.Join(f.data, "config")
	if err := os.MkdirAll(config, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{world, config} {
		if err := os.WriteFile(filepath.Join(root, "large.dat"), make([]byte, 4<<20), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.engine.Update(f.game, func(c *fakedocker.Container) { c.Config.Labels["ludock.files"] = world + "," + config })
	if _, err := f.jobs.Servers.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	logical, _ := f.jobs.Identity.Get(ctx, f.serverID)
	if _, err := f.jobs.Identity.ReviewBinding(ctx, f.admin.ID, f.adminSession, f.serverID, logical.PendingFingerprint,
		logical.BindingRevision); err != nil {
		t.Fatal(err)
	}
	settings, _, _ := f.jobs.Backups.SavedSettings(ctx)
	settings.ReserveBytes = 1 << 20
	if _, err := f.jobs.Backups.SaveSettings(ctx, *settings, f.admin, f.adminSession); err != nil {
		t.Fatal(err)
	}
	backup := f.run(t, "backup", nil)
	if backup.Status != ops.Succeeded {
		t.Fatal(backup)
	}
	return backup.Result["backupId"]
}

// spaceReports answers restore space probes with free(check), counting
// from 1, on one shared device.
func spaceReports(f *fixture, free func(check int) int) *int {
	run, checks := f.engine.Exec, new(int)
	f.engine.Exec = func(c *fakedocker.Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int {
		var request helper.RestoreRequest
		if len(cmd) > 3 && cmd[0] == helper.Binary && cmd[2] == "restore" && json.Unmarshal([]byte(cmd[3]), &request) == nil &&
			request.Operation == "space" {
			*checks++
			out, _ := json.Marshal(map[string]any{"availableBytes": free(*checks), "device": "1"})
			stdout(out)
			return 0
		}
		return run(c, cmd, stdin, stdout, stderr)
	}
	return checks
}

// A loss of authority is reported even when the command succeeds.
func TestAuthorityLostAsCommandEnds(t *testing.T) {
	f := setup(t)
	job := &ops.Context{Job: &ops.Job{ID: "update", ServerID: f.serverID, ActorID: "admin", Kind: "update"}}
	// The command finishes as the watcher sees the owner demoted.
	err := f.jobs.whileAuthorized(ctx, job, authz.ServerUpdate, func(ctx context.Context) error {
		if err := f.jobs.DB.UpdateUserAccess(ctx, "admin", db.RoleOperator, false); err != nil {
			t.Fatal(err)
		}
		<-ctx.Done()
		return nil
	})
	if _, public := apperr.As(err); !public {
		t.Fatal("reported", err, "for a command whose owner lost authority")
	}
	// Or the owner is demoted after the last poll.
	f.jobs.DB.UpdateUserAccess(ctx, "admin", db.RoleAdmin, false)
	err = f.jobs.whileAuthorized(ctx, job, authz.ServerUpdate, func(context.Context) error {
		return f.jobs.DB.UpdateUserAccess(ctx, "admin", db.RoleOperator, false)
	})
	if _, public := apperr.As(err); !public {
		t.Fatal("reported", err, "after a loss since the last poll")
	}
}

// Restore needs space for each root not the whole backup at each root.
func TestRestoreSpacePerRoot(t *testing.T) {
	f := setup(t)
	backupID := twoRootBackup(t, f)
	// Both roots share a filesystem with 10 MiB free, as both checks before
	// the server stops see: staging the first leaves 6 MiB, enough for the
	// second and the reserve.
	checks := spaceReports(f, func(check int) int {
		if check > 3 {
			return 6 << 20
		}
		return 10 << 20
	})
	restored := f.run(t, "restore", map[string]any{"backupId": backupID, "confirmation": "mc"})
	if restored.Status != ops.Succeeded || *checks != 4 {
		t.Fatal(restored.Status, restored.Error, *checks)
	}
}

// Roots sharing a disk must fit together before the server stops.
func TestRestoreSpacePerDisk(t *testing.T) {
	f := setup(t)
	backupID := twoRootBackup(t, f)
	before, _ := f.jobs.Backups.List(ctx, f.serverID)
	// Each root fits in 9 MiB on its own, but not both and the reserve.
	checks := spaceReports(f, func(int) int { return 9 << 20 })
	restored := f.run(t, "restore", map[string]any{"backupId": backupID, "confirmation": "mc"})
	if restored.Status != ops.Failed || !strings.Contains(restored.Error, "lacks space") || *checks != 2 {
		t.Fatal(restored.Status, restored.Error, *checks)
	}
	if after, _ := f.jobs.Backups.List(ctx, f.serverID); len(after) != len(before) {
		t.Fatal("stopped the server and took a safety backup for a restore that cannot fit")
	}
}

// A restore that cannot fit is refused before the server stops.
func TestRestoreSpaceCheckedBeforeStop(t *testing.T) {
	f := setup(t)
	backup := f.run(t, "backup", nil)
	if backup.Status != ops.Succeeded {
		t.Fatal(backup)
	}
	before, _ := f.jobs.Backups.List(ctx, f.serverID)
	run := f.engine.Exec
	f.engine.Exec = func(c *fakedocker.Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int {
		var request helper.RestoreRequest
		if len(cmd) > 3 && cmd[0] == helper.Binary && cmd[2] == "restore" && json.Unmarshal([]byte(cmd[3]), &request) == nil &&
			request.Operation == "space" {
			stdout([]byte(`{"availableBytes":0}`))
			return 0
		}
		return run(c, cmd, stdin, stdout, stderr)
	}
	restored := f.run(t, "restore", map[string]any{"backupId": backup.Result["backupId"], "confirmation": "mc"})
	if restored.Status != ops.Failed || !strings.Contains(restored.Error, "lacks space") {
		t.Fatal(restored.Status, restored.Error)
	}
	if game := f.engine.Container(f.game); !game.State.Running {
		t.Fatal("stopped the server for a restore that cannot fit")
	}
	if after, _ := f.jobs.Backups.List(ctx, f.serverID); len(after) != len(before) {
		t.Fatal("took a safety backup for a restore that cannot fit")
	}
}
