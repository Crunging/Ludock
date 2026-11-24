package jobs

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/schedules"
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

// A schedule counts players down before its operation exists, so the
// warning isn't on the operation. A cancelled scheduled run calls it off
// when its own run was counted down and players heard it, at the binding
// they heard it on; not when only an earlier run was, nobody was warned, or
// the warning never reached them.
func TestCancelFindsAScheduledRunsCountdown(t *testing.T) {
	f := setup(t)
	// Nothing runs the queue, as when other work holds it.
	f.runner.Stop()
	server, _ := f.jobs.Identity.Get(ctx, f.serverID)
	for index, test := range []struct {
		announced string
		heard     bool
		want      int64
	}{{"", false, 0}, {"2026-01-01T05:00", true, server.BindingRevision}, {"2026-01-01T05:00", false, 0}, {"2025-12-31T05:00", true, 0}} {
		scheduleID := fmt.Sprintf("schedule-%d", index)
		job, err := f.runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: "admin", Kind: "restart",
			BindingRevision: server.BindingRevision, Input: map[string]any{"scheduleId": scheduleID}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.jobs.DB.ExecContext(ctx, `INSERT INTO schedules(id,server_id,owner_id,input_json,binding_revision,last_slot,created_at,changed_at,last_operation_id)
      VALUES(?,?,'admin','{}',?,'2026-01-01T05:00',1,1,?)`, scheduleID, f.serverID, server.BindingRevision, job.ID); err != nil {
			t.Fatal(err)
		}
		if test.announced != "" {
			heard := sql.NullInt64{Int64: server.BindingRevision, Valid: test.heard}
			if _, err := f.jobs.DB.ExecContext(ctx, "INSERT INTO schedule_announcements(schedule_id,slot,lead_minutes,heard_binding) VALUES(?,?,1,?)",
				scheduleID, test.announced, heard); err != nil {
				t.Fatal(err)
			}
		}
		if got := f.jobs.countedDown(ctx, job); got != test.want {
			t.Fatalf("announced %q, heard %v: %d", test.announced, test.heard, got)
		}
		earlier := *job
		earlier.ID = "another-operation"
		if f.jobs.countedDown(ctx, &earlier) != 0 {
			t.Fatal("an operation the schedule didn't queue last was counted down")
		}
		if err := f.jobs.Cancel(ctx, f.admin, f.adminSession, f.serverID, job.ID); err != nil {
			t.Fatal(err)
		}
	}
}

// A scheduled restart whose owner lost access while it waited in the queue,
// to the schedule or only to restarting, is refused when its turn comes,
// before it acts, and spends its run's countdown. Players hear it's off on
// the binding they heard the warning on; not once the server was rebound,
// and not when the warning never reached them.
func TestRefusedScheduledRunCallsOffCountdown(t *testing.T) {
	for _, test := range []struct {
		name, owner string
		heard       bool
		rebound     bool
		said        int
	}{
		{"owner disabled", "admin", true, false, 1},
		{"owner can't restart", "member", true, false, 1},
		{"rebound since the warning", "admin", true, true, 0},
		{"warning never heard", "admin", false, false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := setup(t)
			serverID, _, received := f.commandServer(t, "")
			f.runner.Stop()
			if test.owner == "member" {
				f.jobs.DB.CreateUser(ctx, db.UserRecord{User: db.User{ID: "member", Username: "member", Role: db.RoleMember, CreatedAt: 1}, PasswordHash: "x"})
				if err := f.jobs.Authz.Share(ctx, f.admin, f.adminSession, "member", serverID,
					[]authz.Capability{authz.ServerView, authz.SchedulesManage}); err != nil {
					t.Fatal(err)
				}
			}
			warned, _ := f.jobs.Identity.Get(ctx, serverID)
			// Rebound after the warning, before the run was queued: the
			// operation is on the new binding, the players heard the old.
			if test.rebound {
				f.jobs.DB.ExecContext(ctx, "UPDATE logical_servers SET binding_revision=binding_revision+1 WHERE id=?", serverID)
			}
			server, _ := f.jobs.Identity.Get(ctx, serverID)
			job, err := f.runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: serverID, ActorID: test.owner, Kind: "restart",
				BindingRevision: server.BindingRevision, Input: map[string]any{"scheduleId": "schedule", "scheduleRevision": 1}})
			if err != nil {
				t.Fatal(err)
			}
			heard := sql.NullInt64{Int64: warned.BindingRevision, Valid: test.heard}
			if _, err := f.jobs.DB.ExecContext(ctx, `INSERT INTO schedules(id,server_id,owner_id,input_json,binding_revision,last_slot,created_at,changed_at,last_operation_id)
        VALUES('schedule',?,?,'{"kind":"recurring","action":"restart","enabled":true,"time":"05:00","days":[4],"timezone":"UTC"}',1,'2026-01-01T05:00',1,1,?)`,
				serverID, test.owner, job.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.jobs.DB.ExecContext(ctx, "INSERT INTO schedule_announcements(schedule_id,slot,lead_minutes,heard_binding) VALUES('schedule','2026-01-01T05:00',1,?)",
				heard); err != nil {
				t.Fatal(err)
			}
			if test.owner == "admin" {
				f.jobs.DB.ExecContext(ctx, "UPDATE users SET disabled=1 WHERE id='admin'")
			}
			next := &ops.Runner{DB: f.jobs.DB, Locks: f.jobs.Locks}
			f.jobs.Register(next)
			if err := next.Start(ctx, nil); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(next.Stop)
			if done := finished(t, next, job.ID); done.Status != ops.Failed {
				t.Fatalf("%+v", done)
			}
			if got := strings.Count(strings.Join(received(), "\n"), "Server restart cancelled."); got != test.said {
				t.Fatalf("said it's off %d times: %q", got, received())
			}
			if again, _ := schedules.CallOffQueued(ctx, f.jobs.DB, "schedule", job.ID); again != 0 {
				t.Fatal("the refusal left the countdown for another call-off")
			}
		})
	}
}
