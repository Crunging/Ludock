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
	"github.com/Crunging/Ludock/internal/docker"
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
// started, replaced, or ran it since. Work recovery finished ends as
// succeeded; the rest stays interrupted, and says what the server was left
// as wherever Ludock knows.
func TestLifecycleRecoveryAfterItsStop(t *testing.T) {
	stopped := func(f *fixture, run string) {
		f.engine.Update(f.game, func(c *fakedocker.Container) {
			c.State.Status, c.State.Running, c.State.ExitCode, c.State.StartedAt = "exited", false, 0, run
			c.State.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		})
	}
	later := func() string { return time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano) }
	startedElsewhere := func(f *fixture, _ string) {
		f.engine.Update(f.game, func(c *fakedocker.Container) { c.State.StartedAt = later() })
	}
	const started, runningAgain = "Ludock restarted while this ran. " + serverStarted, "Ludock restarted while this ran. " + serverRunningAgain
	for _, c := range []struct {
		name, kind, phase string
		// settled: the stop's end was saved before Ludock restarted;
		// requested: so was an earlier recovery's start, unconfirmed;
		// wasStopped: the server was stopped when the stop was sent;
		// loopRun: Docker started a newer run just before the stop, which
		// the confirmed stop saved.
		settled, requested, wasStopped, loopRun bool
		// meanwhile is what happened while Ludock was down; run is the start
		// time of the run the work stopped.
		meanwhile func(f *fixture, run string)
		sent      []string
		running   bool
		// done: the work ends as succeeded. Otherwise, with guidance the
		// entry doesn't ask to check the server.
		done     bool
		guidance string
		// err, when set, is the entry's error instead.
		err      string
		recovery bool
	}{
		{name: "restart, stop done unconfirmed", kind: "restart", phase: "stopping", meanwhile: stopped,
			sent: []string{"start mc"}, running: true, done: true, guidance: started},
		{name: "restart, stop confirmed", kind: "restart", phase: "verifying_stop", settled: true, meanwhile: stopped,
			sent: []string{"start mc"}, running: true, done: true, guidance: started},
		{name: "restart, start sent but not taken", kind: "restart", phase: "starting", settled: true, meanwhile: stopped,
			sent: []string{"start mc"}, running: true, done: true, guidance: started},
		{name: "restart, started elsewhere", kind: "restart", phase: "stopping", meanwhile: startedElsewhere,
			running: true, done: true, guidance: runningAgain},
		{name: "restart, recovery's start unconfirmed, then started elsewhere", kind: "restart", phase: "starting", settled: true, requested: true,
			meanwhile: startedElsewhere, running: true, done: true, guidance: runningAgain},
		{name: "restart, the stop ended a run Docker began just before it", kind: "restart", phase: "starting", settled: true, loopRun: true,
			meanwhile: func(f *fixture, _ string) {}, sent: []string{"start mc"}, running: true, done: true, guidance: started},
		{name: "restart, its start taken and the game exiting again", kind: "restart", phase: "starting", settled: true, requested: true,
			meanwhile: func(f *fixture, _ string) {
				f.engine.Update(f.game, func(c *fakedocker.Container) {
					c.State.Status, c.State.Running, c.State.Restarting, c.State.StartedAt = "restarting", true, true, later()
				})
			}, running: true, done: true,
			guidance: "Ludock restarted while this ran. " + serverRestarting},
		{name: "restart, stop unconfirmed, run elsewhere and stopped", kind: "restart", phase: "stopping",
			meanwhile: func(f *fixture, _ string) { stopped(f, later()) },
			guidance:  "The server ran again and had stopped when Ludock checked it, so Ludock left it stopped."},
		{name: "restart, run elsewhere and stopped", kind: "restart", phase: "verifying_stop", settled: true,
			meanwhile: func(f *fixture, _ string) { stopped(f, later()) },
			guidance:  "The server ran again and had stopped when Ludock checked it, so Ludock left it stopped."},
		{name: "restart, replaced", kind: "restart", phase: "stopping", meanwhile: func(f *fixture, run string) {
			stopped(f, run)
			f.game = f.engine.Recreate(f.game, nil)
			stopped(f, later())
		}, err: "Recovery requires administrator attention. The server has been left stopped where possible."},
		{name: "stop", kind: "stop", phase: "stopping", meanwhile: stopped, done: true,
			guidance: "Ludock restarted while this ran, and confirmed the server stopped."},
		{name: "stop, server already stopped", kind: "stop", phase: "verifying_stop", settled: true, wasStopped: true,
			meanwhile: func(f *fixture, run string) {
				stopped(f, run)
				f.jobs.Monitor.SetIntentionalStop(ctx, f.serverID, true)
			}, done: true, guidance: "Ludock restarted while this ran. The server was already stopped."},
		{name: "stop, started again elsewhere", kind: "stop", phase: "stopping", meanwhile: startedElsewhere, running: true,
			guidance: "Something outside Ludock started the server again before it stopped, so it is running."},
		{name: "restart, stop not sent", kind: "restart", phase: "saving_world", running: true,
			guidance: "Ludock restarted before it stopped the server, so the server is as it was."},
		{name: "recovery restart, stop done unconfirmed", kind: "restart", phase: "stopping", recovery: true, meanwhile: stopped,
			sent: []string{"start mc"}, running: true, done: true, guidance: started},
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
			if c.phase != "saving_world" && !c.wasStopped {
				recovery.StopStarted, recovery.StopInitialStartedAt = true, &started
				recovery.StopRecoveryDeadlineAt = time.Now().UnixMilli()
			}
			recovery.StopSettled, recovery.StartRequested = c.settled, c.requested
			if c.loopRun {
				newer := later()
				stopped(f, newer)
				run, _ := time.Parse(time.RFC3339Nano, newer)
				recovery.StoppedRun = run.UnixMilli()
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
			status, want := ops.Interrupted, "Ludock restarted while this ran. Check the server before trying again."
			switch {
			case c.done:
				status, want = ops.Succeeded, ""
			case c.err != "":
				want = c.err
			case c.guidance != "":
				want = "Ludock restarted while this ran."
			}
			if job.Status != status || job.Error != want || job.Result.Guidance != c.guidance || !slices.Equal(sent, c.sent) || f.running() != c.running {
				t.Fatalf("%s %q %q, sent %v, running %v", job.Status, job.Error, job.Result.Guidance, sent, f.running())
			}
			// Only a stop is one on purpose, and a start pauses outage tracking
			// past itself, so the time the server was down alerts no one.
			availability, err := f.jobs.Monitor.Get(ctx, f.serverID)
			if err != nil {
				t.Fatal(err)
			}
			if state := availability.State; state.IntentionallyStopped != (c.kind == "stop" && !c.running) || state.OutageStartedAt != 0 ||
				(len(c.sent) > 0 && state.SuppressedUntil <= time.Now().UnixMilli()) {
				t.Fatalf("monitoring %+v", state)
			}
		})
	}
}

