package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/ops"
)

// interruptedWarning saves a restart that warned players and was waiting
// when Ludock stopped, with recovery as it was saved then, and starts a new
// runner as Ludock's next start would.
func (f *fixture) interruptedWarning(t *testing.T, phase string, recovery ops.Recovery) (*ops.Runner, string) {
	t.Helper()
	f.runner.Stop()
	server, _ := f.jobs.Identity.Get(ctx, f.serverID)
	job, err := f.runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: "admin", Kind: "restart",
		BindingRevision: server.BindingRevision, Input: map[string]any{WarnInput: true}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(recovery)
	if _, err := f.jobs.DB.Exec("UPDATE operations SET status='running', phase=?, recovery_json=? WHERE id=?", phase, string(encoded), job.ID); err != nil {
		t.Fatal(err)
	}
	next := &ops.Runner{DB: f.jobs.DB, Locks: f.jobs.Locks}
	f.jobs.Register(next)
	if err := next.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(next.Stop)
	return next, job.ID
}

func finished(t *testing.T, runner *ops.Runner, id string) *ops.Job {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if current, _ := runner.Get(ctx, id); current.Status != ops.Queued && current.Status != ops.Running {
			return current
		}
	}
	t.Fatal("job did not finish")
	return nil
}

// A restart that warned players and was waiting when Ludock stopped resumes
// its wait at the next start and restarts once at the time players were
// given, without warning them again.
func TestWarnedRestartResumesAfterLudockRestarts(t *testing.T) {
	f := setup(t)
	before := f.startedAt()
	until := time.Now().Add(300 * time.Millisecond).UnixMilli()
	runner, id := f.interruptedWarning(t, "warning", ops.Recovery{WarnUntil: until, WarnSent: true})
	job := finished(t, runner, id)
	if job.Status != ops.Succeeded || f.startedAt() == before || !f.running() {
		t.Fatalf("%+v", job)
	}
	if job.Recovery.WarnUntil != until || job.UpdatedAt < until {
		t.Fatal("warned again, or restarted before the time players were given", job.Recovery.WarnUntil, job.UpdatedAt-until)
	}
}

// Ludock stopping while a warned restart waits out its minute ends the wait
// at once and leaves the restart running with the time players were given,
// which the next start resumes.
func TestStoppingDuringAWarningLeavesItToResume(t *testing.T) {
	f := setup(t)
	f.jobs.WarningLead = time.Minute
	f.runner.Stop()
	running, cancel := context.WithCancel(ctx)
	runner := &ops.Runner{DB: f.jobs.DB, Locks: f.jobs.Locks}
	f.jobs.Register(runner)
	if err := runner.Start(running, nil); err != nil {
		t.Fatal(err)
	}
	before := f.startedAt()
	server, _ := f.jobs.Identity.Get(ctx, f.serverID)
	job, err := runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: "admin", Kind: "restart",
		BindingRevision: server.BindingRevision, Input: map[string]any{WarnInput: true}})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if current, _ := runner.Get(ctx, job.ID); current.Recovery.WarnUntil != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the warning never started")
		}
	}
	cancel()
	stopping := time.Now()
	runner.Stop()
	if waited := time.Since(stopping); waited > 2*time.Second {
		t.Fatal("stopping waited", waited)
	}
	current, _ := runner.Get(ctx, job.ID)
	if current.Status != ops.Running || current.Phase != "warning" || current.Recovery.WarnUntil == 0 || f.startedAt() != before {
		t.Fatalf("%+v", current)
	}
}

// A warned restart found long after its time, as after Ludock was down for
// a while, doesn't restart late and says players were told it would; one
// that already began stopping is reconciled, never resumed.
func TestWarnedRestartTooLateOrStartedIsNotResumed(t *testing.T) {
	f := setup(t)
	before := f.startedAt()
	runner, id := f.interruptedWarning(t, "warning", ops.Recovery{WarnUntil: time.Now().Add(-10 * time.Minute).UnixMilli(), WarnSent: true})
	job := finished(t, runner, id)
	if job.Status != ops.Failed || job.Error != "Ludock wasn’t running when the server was due to restart, so it didn’t. Players had been told it would." ||
		f.startedAt() != before {
		t.Fatalf("%+v", job)
	}

	f = setup(t)
	before = f.startedAt()
	runner, id = f.interruptedWarning(t, "verifying_stop", ops.Recovery{WarnUntil: time.Now().UnixMilli(), WarnSent: true, StopStarted: true, StopSettled: true})
	if job := finished(t, runner, id); job.Status != ops.Interrupted || f.startedAt() != before {
		t.Fatalf("%+v", job)
	}
}

// A fresh warned restart saves when players were told before it waits, and
// restarts once that time has come.
func TestWarnedRestartWaitsForTheTimeGiven(t *testing.T) {
	f := setup(t)
	f.jobs.WarningLead = 200 * time.Millisecond
	started := time.Now()
	job := f.run(t, "restart", map[string]any{WarnInput: true})
	if job.Status != ops.Succeeded || job.Recovery.WarnUntil == 0 || time.Since(started) < 200*time.Millisecond {
		t.Fatalf("%+v", job)
	}
}

// A failure after players were warned says so, except one the runner acts
// on, such as resources being busy, which keeps its chain so the job is
// queued again instead of failing.
func TestCalledOffKeepsRunnerErrors(t *testing.T) {
	job := &ops.Context{Job: &ops.Job{Kind: "restart", Recovery: ops.Recovery{WarnSent: true}}}
	internal := errors.New("resources busy")
	if err := calledOff(job, internal); err != internal {
		t.Fatal(err)
	}
	err := calledOff(job, errRevoked)
	if err.Error() != "The person who asked for this can no longer use the server. Players were told the server would restart, but it didn’t." {
		t.Fatal(err)
	}
}
