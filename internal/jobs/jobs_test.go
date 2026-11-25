package jobs

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	"github.com/Crunging/Ludock/internal/mountinfo"
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/self"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/update"
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
	// wasStopped makes interrupt record a server that was stopped.
	wasStopped bool
	// adminSession is the admin's token hash.
	adminSession string
	mu           sync.Mutex
	commands     []string
	failStep     string
	// failTimes, when positive, is how many more times failStep fails.
	failTimes int
	// alerts are the messages sent, in order.
	alerts []string
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
	engine.RunHelpers(func(_ *fakedocker.Container, cmd, args []string, _ *bufio.Reader, _, _ func([]byte)) (int, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.commands = append(f.commands, strings.Join(cmd, " "))
		if args == nil {
			// Game console commands behind the bounded wrapper succeed.
			return 0, true
		}
		if f.failStep == "" || !strings.Contains(strings.Join(args, " "), `"operation":"`+f.failStep+`"`) || f.failTimes < 0 {
			return 0, false
		}
		if f.failTimes > 0 {
			if f.failTimes--; f.failTimes == 0 {
				f.failTimes = -1
			}
		}
		return 1, true
	})
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
	f.jobs = wire(engine, d, f.backupTo)
	f.runner = &ops.Runner{DB: d, Locks: f.jobs.Locks}
	f.jobs.Notify = func(_ notify.Alert, _, message string) {
		f.mu.Lock()
		f.alerts = append(f.alerts, message)
		f.mu.Unlock()
	}
	service.Looping = f.jobs.Monitor.Looping
	f.jobs.Register(f.runner)
	f.adminSession = dbtest.Session(t, d, f.admin.ID)
	service, backupService := f.jobs.Servers, f.jobs.Backups
	views, err := service.List(ctx, f.admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		if view.Name == "mc" {
			f.serverID = view.ID
		}
	}
	if _, err := backupService.SaveSettings(ctx, backups.Settings{Destination: f.backupTo, MaxBytes: 1 << 30},
		f.admin, f.adminSession); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.runner.Stop)
	return f
}

// wire builds Ludock's services on engine and d, as Ludock does, with
// backupRoots its approved backup folders and short waits.
func wire(engine *fakedocker.Engine, d *db.DB, backupRoots ...string) *Jobs {
	client := docker.New(engine.Socket)
	client.OutageLimit = 6 * time.Second
	table := locks.New()
	store := &identity.Store{DB: d}
	authorization := &authz.Service{DB: d, Servers: store}
	observer := &servers.Observer{Docker: client, Own: self.New(nil, "")}
	service := &servers.Service{DB: d, Identity: store, Authz: authorization, Observer: observer}
	images := &launch.Images{Configured: image}
	storage := &backups.Storage{Roots: backupRoots, Own: self.New(client, "ludock")}
	backupService := &backups.Service{DB: d, Storage: storage, Docker: client, Observer: observer, Locks: table,
		Helpers: &backups.Helpers{Docker: client, Images: images, Storage: storage}}
	return &Jobs{DB: d, Authz: authorization, Identity: store, Servers: service, Docker: client, Locks: table,
		Monitor: &monitor.Service{DB: d, Locks: table, Store: store}, Console: &console.Runtime{Docker: client},
		Backups: backupService, Updates: &update.Service{Docker: client}, SaveSettle: time.Millisecond, ReplacementSettle: 100 * time.Millisecond,
		Poll: 20 * time.Millisecond}
}

func (f *fixture) run(t *testing.T, kind string, input map[string]any) *ops.Job {
	t.Helper()
	server, _ := f.jobs.Identity.Get(ctx, f.serverID)
	job, err := f.runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: "admin", Kind: kind,
		BindingRevision: server.BindingRevision, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	return finished(t, f.runner, job.ID)
}

