package schedules

import (
	"testing"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/ops"
)

// warnedRestart creates a daily restart for owner at the fixture's due
// time, warned 5 and 1 minutes ahead, and counts players down to it with
// the 5-minute warning. It returns the schedule and a count of call-offs.
func (f *fixture) warnedRestart(t *testing.T, owner *db.SessionUser) (Schedule, *int) {
	t.Helper()
	input := base
	input.Action = "restart"
	input.Announcements = &Announcements{LeadMinutes: []int{5, 1}}
	created, err := f.s.Create(ctx, owner, f.serverID, input)
	if err != nil {
		t.Fatal(err)
	}
	calls := new(int)
	f.s.calledOff = func(serverID, action string) {
		if serverID != f.serverID || action != "restart" {
			t.Errorf("called off %s on %s", action, serverID)
		}
		*calls++
	}
	f.s.Announce(ctx, due-5*minute)
	f.s.Wait()
	if f.claims(t) != 1 {
		t.Fatal("the countdown didn't start")
	}
	db.Now = func() int64 { return due - 4*minute }
	return *created, calls
}

func (f *fixture) claims(t *testing.T) int {
	var count int
	if err := f.s.DB.QueryRow("SELECT COUNT(*) FROM schedule_announcements").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// Pausing a schedule after its warning tells players once; deleting it
// afterwards, or resuming and pausing it before another warning, says
// nothing more. A resumed schedule counts down afresh.
func TestPauseCallsOffCountdownOnce(t *testing.T) {
	f := setup(t)
	schedule, calls := f.warnedRestart(t, admin)
	paused, err := f.s.SetEnabled(ctx, admin, f.serverID, schedule.ID, schedule.Revision, false)
	if err != nil || *calls != 1 {
		t.Fatal(err, *calls)
	}
	resumed, err := f.s.SetEnabled(ctx, admin, f.serverID, schedule.ID, paused.Revision, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetEnabled(ctx, admin, f.serverID, schedule.ID, resumed.Revision, false); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Delete(ctx, admin, f.serverID, schedule.ID); err != nil || *calls != 1 {
		t.Fatal(err, *calls)
	}
}

// Deleting a counted-down schedule tells players before the claims go with
// it.
func TestDeleteCallsOffCountdown(t *testing.T) {
	f := setup(t)
	schedule, calls := f.warnedRestart(t, admin)
	if err := f.s.Delete(ctx, admin, f.serverID, schedule.ID); err != nil || *calls != 1 || f.claims(t) != 0 {
		t.Fatal(err, *calls)
	}
}

// An edit that leaves the announced run as it was keeps the countdown and
// says nothing; one that moves the run, changes its action, or turns
// warnings off calls it off, once.
func TestEditCallsOffOnlyAChangedRun(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Input)
		off    bool
	}{
		{"other warning times", func(i *Input) { i.Announcements = &Announcements{LeadMinutes: []int{10, 1}} }, false},
		{"other days, same next run", func(i *Input) { i.Days = []int{3} }, false},
		{"a backup first", func(i *Input) { i.CreateBackup = true }, false},
		{"another time", func(i *Input) { i.Time = "09:00" }, true},
		{"another day", func(i *Input) { i.Days = []int{4} }, true},
		{"another action", func(i *Input) { i.Action = "stop" }, true},
		{"warnings off", func(i *Input) { i.Announcements = nil }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := setup(t)
			f.grant(t, authz.ServerView, authz.ServerRestart, authz.SchedulesManage)
			schedule, calls := f.warnedRestart(t, admin)
			f.s.calledOff = func(string, string) { *calls++ }
			input := schedule.Input
			test.change(&input)
			edited, err := f.s.Update(ctx, admin, f.serverID, schedule.ID, schedule.Revision, input)
			if err != nil {
				t.Fatal(err)
			}
			if want := map[bool]int{true: 1}[test.off]; *calls != want {
				t.Fatalf("called off %d times", *calls)
			}
			if err := f.s.Delete(ctx, admin, f.serverID, edited.ID); err != nil || *calls > 1 {
				t.Fatal(err, *calls)
			}
		})
	}
}

