package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
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

// Ludock restarts while a restart or stop is under way. The table gives the
// phase it was in, what happened to the server while Ludock was down, and
// what recovery then sent to Docker, in order. A restart whose stop Ludock
// sent is finished: its server is started once, unless something else
// started, replaced, or ran it since. A stop stays stopped, and a restart
// that never sent its stop is only marked interrupted.
func TestLifecycleRecoveryAfterItsStop(t *testing.T) {
	stopped := func(f *fixture, run string) {
		f.engine.Update(f.game, func(c *fakedocker.Container) {
			c.State.Status, c.State.Running, c.State.ExitCode, c.State.StartedAt = "exited", false, 0, run
			c.State.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		})
	}
	later := func() string { return time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano) }
	for _, c := range []struct {
		name, kind, phase string
		// settled: the stop's end was saved before Ludock restarted.
		settled bool
		// meanwhile is what happened while Ludock was down; run is the start
		// time of the run the work stopped.
		meanwhile func(f *fixture, run string)
		sent      []string
		running   bool
		// guidance is what Activity adds; with it, the server is as the work
		// left it, so the entry doesn't ask to check it.
		guidance string
		// err, when set, is the entry's error instead.
		err      string
		recovery bool
	}{
		{name: "restart, stop done unconfirmed", kind: "restart", phase: "stopping", meanwhile: stopped,
			sent: []string{"start mc"}, running: true, guidance: "Ludock started the server again."},
		{name: "restart, stop confirmed", kind: "restart", phase: "verifying_stop", settled: true, meanwhile: stopped,
			sent: []string{"start mc"}, running: true, guidance: "Ludock started the server again."},
		{name: "restart, start sent but not taken", kind: "restart", phase: "starting", settled: true, meanwhile: stopped,
			sent: []string{"start mc"}, running: true, guidance: "Ludock started the server again."},
		{name: "restart, started elsewhere", kind: "restart", phase: "stopping", meanwhile: func(f *fixture, _ string) {
			f.engine.Update(f.game, func(c *fakedocker.Container) { c.State.StartedAt = later() })
		}, running: true, guidance: "The server was running again, so Ludock left it as it was."},
		{name: "restart, run elsewhere and stopped", kind: "restart", phase: "verifying_stop", settled: true,
			meanwhile: func(f *fixture, _ string) { stopped(f, later()) }},
		{name: "restart, replaced", kind: "restart", phase: "stopping", meanwhile: func(f *fixture, run string) {
			stopped(f, run)
			f.game = f.engine.Recreate(f.game, nil)
			stopped(f, later())
		}, err: "Recovery requires administrator attention. The server has been left stopped where possible."},
		{name: "stop", kind: "stop", phase: "stopping", meanwhile: stopped},
		{name: "restart, stop not sent", kind: "restart", phase: "saving_world", running: true},
		{name: "recovery restart, stop done unconfirmed", kind: "restart", phase: "stopping", recovery: true, meanwhile: stopped,
			sent: []string{"start mc"}, running: true, guidance: "Ludock started the server again."},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			logical, _ := f.jobs.Identity.Get(ctx, f.serverID)
			info := f.engine.Container(f.game)
			run := info.State.StartedAt
			startedAt, _ := time.Parse(time.RFC3339Nano, run)
			started := startedAt.UnixMilli()
			actor, input := "admin", map[string]any{}
			if c.recovery {
				f.setRecovery(t, true)
				actor, input = ops.RecoveryActor, map[string]any{"run": started, "answered": 42}
			}
			recovery := ops.Recovery{ContainerID: f.game, LockKeys: []string{locks.ServerKey(f.serverID)}}
			if c.phase != "saving_world" {
				recovery.StopStarted, recovery.StopInitialStartedAt = true, &started
				recovery.StopRecoveryDeadlineAt = time.Now().UnixMilli()
				recovery.StopSettled = c.settled
			}
			if c.meanwhile != nil {
				c.meanwhile(f, run)
			}
			encoded, _ := json.Marshal(recovery)
			inputJSON, _ := json.Marshal(input)
			id := identity.NewUUID()
			if _, err := f.jobs.DB.Exec(`INSERT INTO operations (id,server_id,actor_id,kind,status,phase,input_json,recovery_json,binding_revision,created_at,updated_at)
				VALUES (?,?,?,?,'running',?,?,?,?,?,?)`, id, f.serverID, actor, c.kind, c.phase, string(inputJSON), string(encoded),
				logical.BindingRevision, db.Now(), db.Now()); err != nil {
				t.Fatal(err)
			}
			if c.recovery {
				state, _ := json.Marshal(monitor.Recovery{OperationID: id, Pending: true})
				if _, err := f.jobs.DB.Exec("UPDATE availability SET recovery_json=? WHERE server_id=?", string(state), f.serverID); err != nil {
					t.Fatal(err)
				}
			}
			before := len(f.engine.Requests)
			runner := &ops.Runner{DB: f.jobs.DB, Locks: f.jobs.Locks}
			f.jobs.Register(runner)
			if err := runner.Start(ctx, nil); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(runner.Stop)
			job, err := runner.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			var sent []string
			for _, request := range f.engine.Requests[before:] {
				method, path, _ := strings.Cut(request, " ")
				parts := strings.Split(path, "/")
				if method != "POST" || len(parts) < 5 || parts[2] != "containers" || !slices.Contains([]string{"start", "stop", "restart", "kill"}, parts[4]) {
					continue
				}
				name := "removed"
				if container := f.engine.Container(parts[3]); container != nil {
					name = strings.TrimPrefix(container.Name, "/")
				}
				sent = append(sent, parts[4]+" "+name)
			}
			want := "Ludock restarted while this ran. Check the server before trying again."
			if c.guidance != "" {
				want = "Ludock restarted while this ran."
			} else if c.err != "" {
				want = c.err
			}
			if job.Status != ops.Interrupted || job.Error != want || job.Result.Guidance != c.guidance || !slices.Equal(sent, c.sent) || f.running() != c.running {
				t.Fatalf("%s %q %q, sent %v, running %v", job.Status, job.Error, job.Result.Guidance, sent, f.running())
			}
			// Only a stop is one on purpose, and a start pauses outage tracking
			// past itself, so the time the server was down alerts no one.
			availability, err := f.jobs.Monitor.Get(ctx, f.serverID)
			if err != nil {
				t.Fatal(err)
			}
			if state := availability.State; state.IntentionallyStopped != (c.kind == "stop") || state.OutageStartedAt != 0 ||
				(len(c.sent) > 0 && state.SuppressedUntil <= time.Now().UnixMilli()) {
				t.Fatalf("monitoring %+v", state)
			}
		})
	}
}