// finished waits for a job to end, until shortly before go test's own
// deadline: a loaded machine slows a job down many times over, and one that
// hangs still fails.
func finished(t *testing.T, runner *ops.Runner, id string) *ops.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Minute)
	if end, ok := t.Deadline(); ok {
		deadline = end.Add(-min(10*time.Second, time.Until(end)/10))
	}
	for ; time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if current, _ := runner.Get(ctx, id); current.Status != ops.Queued && current.Status != ops.Running {
			return current
		}
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
	stopping := &ops.Context{Job: &ops.Job{ID: "interrupted", Recovery: ops.Recovery{StopStarted: true,
		StopRecoveryDeadlineAt: time.Now().Add(time.Hour).UnixMilli()}}}
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
	_, err := f.jobs.Backups.SaveSettings(ctx, backups.Settings{Destination: f.backupTo, MaxBytes: 1 << 20},
		f.admin, f.adminSession)
	if !apperr.Is(err, "AUTHENTICATION_REQUIRED") {
		t.Fatal(err)
	}
	if saved, _, _ := f.jobs.Backups.SavedSettings(ctx); saved == nil || saved.MaxBytes != 1<<30 {
		t.Fatal(saved)
	}
}

// An owner disabled before their stop runs, or just as work that stops the
// server first checks them in a write, stops nothing, removes no backup, and
// leaves outage tracking as it was.
func TestRevokedOwnerCannotRun(t *testing.T) {
	f := setup(t)
	f.jobs.DB.UpdateUserAccess(ctx, "admin", db.RoleAdmin, true)
	job := f.run(t, "stop", nil)
	if job.Status != ops.Failed || job.Error != "The person who asked for this can no longer use the server" || !f.running() {
		t.Fatalf("%+v", job)
	}
	f.jobs.DB.UpdateUserAccess(ctx, "admin", db.RoleAdmin, false)
	backup := f.run(t, "backup", nil)
	f.jobs.DB.Exec("DELETE FROM availability")
	f.jobs.Authz.Rechecking = func(tx *sql.Tx) { tx.Exec("UPDATE users SET disabled=1 WHERE id='admin'") }
	for _, kind := range []string{"stop", "backup", "update"} {
		var input map[string]any
		if kind == "update" {
			input = skipBackup
		}
		job = f.run(t, kind, input)
		var paused int
		f.jobs.DB.QueryRow("SELECT COUNT(*) FROM availability WHERE suppressed_until > 0").Scan(&paused)
		if kept, _ := f.jobs.Backups.Exists(ctx, backup.Result.BackupID, true); job.Status != ops.Failed || paused != 0 ||
			!f.running() || !kept {
			t.Fatalf("%s: %+v paused=%d kept=%v", kind, job, paused, kept)
		}
	}
}

