package jobs

import (
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/ops"
)

func waitFor(t *testing.T, runner *ops.Runner, id string, ready func(*ops.Job) bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if current, _ := runner.Get(ctx, id); ready(current) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the job never got there")
		}
	}
}

// Cancelling a warned restart around the end of its wait has exactly one
// outcome: cancelled with the server untouched, or refused because the
// restart began, which then finishes.
func TestCancelRacesTheEndOfAWarning(t *testing.T) {
	f := setup(t)
	f.jobs.WarningLead = 100 * time.Millisecond
	f.jobs.Poll = 5 * time.Millisecond
	token := f.adminSession
	outcomes := map[bool]int{}
	for offset := -40 * time.Millisecond; offset <= 40*time.Millisecond; offset += 5 * time.Millisecond {
		before := f.startedAt()
		server, _ := f.jobs.Identity.Get(ctx, f.serverID)
		job, err := f.runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: "admin", Kind: "restart",
			BindingRevision: server.BindingRevision, Input: map[string]any{WarnInput: true}})
		if err != nil {
			t.Fatal(err)
		}
		var until int64
		waitFor(t, f.runner, job.ID, func(current *ops.Job) bool {
			until = current.Recovery.WarnUntil
			return until != 0
		})
		time.Sleep(time.Until(time.UnixMilli(until).Add(offset)))
		err = f.jobs.Cancel(ctx, f.admin, token, f.serverID, job.ID)
		done := finished(t, f.runner, job.ID)
		switch {
		case err == nil && done.Status == ops.Cancelled && f.startedAt() == before && f.running():
		case apperr.Is(err, "OPERATION_STARTED") || apperr.Is(err, "OPERATION_ENDED"):
			if done.Status != ops.Succeeded || f.startedAt() == before {
				t.Fatalf("refused, but %+v", done)
			}
		default:
			t.Fatalf("offset %v: cancel %v, %+v, restarted %v", offset, err, done, f.startedAt() != before)
		}
		outcomes[err == nil]++
	}
	t.Log("cancelled", outcomes[true], "refused", outcomes[false])
}

// A cancelled restart stays cancelled when Ludock restarts, and one that
// Ludock resumed after a restart can still be cancelled.
func TestCancelledStaysCancelledAcrossRestarts(t *testing.T) {
	f := setup(t)
	token := f.adminSession
	before := f.startedAt()
	runner, id := f.interruptedWarning(t, ops.PhaseWarning, ops.Recovery{WarnUntil: time.Now().Add(time.Minute).UnixMilli(), WarnSent: true})
	waitFor(t, runner, id, func(current *ops.Job) bool { return current.Status == ops.Running && current.Phase == ops.PhaseWarning })
	if err := f.jobs.Cancel(ctx, f.admin, token, f.serverID, id); err != nil {
		t.Fatal(err)
	}
	if job := finished(t, runner, id); job.Status != ops.Cancelled || job.Result["cancelledBy"] != "admin" {
		t.Fatalf("%+v", job)
	}
	runner.Stop()
	next := &ops.Runner{DB: f.jobs.DB, Locks: f.jobs.Locks}
	f.jobs.Register(next)
	if err := next.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(next.Stop)
	time.Sleep(200 * time.Millisecond)
	if job, _ := next.Get(ctx, id); job.Status != ops.Cancelled || f.startedAt() != before || !f.running() {
		t.Fatalf("%+v", job)
	}
	if err := f.jobs.Cancel(ctx, f.admin, token, f.serverID, id); !apperr.Is(err, "OPERATION_ENDED") {
		t.Fatal(err)
	}
}