// Without a warning sent there is nothing to call off.
func TestNoCountdownNothingToCallOff(t *testing.T) {
	f := setup(t)
	input := base
	input.Action = "restart"
	input.Announcements = &Announcements{LeadMinutes: []int{5}}
	schedule, err := f.s.Create(ctx, admin, f.serverID, input)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.s.calledOff = func(string, string) { calls++ }
	if _, err := f.s.SetEnabled(ctx, admin, f.serverID, schedule.ID, schedule.Revision, false); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Delete(ctx, admin, f.serverID, schedule.ID); err != nil || calls != 0 {
		t.Fatal(err, calls)
	}
}

// A run whose operation is queued is called off by a pause, which its
// operation will be refused for, once; one already running has begun, and
// a pause leaves its countdown alone.
func TestPauseCallsOffOnlyAQueuedRun(t *testing.T) {
	for _, status := range []string{"queued", "running"} {
		t.Run(status, func(t *testing.T) {
			f := setup(t)
			schedule, calls := f.warnedRestart(t, admin)
			f.s.Run(ctx, due)
			queued := f.first(t, admin)
			if queued.LastOperation == nil {
				t.Fatal("not queued", queued.LastResult)
			}
			f.s.DB.Exec("UPDATE operations SET status=?", status)
			if _, err := f.s.SetEnabled(ctx, admin, f.serverID, schedule.ID, queued.Revision, false); err != nil {
				t.Fatal(err)
			}
			want := map[string]int{"queued": 1}[status]
			if *calls != want {
				t.Fatalf("called off %d times", *calls)
			}
			// The refused operation finds the countdown spent, or calls it off.
			counted, err := CallOffQueued(ctx, f.s.DB, schedule.ID, queued.LastOperation.ID)
			if err != nil || counted != (want == 0) {
				t.Fatal(err, counted)
			}
		})
	}
}

// A run the owner can no longer do is suspended when it comes due, and
// players counted down to it hear it's off.
func TestRevokedOwnersRunCallsOffCountdown(t *testing.T) {
	f := setup(t)
	f.grant(t, authz.ServerView, authz.ServerRestart, authz.SchedulesManage, authz.ConsoleExecute)
	_, calls := f.warnedRestart(t, friend)
	f.grant(t, authz.ServerView)
	f.s.Run(ctx, due)
	f.s.Run(ctx, due+30_000)
	if last := f.first(t, admin); f.operations(t) != 0 || *calls != 1 || last.LastResult == "" {
		t.Fatal(*calls, last.LastResult)
	}
}

// A warned waiting run that other work keeps from going ahead to the end
// is skipped, and players counted down to it hear it's off; one that goes
// ahead says nothing.
func TestSkippedWaitCallsOffCountdown(t *testing.T) {
	for _, blocked := range []bool{true, false} {
		t.Run(map[bool]string{true: "blocked", false: "goes ahead"}[blocked], func(t *testing.T) {
			f := setup(t)
			f.withPlayers(1, true)
			input := waitingRestart(10, WaitRun)
			input.Announcements = &Announcements{LeadMinutes: []int{5}}
			if _, err := f.s.Create(ctx, admin, f.serverID, input); err != nil {
				t.Fatal(err)
			}
			calls := 0
			f.s.calledOff = func(string, string) { calls++ }
			f.s.Run(ctx, due)
			f.s.Announce(ctx, due+5*minute)
			f.s.Wait()
			if f.claims(t) != 1 {
				t.Fatal("no countdown to the end of the wait")
			}
			if blocked {
				server, _ := f.s.Identity.Get(ctx, f.serverID)
				if _, err := f.s.Runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: admin.ID, Kind: "start",
					BindingRevision: server.BindingRevision}); err != nil {
					t.Fatal(err)
				}
			}
			f.s.Run(ctx, due+15*minute-20_000)
			want := map[bool]int{true: 1}[blocked]
			if last := f.first(t, admin); calls != want || (blocked != (last.LastOperation == nil)) {
				t.Fatalf("called off %d times: %s", calls, last.LastResult)
			}
		})
	}
}