func TestFailedRestoreRollsBack(t *testing.T) {
	f := setup(t)
	backup := f.run(t, "backup", nil)
	backupID := backup.Result.BackupID
	os.WriteFile(filepath.Join(f.data, "world", "level.dat"), []byte("newer"), 0o644)
	f.mu.Lock()
	f.failStep = helper.RestoreMoveNew
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

// A restore whose staging cleanup fails after the new data is in place
// cleans up through a fresh helper; if that fails too, it says which folder
// holds the replaced data.
func TestRestoreCleanupAfterCommit(t *testing.T) {
	f := setup(t)
	backup := f.run(t, "backup", nil)
	backupID := backup.Result.BackupID
	os.WriteFile(filepath.Join(f.data, "world", "level.dat"), []byte("newer"), 0o644)
	staged := func() bool {
		entries, _ := os.ReadDir(f.data)
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".ludock-restore-") {
				return true
			}
		}
		return false
	}
	f.mu.Lock()
	f.failStep, f.failTimes = helper.RestoreCleanup, 1
	f.mu.Unlock()
	restored := f.run(t, "restore", map[string]any{"backupId": backupID, "confirmation": "mc"})
	if restored.Status != ops.Succeeded || staged() || !f.running() {
		t.Fatalf("%+v", restored)
	}
	if data, _ := os.ReadFile(filepath.Join(f.data, "world", "level.dat")); string(data) == "newer" {
		t.Fatal("the backup was not restored")
	}
	f.mu.Lock()
	f.failStep, f.failTimes = helper.RestoreCleanup, 0
	f.mu.Unlock()
	stuck := f.run(t, "restore", map[string]any{"backupId": backupID, "confirmation": "mc"})
	if stuck.Status != ops.Failed || !strings.Contains(stuck.Error, ".ludock-restore-"+stuck.ID) || !staged() || !f.running() {
		t.Fatalf("%+v", stuck)
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
	job := &ops.Context{Job: &ops.Job{ID: backup.ID, Recovery: ops.Recovery{BackupID: backup.Result.BackupID, BackupDestination: f.backupTo}}}
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
	// The fourth backup of a day is the first retention removes.
	old := f.run(t, "backup", nil).Result.BackupID
	f.run(t, "backup", nil)
	f.run(t, "backup", nil)
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
	if err := f.jobs.DB.UpdateUserAccess(ctx, "admin", db.RoleMember, false); err != nil {
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
	if _, err := f.jobs.Servers.RefreshNow(ctx); err != nil {
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
	return backup.Result.BackupID
}

// spaceReports answers restore space probes with free(check), counting
// from 1, on one shared device.
func spaceReports(f *fixture, free func(check int) (bytes int, device string)) *int {
	run, checks := f.engine.Exec, new(int)
	f.engine.Exec = func(c *fakedocker.Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int {
		var request helper.RestoreRequest
		if len(cmd) > 3 && cmd[0] == helper.Binary && cmd[2] == "restore" && json.Unmarshal([]byte(cmd[3]), &request) == nil &&
			request.Operation == helper.RestoreSpace {
			*checks++
			bytes, device := free(*checks)
			out, _ := json.Marshal(map[string]any{"availableBytes": bytes, "device": device})
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
		if err := f.jobs.DB.UpdateUserAccess(ctx, "admin", db.RoleMember, false); err != nil {
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
		return f.jobs.DB.UpdateUserAccess(ctx, "admin", db.RoleMember, false)
	})
	if _, public := apperr.As(err); !public {
		t.Fatal("reported", err, "after a loss since the last poll")
	}
}

// Restore needs space for each root not the whole backup at each root.
func TestRestoreSpacePerRoot(t *testing.T) {
	f := setup(t)
	backupID := twoRootBackup(t, f)
	// Each root has its own disk with 6 MiB free: enough for its 4 MiB and
	// the reserve, though not for both roots.
	checks := spaceReports(f, func(check int) (int, string) { return 6 << 20, strconv.Itoa(check % 2) })
	// Both roots stage from one pass over the archive.
	for _, root := range []string{"world", "config"} {
		if err := os.WriteFile(filepath.Join(f.data, root, "large.dat"), []byte("changed"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	restored := f.run(t, "restore", map[string]any{"backupId": backupID, "confirmation": "mc"})
	if restored.Status != ops.Succeeded || *checks != 4 {
		t.Fatal(restored.Status, restored.Error, *checks)
	}
	for _, root := range []string{"world", "config"} {
		if info, err := os.Stat(filepath.Join(f.data, root, "large.dat")); err != nil || info.Size() != 4<<20 {
			t.Fatal(root, "was not restored:", err)
		}
	}
}

// Roots sharing a disk must fit together, or the restore is refused before
// the server stops.
func TestRestoreSpacePerDisk(t *testing.T) {
	f := setup(t)
	backupID := twoRootBackup(t, f)
	before, _ := f.jobs.Backups.List(ctx, f.serverID)
	// Each root fits in 9 MiB on its own, but not both and the reserve.
	checks := spaceReports(f, func(int) (int, string) { return 9 << 20, "1" })
	restored := f.run(t, "restore", map[string]any{"backupId": backupID, "confirmation": "mc"})
	if restored.Status != ops.Failed || !strings.Contains(restored.Error, "doesn’t have room to unpack") || *checks != 2 {
		t.Fatal(restored.Status, restored.Error, *checks)
	}
	if after, _ := f.jobs.Backups.List(ctx, f.serverID); len(after) != len(before) || !f.running() {
		t.Fatal("stopped the server or took a safety backup for a restore that cannot fit")
	}
}

// Ludock restarts after dispatching a stop whose owner has since been
// disabled: Docker may still be stopping the server, so recovery keeps its
// lock until the stop's deadline before reporting the lost authority.
func TestRecoveryHoldsADispatchedStop(t *testing.T) {
	f := setup(t)
	logical, _ := f.jobs.Identity.Get(ctx, f.serverID)
	// Recovery keeps the deadline in milliseconds, so the test does too.
	deadline := time.UnixMilli(time.Now().Add(time.Second).UnixMilli())
	job := &ops.Context{Job: &ops.Job{ID: "stop", ServerID: f.serverID, ActorID: "admin", Kind: "stop",
		BindingRevision: logical.BindingRevision, Recovery: ops.Recovery{ContainerID: logical.ContainerID,
			StopStarted: true, StopRecoveryDeadlineAt: deadline.UnixMilli(), LockKeys: []string{locks.ServerKey(f.serverID)}}}}
	if err := f.jobs.DB.UpdateUserAccess(ctx, "admin", db.RoleAdmin, true); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.jobs.recoverLifecycle(ctx, job)
		done <- err
	}()
	for waited := time.Now(); !f.jobs.Locks.ServerBusy(f.serverID); time.Sleep(time.Millisecond) {
		select {
		case err := <-done:
			t.Fatal("recovery ended without holding the server:", err)
		default:
		}
		if time.Since(waited) > 10*time.Second {
			t.Fatal("recovery never held the server")
		}
	}
	if release, err := f.jobs.Locks.Acquire([]string{locks.ServerKey(f.serverID)}, nil); err == nil {
		release()
		t.Fatal("new work could take the server while its stop may still be running")
	}
	err := <-done
	if _, public := apperr.As(err); !public || time.Now().Before(deadline) {
		t.Fatal("reported", err, "before the stop's deadline")
	}
}

// An old backup's archive is gone from the backup folder, though its record
// alone fills the total limit. A new backup still fits: only archives Ludock
// finds use the limit.
func TestBackupLimitCountsFoundArchives(t *testing.T) {
	f := setup(t)
	if _, err := f.jobs.DB.Exec(`INSERT INTO backups(id,server_id,binding_fingerprint,destination,roots_json,size,checksum,created_at,state,game_type)
    VALUES(?,?,'x',?,'[]',?,'',1,'complete','')`, identity.NewUUID(), f.serverID, f.backupTo, int64(1<<30)); err != nil {
		t.Fatal(err)
	}
	if job := f.run(t, "backup", nil); job.Status != ops.Succeeded {
		t.Fatalf("%+v", job)
	}
}

// Compose replaces a container to change it. A backup whose stopped server
// is replaced meanwhile says something outside Ludock changed it, rather
// than that something went wrong.
func TestBackupOfReplacedServer(t *testing.T) {
	f := setup(t)
	validate := f.engine.OnStart
	f.engine.OnStart = func(c *fakedocker.Container) {
		validate(c)
		// The helper that copies, not the one checking before the stop.
		if c.CreateRequest.Labels["ludock.internal"] == "backup-helper" && !f.running() {
			f.engine.RemoveContainer(f.game)
		}
	}
	job := f.run(t, "backup", nil)
	if job.Status != ops.Failed || !strings.HasPrefix(job.Error, "Something outside Ludock started or changed the server") {
		t.Fatalf("%+v", job)
	}
}

// Docker fails every request right after a restart stopped the server. The
// restart fails and the server stays stopped; Ludock keeps running.
func TestRestartWhenDockerGoesAway(t *testing.T) {
	f := setup(t)
	f.jobs.Docker.OutageLimit = 300 * time.Millisecond
	f.engine.OnStop = func(c *fakedocker.Container) {
		if c.ID == f.game {
			f.engine.AddFault(`^GET .*/containers/`, 500, false, 0)
		}
	}
	job := f.run(t, "restart", nil)
	f.engine.ClearFaults()
	if job.Status != ops.Failed || f.running() || !strings.HasPrefix(job.Error, "Docker reported an error") {
		t.Fatalf("%+v", job)
	}
}

// Docker refuses to create a backup's data helper once its mounts are
// proven. The backup blames Docker, not the server's data folders, and the
// server runs again.
func TestBackupHelperCreateFails(t *testing.T) {
	f := setup(t)
	validate := f.engine.OnStart
	f.engine.OnStart = func(c *fakedocker.Container) {
		validate(c)
		if c.CreateRequest.Labels["ludock.internal"] == "mount-validator" {
			f.engine.AddFault(`^POST .*/containers/create`, 500, false, 0)
		}
	}
	job := f.run(t, "backup", nil)
	f.engine.ClearFaults()
	if job.Status != ops.Failed || !strings.HasPrefix(job.Error, "Docker reported an error when Ludock started its helper") || !f.running() {
		t.Fatalf("%+v", job)
	}
}

// A server Docker had to kill when it was stopped outside Ludock, and whose
// outage was already alerted, is backed up as it is, and its outage stays
// alerted, so the backup doesn't bring a second alert.
func TestBackupOfServerKilledElsewhere(t *testing.T) {
	f := setup(t)
	f.engine.Update(f.game, func(c *fakedocker.Container) {
		c.State.Running, c.State.Status, c.State.ExitCode = false, "exited", 137
	})
	if err := f.jobs.Monitor.SetIntentionalStop(ctx, f.serverID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.jobs.DB.Exec("UPDATE availability SET outage_started_at=5,notified=1 WHERE server_id=?", f.serverID); err != nil {
		t.Fatal(err)
	}
	job := f.run(t, "backup", nil)
	var started, notified int64
	f.jobs.DB.QueryRow("SELECT outage_started_at,notified FROM availability WHERE server_id=?", f.serverID).Scan(&started, &notified)
	if job.Status != ops.Succeeded || f.running() || started != 5 || notified != 1 {
		t.Fatalf("%+v, outage %d, notified %d", job, started, notified)
	}
}

// Docker fails just as a backup's archive is read, comes back from a
// restart without the archive's exec, or fails to start the archive's
// command, as a daemon stalled at that moment does. A failing Docker fails
// the backup, which blames Docker, not the server's files; a lost or
// unstarted exec is copied again.
func TestBackupWhenDockerGoesAway(t *testing.T) {
	for _, status := range []int{500, 404, 0} {
		f := setup(t)
		if status == 0 {
			var once sync.Once
			f.engine.FailExecStart = func(cmd []string) bool {
				failed := false
				if strings.Contains(strings.Join(cmd, " "), `"operation":"backup"`) {
					once.Do(func() { failed = true })
				}
				return failed
			}
			if job := f.run(t, "backup", nil); job.Status != ops.Succeeded || !f.running() {
				t.Fatalf("unstarted: %+v", job)
			}
			continue
		}
		f.jobs.Docker.OutageLimit = 300 * time.Millisecond
		exec := f.engine.Exec
		var once sync.Once
		f.engine.Exec = func(c *fakedocker.Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int {
			if strings.Contains(strings.Join(cmd, " "), `"operation":"backup"`) {
				once.Do(func() { f.engine.AddFault(`/exec/`, status, false, 0) })
			}
			return exec(c, cmd, stdin, stdout, stderr)
		}
		if status == 404 {
			// The restarted Docker knows the execs made after it.
			validate := f.engine.OnStart
			f.engine.OnStart = func(c *fakedocker.Container) {
				validate(c)
				if c.CreateRequest.Labels["ludock.internal"] == "mount-validator" {
					f.engine.ClearFaults()
				}
			}
		}
		job := f.run(t, "backup", nil)
		f.engine.ClearFaults()
		if status == 500 && (job.Status != ops.Failed || !strings.HasPrefix(job.Error, "Docker reported an error")) {
			t.Fatalf("%d: %+v", status, job)
		}
		if status == 404 && job.Status != ops.Succeeded {
			t.Fatalf("%d: %+v", status, job)
		}
	}
}

// A server started with --rm has an unnamed volume, which Docker deletes
// with it when it stops. Every kind of work that would stop
// it is refused with one message before it acts, and so is a stop or a
// restart reaching the shared path to Docker directly: the server and its
// volume stay.
func TestDeletedOnStopServerIsNeverStopped(t *testing.T) {
	f := setup(t)
	volume := strings.Repeat("a", 64)
	if _, err := f.jobs.Docker.CreateVolume(ctx, volume); err != nil {
		t.Fatal(err)
	}
	f.engine.Update(f.game, func(c *fakedocker.Container) {
		c.HostConfig = map[string]any{"AutoRemove": true}
		c.Mounts = append(c.Mounts, docker.MountPoint{Type: "volume", Name: volume, Source: "/var/lib/docker/volumes/" + volume + "/_data",
			Destination: "/mods"})
	})
	if _, err := f.jobs.Servers.RefreshNow(ctx); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{ops.KindStop, ops.KindRestart, ops.KindBackup, ops.KindRestore, ops.KindUndoRestore, ops.KindUpdate} {
		job := f.run(t, kind, nil)
		if job.Status != ops.Failed || job.Error != servers.AutoRemoveMessage || f.engine.Container(f.game) == nil || !f.running() {
			t.Fatalf("%s: %s, container kept: %v", kind, job.Error, f.engine.Container(f.game) != nil)
		}
	}
	for _, action := range []servers.Action{servers.Stop, servers.Restart} {
		if err := f.jobs.Servers.Observer.ChangeState(ctx, f.game, action, nil); !apperr.Is(err, "SERVER_DELETED_ON_STOP") {
			t.Fatal(action, err)
		}
	}
	volumes, _ := f.jobs.Docker.ListVolumes(ctx)
	if f.engine.Container(f.game) == nil || !slices.ContainsFunc(volumes, func(v docker.VolumeInspect) bool { return v.Name == volume }) ||
		slices.ContainsFunc(f.engine.Requests, func(request string) bool {
			return strings.Contains(request, "/stop") || strings.Contains(request, "/restart")
		}) {
		t.Fatal(f.engine.Requests)
	}
}

// A server's data folder has another mount inside it, first one Docker
// declares and then one only the host has (a second disk under the bind
// source). A backup, a restart or update that backs up first, and a restore
// are each refused before players are warned or the server stops, so a
// nightly restart with a backup never takes it offline. The host's mount is
// found by the helper's mount table, faked here since tests can't mount;
// the Backups tab then reports it until someone checks again.
func TestImpossibleBackupNeverStopsServer(t *testing.T) {
	f := setup(t)
	backupID := f.run(t, "backup", nil).Result.BackupID
	mods := filepath.Join(f.data, "world", "mods")
	os.MkdirAll(mods, 0o755)
	warned := map[string]any{WarnInput: true}
	kinds := []struct {
		kind  string
		input map[string]any
	}{
		{"backup", warned},
		{"restart", map[string]any{"createBackup": true, WarnInput: true}},
		{"update", map[string]any{"request": map[string]any{"createBackup": true}, WarnInput: true}},
		{"restore", map[string]any{"backupId": backupID, "confirmation": "mc", MovedInput: true, WarnInput: true}},
	}
	refused := func(how, message string) {
		t.Helper()
		for _, c := range kinds {
			before := len(f.engine.Requests)
			job := f.run(t, c.kind, c.input)
			stopped := slices.ContainsFunc(f.engine.Requests[before:], func(request string) bool { return strings.Contains(request, f.game+"/stop") })
			if job.Status != ops.Failed || job.Error != message || job.Recovery.WarnUntil != 0 || stopped || !f.running() {
				t.Errorf("%s: %s: %s %q, warned %v, stopped %v", how, c.kind, job.Status, job.Error, job.Recovery.WarnUntil != 0, stopped)
			}
		}
	}
	// Backups fill the total limit, or leave less than an empty archive
	// needs. Retention never removes the newest backup, so there is no room.
	limit := func(bytes int64) {
		t.Helper()
		if _, err := f.jobs.Backups.SaveSettings(ctx, backups.Settings{Destination: f.backupTo, MaxBytes: bytes}, f.admin, f.adminSession); err != nil {
			t.Fatal(err)
		}
	}
	found, _, err := f.jobs.Backups.FoundBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for room, problem := range map[int64]string{0: "Backups have reached their total size limit.", 2560: "The backup doesn’t fit under the total size limit."} {
		limit(found + room)
		refused("full", problem+" Delete backups you no longer need, or raise the limit in Settings.")
	}
	limit(1 << 30)
	// Docker can't change a container's mounts; a label changed with them
	// stands in for the new container a real change makes.
	redefine := func(change func(c *fakedocker.Container)) {
		f.engine.Update(f.game, func(c *fakedocker.Container) { change(c); c.Config.Labels["revision"] += "+" })
		if _, err := f.jobs.Servers.RefreshNow(ctx); err != nil {
			t.Fatal(err)
		}
	}
	redefine(func(c *fakedocker.Container) {
		c.Mounts = append(c.Mounts, docker.MountPoint{Type: "bind", Source: mods, Destination: mods, RW: true})
	})
	refused("declared", "One of the server’s data folders has another mount inside it, which Ludock can’t back up as one folder. List folders without one inside in the ludock.files label, then try again.")
	redefine(func(c *fakedocker.Container) { c.Mounts = c.Mounts[:1] })
	previous := mountinfo.Table
	t.Cleanup(func() { mountinfo.Table = previous })
	mountinfo.Table = func() ([]byte, error) { return []byte("40 22 0:45 / " + mods + " rw - nfs4 server:/share rw\n"), nil }
	refused("host", backups.ErrHostMount.Message)
	server, err := f.jobs.Servers.ResolveAuthorized(ctx, f.admin, f.serverID, authz.BackupsCreate, -1)
	if err != nil {
		t.Fatal(err)
	}
	has := func() bool {
		return slices.ContainsFunc(f.jobs.Backups.Preflight(ctx, server).Issues, func(issue backups.Issue) bool { return issue.Code == "NESTED_HOST_MOUNT" })
	}
	if !has() || !f.jobs.Backups.HostMounted(f.serverID, server.Logical.BindingRevision) {
		t.Fatal("readiness doesn't report the host's mount")
	}
	f.jobs.Backups.ForgetHostMount(f.serverID)
	if has() {
		t.Fatal("still reported after checking again")
	}
	// A mount first met while copying leaves a restart that backs up first
	// stopped, and is reported too.
	mountinfo.Table = func() ([]byte, error) {
		if f.running() {
			return nil, nil
		}
		return []byte("40 22 0:45 / " + mods + " rw - nfs4 server:/share rw\n"), nil
	}
	job := f.run(t, "restart", map[string]any{"createBackup": true})
	if job.Status != ops.Failed || job.Error != backups.ErrHostMount.Message || f.running() || !has() {
		t.Fatalf("copying: %s %q, running %v", job.Status, job.Error, f.running())
	}
}

// A server has three safety backups, then three of its own, and less room
// under the total limit than its next backup: the limit was lowered, its
// data grew, or another server's backup took the room. Before the server
// stops, a backup, a restart or update that backs up first, and a restore
// remove what retention would remove after it, safety backups oldest first
// and never a restore's source, so the backup fits on its first try. With
// nothing removable, it still tries, and fails without room. The Backups tab
// offers each backup. Traces show
// which of the server's backups (oldest first) are left as the stop arrives.
func TestBackupMakesRoomBeforeTheStop(t *testing.T) {
	cases := []struct {
		name, kind string
		input      func(ids []string) map[string]any
		// room under the limit, in backup sizes
		room float64
		// other: another server's backup first takes one backup's room.
		other, keep bool
		// revoked: access is lost after the first removal.
		revoked bool
		want    string
		failed  bool
	}{
		{name: "backup", kind: "backup", room: 0.95, want: "1 2 3 4 5"},
		{name: "lowered limit", kind: "restart", input: func([]string) map[string]any { return map[string]any{"createBackup": true} },
			room: 0, want: "2 3 4 5"},
		{name: "update", kind: "update", input: func([]string) map[string]any {
			return map[string]any{"request": map[string]any{"createBackup": true}}
		}, room: 0.95, want: "1 2 3 4 5"},
		{name: "restore", kind: "restore", input: func(ids []string) map[string]any {
			return map[string]any{"backupId": ids[0], "confirmation": "mc", MovedInput: true}
		}, room: 0.95, want: "0 2 3 4 5"},
		{name: "other server", kind: "backup", room: 1.95, other: true, want: "1 2 3 4 5"},
		{name: "nothing removable", kind: "backup", room: 0.5, keep: true, want: "0 1 2 3 4 5", failed: true},
		{name: "access lost while making room", room: 0, revoked: true, want: "1 2 3 4 5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			var ids []string
			for index := range 6 {
				ids = append(ids, f.run(t, "backup", nil).Result.BackupID)
				if index < 3 {
					f.jobs.DB.Exec("UPDATE backups SET reason='restore' WHERE id=?", ids[index])
				}
			}
			if c.keep {
				f.jobs.DB.Exec("UPDATE backups SET kept_at=1 WHERE id IN (?,?,?)", ids[0], ids[1], ids[2])
			}
			newest, _ := f.jobs.Backups.Get(ctx, f.serverID, ids[5])
			used, _, _ := f.jobs.Backups.FoundBytes(ctx)
			limit := used + int64(c.room*float64(newest.Size))
			if _, err := f.jobs.Backups.SaveSettings(ctx, backups.Settings{Destination: f.backupTo, MaxBytes: limit}, f.admin, f.adminSession); err != nil {
				t.Fatal(err)
			}
			if c.other {
				data := filepath.Join(t.TempDir(), "game")
				os.MkdirAll(filepath.Join(data, "world"), 0o755)
				os.WriteFile(filepath.Join(data, "world", "level.dat"), []byte("original"), 0o644)
				f.engine.AddContainer(fakedocker.GameOptions{Name: "other", Image: "itzg/minecraft-server", Running: true,
					Mounts: []docker.MountPoint{{Type: "bind", Source: data, Destination: data, RW: true}}})
				if _, err := f.jobs.Servers.RefreshNow(ctx); err != nil {
					t.Fatal(err)
				}
				views, _ := f.jobs.Servers.List(ctx, f.admin)
				own := f.serverID
				for _, view := range views {
					if view.Name == "other" {
						f.serverID = view.ID
					}
				}
				if job := f.run(t, "backup", nil); job.Status != ops.Succeeded {
					t.Fatalf("other server: %+v", job)
				}
				f.serverID = own
			}
			// The Backups tab offers the backup.
			server, err := f.jobs.Servers.ResolveAuthorized(ctx, f.admin, f.serverID, authz.BackupsCreate, -1)
			if err != nil {
				t.Fatal(err)
			}
			if preflight := f.jobs.Backups.Preflight(ctx, server); !preflight.Ready {
				t.Errorf("not ready: %+v", preflight.Issues)
			}
			left := func() string {
				var left []string
				for index, id := range ids {
					if _, err := os.Stat(filepath.Join(f.backupTo, id+".tar")); err == nil {
						left = append(left, strconv.Itoa(index))
					}
				}
				return strings.Join(left, " ")
			}
			if c.revoked {
				checks := 0
				allowed := func() error {
					if checks++; checks > 1 {
						return apperr.New("FORBIDDEN", 403, "You can’t do that on this server")
					}
					return nil
				}
				err := f.jobs.Backups.MakeRoom(ctx, server, allowed, func(*sql.Tx) error { return allowed() })
				if !apperr.Is(err, "FORBIDDEN") || left() != c.want {
					t.Errorf("%v, left %q, want %q", err, left(), c.want)
				}
				return
			}
			var mu sync.Mutex
			var trace []string
			f.engine.OnStop = func(container *fakedocker.Container) {
				if container.ID != f.game {
					return
				}
				mu.Lock()
				trace = append(trace, left())
				mu.Unlock()
			}
			var input map[string]any
			if c.input != nil {
				input = c.input(ids)
			}
			job := f.run(t, c.kind, input)
			mu.Lock()
			defer mu.Unlock()
			if want := []string{c.want}; !slices.Equal(trace, want) {
				t.Errorf("left at each stop %q, want %q", trace, want)
			}
			if failed := job.Status == ops.Failed && strings.HasPrefix(job.Error, "The backup doesn’t fit under the total size limit."); failed != c.failed ||
				(!c.failed && job.Status != ops.Succeeded) {
				t.Errorf("%s %q", job.Status, job.Error)
			}
		})
	}
}
