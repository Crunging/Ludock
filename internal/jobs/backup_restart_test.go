package jobs

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/ops"
)

// Ludock is killed while a backup, or a restart that backs up first, is
// copying the stopped server. Recovery starts the server, which was running,
// without taking the backup again, says the backup didn't finish, and alerts
// once, as a failed backup would.
func TestBackupRestartInterruptedDuringBackup(t *testing.T) {
	for kind, input := range map[string]map[string]any{"restart": {"createBackup": true}, "backup": nil} {
		t.Run(kind, func(t *testing.T) {
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
				StoppedStartedAt: stopped.State.StartedAt, BackupID: "11111111-1111-4111-8111-111111111111", BackupDestination: f.backupTo})
			if _, err := f.jobs.DB.Exec("UPDATE operations SET status='running',phase='backing_up',recovery_json=? WHERE id=?", string(recovery), job.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.runner.Start(ctx, nil); err != nil {
				t.Fatal(err)
			}
			recovered, _ := f.runner.Get(ctx, job.ID)
			guidance, _ := recovered.Result["guidance"].(string)
			if recovered.Status != ops.Interrupted || (kind == "restart" && !strings.Contains(guidance, "no new backup")) || !f.running() {
				t.Fatalf("%+v", recovered)
			}
			if backups, _ := f.jobs.Backups.List(ctx, f.serverID); len(backups) != 0 {
				t.Fatal("backup taken again:", backups)
			}
			if want := []string{"mc: " + backupInterrupted}; !slices.Equal(f.alerts, want) {
				t.Fatal("alerts:", f.alerts)
			}
		})
	}
}

// Ludock is killed while a restart that backs up first waits for the world
// to save, before anything stopped, after its check of the data left a
// helper. Recovery leaves the server running, removes the helper, and alerts
// once that there's no new backup.
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
	if want := []string{"mc: " + backupInterrupted}; recovered.Status != ops.Interrupted || !f.running() || !slices.Equal(f.alerts, want) {
		t.Fatalf("%+v, alerts %v", recovered, f.alerts)
	}
	if f.engine.Container(left) != nil {
		t.Fatal("the check's helper was left")
	}
}
