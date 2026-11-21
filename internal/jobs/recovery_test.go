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
		_, err = tx.ExecContext(ctx, "UPDATE availability SET recovery_json=? WHERE server_id=?", string(state), f.serverID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.runner.Wake()
	return finished(t, f.runner, job.ID)
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

// A recovery restart does nothing when the game answered again, was stopped,
// or answered and went silent again since it was queued, when recovery is no
// longer on or was turned off while the restart ran, when it was queued for
// an earlier run of the container, or when recovery was turned off and on
// again, which drops its pending record.
func TestRecoveryRestartSkipsUnlessStillWanted(t *testing.T) {
	for name, scenario := range map[string]struct {
		hung      func(*fixture) bool
		stop      bool
		newerRun  bool
		forgotten bool
		policy    func(*monitor.Policy)
		status    string
	}{
		"answered again": {hung: func(*fixture) bool { return false }, status: ops.Skipped},
		"new silence": {status: ops.Skipped, hung: func(f *fixture) bool {
			f.jobs.Hung = func(string, servers.Container) (bool, int64) { return true, 43 }
			return true
		}},
		"stopped":       {stop: true, status: ops.Skipped},
		"newer run":     {newerRun: true, status: ops.Skipped},
		"switched off":  {policy: func(p *monitor.Policy) { p.Recover = false }, status: ops.Failed},
		"paused":        {policy: func(p *monitor.Policy) { p.Maintenance = true }, status: ops.Failed},
		"not monitored": {policy: func(p *monitor.Policy) { p.Enabled = false }, status: ops.Failed},
		"turned off while running": {status: ops.Failed, hung: func(f *fixture) bool {
			if err := f.jobs.Monitor.SetRecovery(ctx, f.admin.ID, f.adminSession, f.serverID, false); err != nil {
				panic(err)
			}
			return true
		}},
		"pending record dropped": {forgotten: true, status: ops.Failed},
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
			if scenario.newerRun {
				f.engine.Update(f.game, func(c *fakedocker.Container) {
					c.State.StartedAt = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
				})
				f.jobs.Servers.RefreshNow(ctx)
			}
			hung := func() bool { return true }
			if scenario.hung != nil {
				hung = func() bool { return scenario.hung(f) }
			}
			before := f.startedAt()
			job := f.recoveryRestart(t, hung, scenario.forgotten, scenario.newerRun)
			if job.Status != scenario.status || f.startedAt() != before || f.running() == scenario.stop {
				t.Fatalf("%+v", job)
			}
		})
	}
}
