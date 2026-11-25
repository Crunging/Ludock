package jobs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/ops"
)

// Ludock is killed while a backup, or a restart that backs up first, is
// copying the stopped server, or once its backup was recorded. Recovery
// starts the server, which was running, without taking the backup again,
// and logs what it did without the server's paths. Unfinished, the work
// says the backup didn't finish and that the server was started again,
// without asking to check it, and alerts once, as a failed backup would.
// Finished, it ends as succeeded and alerts no one.
func TestBackupRestartInterruptedDuringBackup(t *testing.T) {
	for _, c := range []struct {
		kind     string
		input    map[string]any
		complete bool
	}{{"restart", map[string]any{"createBackup": true}, false}, {"backup", nil, false},
		{"restart", map[string]any{"createBackup": true}, true}, {"backup", nil, true}} {
		kind, input := c.kind, c.input
		t.Run(fmt.Sprint(kind, " complete ", c.complete), func(t *testing.T) {
			f := setup(t)
			logical, _ := f.jobs.Identity.Get(ctx, f.serverID)
			f.runner.Stop()
			job, err := f.runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: "admin", Kind: kind,
				BindingRevision: logical.BindingRevision, Input: input})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.jobs.Docker.Stop(ctx, f.game, nil); err != nil {
				t.Fatal(err)
			}
			stopped, _ := f.jobs.Docker.Inspect(ctx, f.game)
			running := true
			recovery, _ := json.Marshal(ops.Recovery{ContainerID: logical.ContainerID, Fingerprint: logical.BindingFingerprint,
				LockKeys: []string{locks.ServerKey(f.serverID)}, InitialRunning: &running, StopStarted: true, StopSettled: true,
				StoppedStartedAt: stopped.State.StartedAt, BackupID: "11111111-1111-4111-8111-111111111111", BackupDestination: f.backupTo,
				BackupComplete: c.complete})
			if _, err := f.jobs.DB.Exec("UPDATE operations SET status='running',phase='backing_up',recovery_json=? WHERE id=?", string(recovery), job.ID); err != nil {
				t.Fatal(err)
			}
			var logged bytes.Buffer
			logging.SetOutput(&logged, &logged)
			defer logging.SetOutput(os.Stdout, os.Stderr)
			if err := f.runner.Start(ctx, nil); err != nil {
				t.Fatal(err)
			}
			recovered, _ := f.runner.Get(ctx, job.ID)
			guidance := recovered.Result.Guidance
			status, message, alerts, outcome := ops.Interrupted, "Ludock restarted while this ran.", []string{"mc: " + backupInterrupted}, "ended as interrupted"
			ok := strings.HasSuffix(guidance, map[string]string{"restart": "The server was started again.", "backup": "Ludock started the server again."}[kind]) &&
				strings.Contains(guidance, "no new backup")
			if c.complete {
				status, message, alerts, outcome = ops.Succeeded, "", nil, "finished"
				ok = guidance == "Ludock restarted while this ran, after the backup had finished. Ludock started the server again."
			}
			if recovered.Status != status || recovered.Error != message || !ok || !f.running() {
				t.Fatalf("%+v", recovered)
			}
			if line := logged.String(); !strings.Contains(line, job.ID) || !strings.Contains(line, `"kind":"`+kind+`"`) ||
				!strings.Contains(line, outcome+", server started again") || strings.Contains(line, f.backupTo) || strings.Contains(line, f.data) {
				t.Fatal(line)
			}
			if backups, _ := f.jobs.Backups.List(ctx, f.serverID); len(backups) != 0 {
				t.Fatal("backup taken again:", backups)
			}
			if !slices.Equal(f.alerts, alerts) {
				t.Fatal("alerts:", f.alerts)
			}
		})
	}
}

// Ludock is killed while a restart that backs up first waits for the world
// to save, before anything stopped, after its check of the data left a
// helper. Recovery leaves the server running, removes the helper, says
// nothing changed without asking to check the server, and alerts once that
// there's no new backup.
func TestBackupRestartInterruptedBeforeStop(t *testing.T) {
	f := setup(t)
	logical, _ := f.jobs.Identity.Get(ctx, f.serverID)
	f.runner.Stop()
	job, err := f.runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: "admin", Kind: "restart",
		BindingRevision: logical.BindingRevision, Input: map[string]any{"createBackup": true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.jobs.DB.Exec("UPDATE operations SET status='running',phase='waiting_for_save' WHERE id=?", job.ID); err != nil {
		t.Fatal(err)
	}
	left := f.engine.AddContainer(fakedocker.GameOptions{Name: "left", Image: image, Running: true,
		Labels: map[string]string{"ludock.internal": "backup-helper", "ludock.operation": job.ID}})
	if err := f.runner.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	recovered, _ := f.runner.Get(ctx, job.ID)
	if want := []string{"mc: " + backupInterrupted}; recovered.Status != ops.Interrupted || recovered.Error != "Ludock restarted while this ran." ||
		recovered.Result.Guidance != "Ludock restarted before this stopped the server, so nothing changed and there’s no new backup." ||
		!f.running() || !slices.Equal(f.alerts, want) {
		t.Fatalf("%+v, alerts %v", recovered, f.alerts)
	}
	if f.engine.Container(left) != nil {
		t.Fatal("the check's helper was left")
	}
}

// Something outside Ludock stops the server after a restart that backs up
// first found it running, while it checks the data before the stop. The
// backup is still taken, and the server stays stopped, as its owner left it
// and as recovery would leave it.
func TestBackupRestartKeepsServerStoppedElsewhere(t *testing.T) {
	f := setup(t)
	run := f.engine.Exec
	var stopped sync.Once
	f.engine.Exec = func(c *fakedocker.Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int {
		if c.ID != f.game {
			stopped.Do(func() {
				f.engine.Update(f.game, func(c *fakedocker.Container) { c.State.Status, c.State.Running = "exited", false })
			})
		}
		return run(c, cmd, stdin, stdout, stderr)
	}
	job := f.run(t, "restart", map[string]any{"createBackup": true})
	if backups, _ := f.jobs.Backups.List(ctx, f.serverID); job.Status != ops.Succeeded || f.running() || len(backups) != 1 ||
		job.Result.BackupID != backups[0].ID || job.Result.Guidance != stoppedElsewhere {
		t.Fatalf("%+v, running %v, backups %v", job, f.running(), backups)
	}
}
