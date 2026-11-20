package jobs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/ops"
)

// Ludock is killed while a restart that backs up first is copying the
// stopped server. Recovery starts the
// server, which was running, without taking the backup again, and says the
// backup didn't finish.
func TestBackupRestartInterruptedDuringBackup(t *testing.T) {
	f := setup(t)
	logical, _ := f.jobs.Identity.Get(ctx, f.serverID)
	f.runner.Stop()
	job, err := f.runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: "admin", Kind: "restart",
		BindingRevision: logical.BindingRevision, Input: map[string]any{"createBackup": true}})
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
	if recovered.Status != ops.Interrupted || !strings.Contains(guidance, "no new backup") || !f.running() {
		t.Fatalf("%+v", recovered)
	}
	if backups, _ := f.jobs.Backups.List(ctx, f.serverID); len(backups) != 0 {
		t.Fatal("backup taken again:", backups)
	}
}
