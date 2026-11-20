package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/monitor"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
)

// recoveryRestart runs a recovery restart whose game is hung while hung says
// so, with its pending record unless forgotten.
func (f *fixture) recoveryRestart(t *testing.T, hung func() bool, options ...bool) *ops.Job {
	forgotten, stale := len(options) > 0 && options[0], len(options) > 1 && options[1]
	t.Helper()
	f.jobs.Hung = func(string, servers.Container) (bool, int64) { return hung(), 42 }
	server, err := f.jobs.Servers.ResolveChecked(ctx, f.serverID, -1, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	// Queued as monitoring queues it, with its pending record.
	var job *ops.Job
	if err := f.jobs.DB.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		if job, err = f.runner.Enqueue(ctx, tx, ops.EnqueueOptions{ServerID: f.serverID, ActorID: ops.RecoveryActor, Kind: "restart",
			BindingRevision: server.Logical.BindingRevision, Input: map[string]any{"run": server.Container.StateSince - map[bool]int64{true: 1000}[stale], "answered": 42}}); err != nil {
			return err
		}
		state, _ := json.Marshal(monitor.Recovery{OperationID: job.ID, Pending: !forgotten})
		_, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value_json) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json",
			"monitor.recovery:"+f.serverID, string(state))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.runner.Wake()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if current, _ := f.runner.Get(ctx, job.ID); current.Status != ops.Queued && current.Status != ops.Running {
			return current
		}
	}
	t.Fatal("job did not finish")
	return nil
}

func (f *fixture) setRecovery(t *testing.T, on bool) {
	t.Helper()
	if err := f.jobs.Monitor.SetRecovery(ctx, f.admin.ID, f.adminSession, f.serverID, on); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) startedAt() string { return f.engine.Container(f.game).State.StartedAt }

// A recovery restart runs on the server's setting, not on the administrator
// who turned it on: one demoted since still has it restart the server.
func TestRecoveryRestartRunsOnTheServersSetting(t *testing.T) {
	f := setup(t)
	f.setRecovery(t, true)
	if err := f.jobs.DB.UpdateUserAccess(ctx, "admin", db.RoleMember, false); err != nil {
		t.Fatal(err)
	}
	before := f.startedAt()
	time.Sleep(2 * time.Millisecond)
	job := f.recoveryRestart(t, func() bool { return true })
	if job.Status != ops.Succeeded || f.startedAt() == before || !f.running() {
		t.Fatalf("%+v", job)
	}
}

// A recovery restart does nothing when the game answered again or the
// server was stopped since it was queued, or when recovery is no longer on.
func TestRecoveryRestartSkipsUnlessStillWanted(t *testing.T) {
	for name, scenario := range map[string]struct {
		hung   bool
		stop   bool
		policy func(*monitor.Policy)
		status string
	}{
		"answered again": {status: ops.Skipped},
		"stopped":        {hung: true, stop: true, status: ops.Skipped},
		"switched off":   {hung: true, policy: func(p *monitor.Policy) { p.Recover = false }, status: ops.Failed},
		"paused":         {hung: true, policy: func(p *monitor.Policy) { p.Maintenance = true }, status: ops.Failed},
		"not monitored":  {hung: true, policy: func(p *monitor.Policy) { p.Enabled = false }, status: ops.Failed},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			policy := monitor.DefaultPolicy()
			policy.Recover = true
			if scenario.policy != nil {
				scenario.policy(&policy)
			}
			if _, err := f.jobs.Monitor.Configure(ctx, f.admin.ID, f.adminSession, f.serverID, policy); err != nil {
				t.Fatal(err)
			}
			if scenario.stop {
				f.engine.Update(f.game, func(c *fakedocker.Container) { c.State.Status, c.State.Running = "exited", false })
			}
			before := f.startedAt()
			job := f.recoveryRestart(t, func() bool { return scenario.hung })
			if job.Status != scenario.status || f.startedAt() != before || f.running() == scenario.stop {
				t.Fatalf("%+v", job)
			}
		})
	}
}

// Recovery turned off while its restart runs stops it before it acts.
func TestRecoveryRestartStopsWhenTurnedOff(t *testing.T) {
	f := setup(t)
	f.setRecovery(t, true)
	before := f.startedAt()
	var turnedOff error
	job := f.recoveryRestart(t, func() bool {
		turnedOff = f.jobs.Monitor.SetRecovery(ctx, f.admin.ID, f.adminSession, f.serverID, false)
		return true
	})
	if turnedOff != nil || job.Status != ops.Failed || f.startedAt() != before || !f.running() {
		t.Fatalf("%v %+v", turnedOff, job)
	}
}

// A restart queued before recovery was turned off and on again doesn't run.
func TestRecoveryRestartNeedsItsPendingRecord(t *testing.T) {
	f := setup(t)
	f.setRecovery(t, true)
	before := f.startedAt()
	job := f.recoveryRestart(t, func() bool { return true }, true)
	if job.Status != ops.Failed || f.startedAt() != before {
		t.Fatalf("%+v", job)
	}
}

// A restart queued for an earlier run of the container doesn't act on a
// newer one.
func TestRecoveryRestartActsOnlyOnItsRun(t *testing.T) {
	f := setup(t)
	f.setRecovery(t, true)
	f.engine.Update(f.game, func(c *fakedocker.Container) {
		c.State.StartedAt = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
	})
	before := f.startedAt()
	f.jobs.Servers.RefreshNow(ctx)
	job := f.recoveryRestart(t, func() bool { return true }, false, true)
	if job.Status != ops.Skipped || f.startedAt() != before || !f.running() {
		t.Fatalf("%+v", job)
	}
}

// A game that answered after its restart was queued has a new silence, with
// its own wait.
func TestRecoveryRestartNeedsTheSilenceItWasQueuedFor(t *testing.T) {
	f := setup(t)
	f.setRecovery(t, true)
	before := f.startedAt()
	job := f.recoveryRestart(t, func() bool {
		f.jobs.Hung = func(string, servers.Container) (bool, int64) { return true, 43 }
		return true
	})
	if job.Status != ops.Skipped || f.startedAt() != before {
		t.Fatalf("%+v", job)
	}
}