// A restart of a crash-looping server, and Ludock restarts just as it
// sends the start. Either Docker started the loop's next run between
// Ludock's last look and its stop, which ended that newer run, and recovery
// starts the server once, as the restart would; or another manager ran and
// stopped a newer run first, so Docker found it stopped, and recovery
// leaves that run as it is.
func TestRestartRecoveryAfterItStoppedANewerRun(t *testing.T) {
	for _, c := range []struct {
		name string
		// elsewhere: the newer run was stopped before Ludock's stop arrived.
		elsewhere bool
		status    string
		starts    int
	}{
		{name: "the stop ended it", status: ops.Succeeded, starts: 1},
		{name: "stopped elsewhere first", elsewhere: true, status: ops.Interrupted},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			f.engine.CrashLoop(f.game, false)
			var stopped docker.ContainerState
			newerRun := func() {
				f.engine.Update(f.game, func(c *fakedocker.Container) {
					c.State.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
				})
			}
			if c.elsewhere {
				f.engine.Hang = func(request string) bool {
					if strings.HasPrefix(request, "POST ") && strings.HasSuffix(request, "/containers/"+f.game+"/stop") {
						f.engine.Update(f.game, func(c *fakedocker.Container) {
							c.State.Status, c.State.Running, c.State.Restarting = "exited", false, false
							c.State.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
							stopped = c.State
						})
					}
					return false
				}
			} else {
				onStop := f.engine.OnStop
				f.engine.OnStop = func(c *fakedocker.Container) {
					if onStop != nil {
						onStop(c)
					}
					if c.ID == f.game {
						newerRun()
						stopped = f.engine.Container(f.game).State
					}
				}
			}
			// What was saved as the start was sent.
			var saved, phase string
			onStart := f.engine.OnStart
			f.engine.OnStart = func(c *fakedocker.Container) {
				if onStart != nil {
					onStart(c)
				}
				if c.ID == f.game && saved == "" {
					if err := f.jobs.DB.QueryRow("SELECT recovery_json,phase FROM operations WHERE kind='restart' AND status='running'").Scan(&saved, &phase); err != nil {
						t.Error(err)
					}
				}
			}
			job := f.run(t, ops.KindRestart, nil)
			if job.Status != ops.Succeeded || saved == "" {
				t.Fatalf("%+v", job)
			}
			f.runner.Stop()
			f.engine.OnStart, f.engine.Hang = onStart, nil
			f.engine.Update(f.game, func(c *fakedocker.Container) { c.State = stopped })
			if _, err := f.jobs.DB.Exec("UPDATE operations SET status='running',phase=?,recovery_json=?,result_json=NULL WHERE id=?", phase, saved, job.ID); err != nil {
				t.Fatal(err)
			}
			before := len(f.engine.Requests)
			runner := &ops.Runner{DB: f.jobs.DB, Locks: f.jobs.Locks}
			f.jobs.Register(runner)
			if err := runner.Start(ctx, nil); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(runner.Stop)
			recovered, _ := runner.Get(ctx, job.ID)
			starts := 0
			for _, request := range f.engine.Requests[before:] {
				if strings.HasSuffix(request, "/containers/"+f.game+"/start") {
					starts++
				}
			}
			if recovered.Status != c.status || f.running() != (c.starts == 1) || starts != c.starts {
				t.Fatalf("%s %q, running %v, %d starts", recovered.Status, recovered.Result.Guidance, f.running(), starts)
			}
		})
	}
}

