package schedules

import (
	"database/sql"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/console"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/self"
	"github.com/Crunging/Ludock/internal/servers"
)

// warnedRestart creates a daily restart for owner at the fixture's due
// time, warned 5 and 1 minutes ahead, and counts players down to it with
// the 5-minute warning, delivered on the server's binding. It returns the
// schedule and a count of call-offs.
func (f *fixture) warnedRestart(t *testing.T, owner *db.SessionUser) (Schedule, *int) {
	t.Helper()
	input := base
	input.Action = "restart"
	input.Announcements = &Announcements{LeadMinutes: []int{5, 1}}
	created, err := f.s.Create(ctx, owner, session(owner), f.serverID, input)
	if err != nil {
		t.Fatal(err)
	}
	calls := new(int)
	server, _ := f.s.Identity.Get(ctx, f.serverID)
	f.s.calledOff = func(serverID, action string, binding int64) {
		if serverID != f.serverID || action != "restart" || binding != server.BindingRevision {
			t.Errorf("called off %s on %s at %d", action, serverID, binding)
		}
		*calls++
	}
	f.s.Announce(ctx, due-5*minute)
	f.s.Wait()
	f.delivered(t)
	db.Now = func() int64 { return due - 4*minute }
	return *created, calls
}

// delivered marks the one claimed warning as heard on the server's current
// binding, as a console that took it would.
func (f *fixture) delivered(t *testing.T) {
	t.Helper()
	server, _ := f.s.Identity.Get(ctx, f.serverID)
	result, err := f.s.DB.Exec("UPDATE schedule_announcements SET heard_binding=?", server.BindingRevision)
	if err != nil {
		t.Fatal(err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		t.Fatal("the countdown didn't start")
	}
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
	paused, err := f.s.SetEnabled(ctx, admin, adminSession, f.serverID, schedule.ID, schedule.Revision, false)
	if err != nil || *calls != 1 {
		t.Fatal(err, *calls)
	}
	resumed, err := f.s.SetEnabled(ctx, admin, adminSession, f.serverID, schedule.ID, paused.Revision, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetEnabled(ctx, admin, adminSession, f.serverID, schedule.ID, resumed.Revision, false); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Delete(ctx, admin, adminSession, f.serverID, schedule.ID); err != nil || *calls != 1 {
		t.Fatal(err, *calls)
	}
}

// Deleting a counted-down schedule tells players before the claims go with
// it.
func TestDeleteCallsOffCountdown(t *testing.T) {
	f := setup(t)
	schedule, calls := f.warnedRestart(t, admin)
	if err := f.s.Delete(ctx, admin, adminSession, f.serverID, schedule.ID); err != nil || *calls != 1 || f.claims(t) != 0 {
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
			f.s.calledOff = func(string, string, int64) { *calls++ }
			input := schedule.Input
			test.change(&input)
			edited, err := f.s.Update(ctx, admin, adminSession, f.serverID, schedule.ID, schedule.Revision, input)
			if err != nil {
				t.Fatal(err)
			}
			if want := map[bool]int{true: 1}[test.off]; *calls != want {
				t.Fatalf("called off %d times", *calls)
			}
			if err := f.s.Delete(ctx, admin, adminSession, f.serverID, edited.ID); err != nil || *calls > 1 {
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
	schedule, err := f.s.Create(ctx, admin, adminSession, f.serverID, input)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.s.calledOff = func(string, string, int64) { calls++ }
	if _, err := f.s.SetEnabled(ctx, admin, adminSession, f.serverID, schedule.ID, schedule.Revision, false); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Delete(ctx, admin, adminSession, f.serverID, schedule.ID); err != nil || calls != 0 {
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
			if _, err := f.s.SetEnabled(ctx, admin, adminSession, f.serverID, schedule.ID, queued.Revision, false); err != nil {
				t.Fatal(err)
			}
			want := map[string]int{"queued": 1}[status]
			if *calls != want {
				t.Fatalf("called off %d times", *calls)
			}
			// The refused operation finds the countdown spent, or calls it off.
			heard, err := CallOffQueued(ctx, f.s.DB, schedule.ID, queued.LastOperation.ID)
			if err != nil || (heard != 0) != (want == 0) {
				t.Fatal(err, heard)
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
			if _, err := f.s.Create(ctx, admin, adminSession, f.serverID, input); err != nil {
				t.Fatal(err)
			}
			calls := 0
			f.s.calledOff = func(string, string, int64) { calls++ }
			f.s.Run(ctx, due)
			f.s.Announce(ctx, due+5*minute)
			f.s.Wait()
			f.delivered(t)
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

// An edit while a due run waits for other work to finish ends that run, even
// one that keeps its settings, so the countdown to it is called off.
func TestEditEndsADueRunsCountdown(t *testing.T) {
	f := setup(t)
	schedule, calls := f.warnedRestart(t, admin)
	server, _ := f.s.Identity.Get(ctx, f.serverID)
	if _, err := f.s.Runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: admin.ID, Kind: "start",
		BindingRevision: server.BindingRevision}); err != nil {
		t.Fatal(err)
	}
	f.s.Run(ctx, due)
	if f.operations(t) != 1 || *calls != 0 {
		t.Fatal("the run didn't wait for the other work")
	}
	db.Now = func() int64 { return due + 2*minute }
	input := schedule.Input
	input.Announcements = &Announcements{LeadMinutes: []int{10, 1}}
	if _, err := f.s.Update(ctx, admin, adminSession, f.serverID, schedule.ID, schedule.Revision, input); err != nil || *calls != 1 {
		t.Fatal(err, *calls)
	}
}

// withConsole moves the fixture to a running Minecraft server on a fake
// Docker daemon, with labels added, whose RCON console records what it
// receives, and sends warnings through it.
func (f *fixture) withConsole(t *testing.T, labels map[string]string) func(text string) int {
	t.Helper()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	var mu sync.Mutex
	var received []string
	port, err := fakedocker.ServeRCON("rcon-secret", func(command string) string {
		mu.Lock()
		received = append(received, command)
		mu.Unlock()
		if f.onCommand != nil {
			f.onCommand(command)
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]string{"ludock.console.host": "127.0.0.1", "ludock.console.port": fmt.Sprint(port)}
	maps.Copy(all, labels)
	engine.AddContainer(fakedocker.GameOptions{Name: "console", Image: "itzg/minecraft-server", Running: true, Labels: all,
		Env: []string{"RCON_PASSWORD=rcon-secret"}})
	client := docker.New(engine.Socket)
	f.s.Servers = &servers.Service{DB: f.s.DB, Identity: f.s.Identity, Authz: f.s.Authz, Observer: &servers.Observer{Docker: client, Own: self.New(nil, "")}}
	f.s.Console = console.Broadcaster{Runtime: &console.Runtime{Docker: client}}
	if _, err := f.s.Servers.RefreshNow(ctx); err != nil {
		t.Fatal(err)
	}
	views, err := f.s.Servers.List(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		if view.Name == "console" {
			f.serverID = view.ID
		}
	}
	return func(text string) int {
		mu.Lock()
		defer mu.Unlock()
		return strings.Count(strings.Join(received, "\n"), text)
	}
}

// soonRestart creates a daily restart warned 5 minutes ahead, at a time at
// least five minutes away in real time, so its warning can still go out,
// and counts players down to it. It returns the schedule and its run time.
func (f *fixture) soonRestart(t *testing.T) (*Schedule, int64) {
	t.Helper()
	runAt := time.Now().Add(6 * time.Minute).Truncate(time.Minute)
	input := base
	input.Action, input.Timezone, input.Time = "restart", "UTC", runAt.UTC().Format("15:04")
	input.Announcements = &Announcements{LeadMinutes: []int{5}}
	schedule, err := f.s.Create(ctx, admin, adminSession, f.serverID, input)
	if err != nil {
		t.Fatal(err)
	}
	f.s.Announce(ctx, runAt.UnixMilli()-5*minute)
	f.s.Wait()
	if f.claims(t) != 1 {
		t.Fatal("the countdown didn't start")
	}
	db.Now = func() int64 { return runAt.UnixMilli() - 4*minute }
	return schedule, runAt.UnixMilli()
}

func (f *fixture) heardOn(t *testing.T) sql.NullInt64 {
	t.Helper()
	var heard sql.NullInt64
	if err := f.s.DB.QueryRow("SELECT heard_binding FROM schedule_announcements").Scan(&heard); err != nil {
		t.Fatal(err)
	}
	return heard
}

// A warning that reached players records the binding it went out on, and a
// pause tells them it's off there; once the server is rebound, the console
// there may not be the one they heard it on, so nothing is said.
func TestCallOffGoesOnlyToTheWarnedBinding(t *testing.T) {
	for _, rebound := range []bool{false, true} {
		t.Run(map[bool]string{false: "same binding", true: "rebound"}[rebound], func(t *testing.T) {
			f := setup(t)
			said := f.withConsole(t, nil)
			schedule, _ := f.soonRestart(t)
			server, _ := f.s.Identity.Get(ctx, f.serverID)
			if said("Server restart in 5 minutes.") != 1 || f.heardOn(t).Int64 != server.BindingRevision {
				t.Fatal("the warning wasn't recorded as heard", f.heardOn(t))
			}
			if rebound {
				f.s.DB.Exec("UPDATE logical_servers SET binding_revision=binding_revision+1 WHERE id=?", f.serverID)
			}
			if _, err := f.s.SetEnabled(ctx, admin, adminSession, f.serverID, schedule.ID, schedule.Revision, false); err != nil {
				t.Fatal(err)
			}
			f.s.Wait()
			if got, want := said("Server restart cancelled."), map[bool]int{false: 1}[rebound]; got != want || f.claims(t) != 0 {
				t.Fatalf("said it's off %d times", got)
			}
		})
	}
}

// A run called off while its warning is on its way to players spends a
// claim nobody is recorded as having heard, so it says nothing. The warning
// then finds its claim gone and tells them itself, once.
func TestWarningCalledOffWhileItGoesOutIsTakenBack(t *testing.T) {
	f := setup(t)
	said := f.withConsole(t, nil)
	f.onCommand = func(command string) {
		if strings.Contains(command, "Server restart in 5 minutes.") {
			if binding, err := callOff(ctx, f.s.DB, f.scheduleID(t), f.slot(t)); err != nil || binding != 0 {
				t.Error("the call-off saw a warning recorded before it arrived", binding, err)
			}
		}
	}
	runAt := time.Now().Add(6 * time.Minute).Truncate(time.Minute)
	input := base
	input.Action, input.Timezone, input.Time = "restart", "UTC", runAt.UTC().Format("15:04")
	input.Announcements = &Announcements{LeadMinutes: []int{5}}
	if _, err := f.s.Create(ctx, admin, adminSession, f.serverID, input); err != nil {
		t.Fatal(err)
	}
	f.s.Announce(ctx, runAt.UnixMilli()-5*minute)
	f.s.Wait()
	if warned, off := said("Server restart in 5 minutes."), said("Server restart cancelled."); warned != 1 || off != 1 || f.claims(t) != 0 {
		t.Fatalf("warned %d times, called off %d times", warned, off)
	}
}

func (f *fixture) scheduleID(t *testing.T) string {
	t.Helper()
	var id string
	if err := f.s.DB.QueryRow("SELECT schedule_id FROM schedule_announcements").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) slot(t *testing.T) string {
	t.Helper()
	var slot string
	if err := f.s.DB.QueryRow("SELECT slot FROM schedule_announcements").Scan(&slot); err != nil {
		t.Fatal(err)
	}
	return slot
}

// A countdown nobody heard is never called off: the warning was claimed but
// the pause came before it went out, or the game can't message players.
func TestUnheardWarningIsNotCalledOff(t *testing.T) {
	t.Run("paused before it went out", func(t *testing.T) {
		f := setup(t)
		// Without a console the claimed warning never goes out, as when the
		// pause wins the race to it.
		_, calls := f.warnedRestartUnheard(t)
		if *calls != 0 {
			t.Fatalf("called off %d times", *calls)
		}
	})
	t.Run("no broadcast", func(t *testing.T) {
		f := setup(t)
		said := f.withConsole(t, map[string]string{"ludock.console": "source-rcon"})
		schedule, _ := f.soonRestart(t)
		if f.heardOn(t).Valid || said("Server restart") != 0 {
			t.Fatal("a game that can't message players heard the warning")
		}
		if _, err := f.s.SetEnabled(ctx, admin, adminSession, f.serverID, schedule.ID, schedule.Revision, false); err != nil {
			t.Fatal(err)
		}
		f.s.Wait()
		if said("Server restart") != 0 || f.claims(t) != 0 {
			t.Fatal("called off a warning nobody heard")
		}
	})
}

// warnedRestartUnheard claims a warning that never reaches players, then
// pauses the schedule, and returns the schedule and its call-offs.
func (f *fixture) warnedRestartUnheard(t *testing.T) (*Schedule, *int) {
	t.Helper()
	input := base
	input.Action = "restart"
	input.Announcements = &Announcements{LeadMinutes: []int{5}}
	schedule, err := f.s.Create(ctx, admin, adminSession, f.serverID, input)
	if err != nil {
		t.Fatal(err)
	}
	calls := new(int)
	f.s.calledOff = func(string, string, int64) { *calls++ }
	f.s.Announce(ctx, due-5*minute)
	f.s.Wait()
	if f.claims(t) != 1 || f.heardOn(t).Valid {
		t.Fatal("expected one unheard claim")
	}
	db.Now = func() int64 { return due - 4*minute }
	if _, err := f.s.SetEnabled(ctx, admin, adminSession, f.serverID, schedule.ID, schedule.Revision, false); err != nil {
		t.Fatal(err)
	}
	return schedule, calls
}