// Ludock restarts while work that had stopped a running server is past its
// stop, and while it was down the server stayed stopped, was started
// elsewhere, or ran again after an earlier recovery's start and stopped.
// Each kind of work says which, in the same words, and starts the server
// only in the first case.
func TestRecoverySaysWhatBecameOfTheServer(t *testing.T) {
	kinds := []struct {
		name, kind string
		input      map[string]any
		saved      func(r *ops.Recovery)
		// lead begins the guidance; done says the work ends as succeeded,
		// and with the server back in the first two cases only.
		lead       string
		done, back bool
	}{
		{"backup", "backup", nil, func(r *ops.Recovery) { r.BackupID, r.BackupComplete = "11111111-1111-4111-8111-111111111111", true },
			"Ludock restarted while this ran, after the backup had finished.", true, false},
		{"restart with a backup", "restart", map[string]any{"createBackup": true}, func(r *ops.Recovery) {
			r.BackupID, r.BackupComplete = "11111111-1111-4111-8111-111111111111", true
		}, "Ludock restarted while this ran, after the backup had finished.", true, true},
		{"restart with no backup made", "restart", map[string]any{"createBackup": true}, func(r *ops.Recovery) { r.BackupID = "11111111-1111-4111-8111-111111111111" },
			"Ludock restarted before the backup finished, so there’s no new backup.", false, false},
		{"restore", "restore", nil, func(r *ops.Recovery) { r.Restore = json.RawMessage(`{"status":"cleaned"}`) },
			"Ludock restarted while this ran, after the restore had finished: what it restored is in place.", true, false},
		{"undo", "undo_restore", nil, func(r *ops.Recovery) { r.Undone = "Ludock finished undoing a restore that was stopped partway." },
			"Ludock restarted while this ran, and finished it.", true, false},
		{"update", "update", skipBackup, func(*ops.Recovery) {}, "Ludock restarted during the update, so the update didn’t happen.", false, false},
	}
	states := []struct {
		name      string
		requested bool
		meanwhile func(f *fixture, run string)
		said      string
		running   bool
		starts    int
	}{
		{name: "stayed stopped", meanwhile: func(f *fixture, run string) {
			f.engine.Update(f.game, func(c *fakedocker.Container) {
				c.State.Status, c.State.Running, c.State.StartedAt = "exited", false, run
			})
		}, said: serverStarted, running: true, starts: 1},
		{name: "started elsewhere", meanwhile: func(f *fixture, _ string) {
			f.engine.Update(f.game, func(c *fakedocker.Container) {
				c.State.StartedAt = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
			})
		}, said: serverRunningAgain, running: true},
		// A start saved before Ludock exited may never have been sent.
		{name: "start saved, started elsewhere", requested: true, meanwhile: func(f *fixture, _ string) {
			f.engine.Update(f.game, func(c *fakedocker.Container) {
				c.State.StartedAt = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
			})
		}, said: serverRunningAgain, running: true},
		{name: "start saved, game exiting again", requested: true, meanwhile: func(f *fixture, _ string) {
			f.engine.Update(f.game, func(c *fakedocker.Container) {
				c.State.Status, c.State.Running, c.State.Restarting = "restarting", true, true
				c.State.StartedAt = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
			})
		}, said: serverRestarting, running: true},
		// Another manager starts it as Ludock's start is on its way.
		{name: "started elsewhere as Ludock started it", meanwhile: func(f *fixture, run string) {
			f.engine.Update(f.game, func(c *fakedocker.Container) {
				c.State.Status, c.State.Running, c.State.StartedAt = "exited", false, run
			})
			f.engine.Hang = func(request string) bool {
				if strings.HasPrefix(request, "POST ") && strings.HasSuffix(request, "/containers/"+f.game+"/start") {
					f.engine.Update(f.game, func(c *fakedocker.Container) {
						c.State.Status, c.State.Running = "running", true
						c.State.StartedAt = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
					})
				}
				return false
			}
		}, said: serverRunningAgain, running: true, starts: 1},
		{name: "ran again and stopped", requested: true, meanwhile: func(f *fixture, _ string) {
			f.engine.Update(f.game, func(c *fakedocker.Container) {
				c.State.Status, c.State.Running = "exited", false
				c.State.StartedAt = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
			})
		}, said: serverStoppedAgain},
	}
	for _, k := range kinds {
		for _, s := range states {
			t.Run(k.name+", "+s.name, func(t *testing.T) {
				f := setup(t)
				f.runner.Stop()
				logical, _ := f.jobs.Identity.Get(ctx, f.serverID)
				run := f.engine.Container(f.game).State.StartedAt
				running := true
				recovery := ops.Recovery{ContainerID: f.game, Fingerprint: logical.BindingFingerprint, LockKeys: []string{locks.ServerKey(f.serverID)},
					InitialRunning: &running, StopStarted: true, StopSettled: true, StoppedStartedAt: run, StartRequested: s.requested}
				k.saved(&recovery)
				s.meanwhile(f, run)
				encoded, _ := json.Marshal(recovery)
				input, _ := json.Marshal(k.input)
				if k.input == nil {
					input = []byte("{}")
				}
				id := identity.NewUUID()
				if _, err := f.jobs.DB.Exec(`INSERT INTO operations (id,server_id,actor_id,kind,status,phase,input_json,recovery_json,binding_revision,created_at,updated_at)
					VALUES (?,?,'admin',?,'running','stopped',?,?,?,1,1)`, id, f.serverID, k.kind, string(input), string(encoded), logical.BindingRevision); err != nil {
					t.Fatal(err)
				}
				before := len(f.engine.Requests)
				runner := &ops.Runner{DB: f.jobs.DB, Locks: f.jobs.Locks}
				f.jobs.Register(runner)
				if err := runner.Start(ctx, nil); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(runner.Stop)
				job, _ := runner.Get(ctx, id)
				starts := 0
				for _, request := range f.engine.Requests[before:] {
					if strings.HasSuffix(request, "/containers/"+f.game+"/start") {
						starts++
					}
				}
				status := ops.Interrupted
				if k.done && (!k.back || s.said == serverStarted || s.said == serverRunningAgain) {
					status = ops.Succeeded
				}
				if job.Status != status || job.Result.Guidance != k.lead+" "+s.said || f.running() != s.running || starts != s.starts {
					t.Fatalf("%s %q %q, running %v, %d starts", job.Status, job.Error, job.Result.Guidance, f.running(), starts)
				}
			})
		}
	}
}
