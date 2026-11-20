package schedules

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/ops"
)

type fakePlayers struct {
	now    Occupancy
	lists  bool
	checks int
}

// Online reports a reading taken during any wait unless the test dates it.
func (p *fakePlayers) Online(context.Context, *db.SessionUser, string, authz.Capability, int64) (Occupancy, error) {
	occupancy := p.now
	if occupancy.Known && occupancy.ReadAt == 0 {
		occupancy.ReadAt = 1 << 62
	}
	return occupancy, nil
}

func (p *fakePlayers) Lists(context.Context, *db.SessionUser, string) (bool, error) {
	return p.lists, nil
}

func (p *fakePlayers) Check(context.Context, string) { p.checks++ }

const minute = int64(60_000)

func waitingRestart(minutes int, then string) Input {
	input := base
	input.Action = "restart"
	input.WaitForEmpty = &WaitForEmpty{Minutes: minutes, Then: then}
	return input
}

func (f *fixture) withPlayers(online int, known bool) *fakePlayers {
	players := &fakePlayers{now: Occupancy{Online: online, Known: known}, lists: true}
	f.s.Players = players
	return players
}

// A restart due with players online waits from its scheduled minute, asks
// for readings at most every 30 seconds, and runs once the server empties;
// with players still online at the end, it skips without notifying, and the
// next day's run starts afresh.
func TestWaitForEmptyWindow(t *testing.T) {
	f := setup(t)
	var notified []string
	f.s.Notify = func(_ notify.Alert, key, message string) { notified = append(notified, key) }
	players := f.withPlayers(2, true)
	if _, err := f.s.Create(ctx, admin, f.serverID, waitingRestart(30, WaitSkip)); err != nil {
		t.Fatal(err)
	}
	// A tick a few seconds into the minute still waits from the minute.
	f.s.Run(ctx, due+5_000)
	first := f.first(t, admin)
	if f.operations(t) != 0 || first.LastResult != WaitingResult || first.WaitingUntil != due+30*minute || first.LastRunAt != due ||
		first.CompletedAt != 0 || players.checks != 1 {
		t.Fatal(first, players.checks)
	}
	f.s.Run(ctx, due+20_000)
	f.s.Run(ctx, due+40_000)
	if players.checks != 2 {
		t.Fatal("readings were not throttled", players.checks)
	}
	f.s.Run(ctx, due+30*minute-1)
	if f.operations(t) != 0 || f.first(t, admin).LastResult != WaitingResult {
		t.Fatal("stopped waiting early")
	}
	f.s.Run(ctx, due+30*minute)
	if last := f.first(t, admin); f.operations(t) != 0 || last.LastResult != string(SkipPlayersOnline) || last.WaitingUntil != 0 ||
		len(notified) != 0 {
		t.Fatal(last, notified)
	}
	f.s.Run(ctx, due+31*minute)
	if f.operations(t) != 0 {
		t.Fatal("ran a skipped slot")
	}

	// The next day, the server empties during the wait.
	tomorrow := due + 86_400_000
	f.s.Run(ctx, tomorrow)
	players.now.Online = 0
	f.s.Run(ctx, tomorrow+3*minute)
	if last := f.first(t, admin); f.operations(t) != 1 || last.LastOperation == nil || last.LastRunAt != tomorrow+3*minute {
		t.Fatal(last)
	}
	f.s.Run(ctx, tomorrow+4*minute)
	if f.operations(t) != 1 {
		t.Fatal("ran a slot twice")
	}

	// An empty running server runs once a reading asked for at its time has
	// had time to land, never on a reading from before; a stopped server runs
	// at once.
	f.s.DB.Exec("UPDATE operations SET status='succeeded'")
	day3 := tomorrow + 86_400_000
	f.s.Run(ctx, day3)
	f.s.Run(ctx, day3+15_000)
	if f.operations(t) != 1 {
		t.Fatal("acted on a reading from before the run was due")
	}
	f.s.Run(ctx, day3+30_000)
	if f.operations(t) != 2 {
		t.Fatal("waited on an empty server")
	}
	f.s.DB.Exec("UPDATE operations SET status='succeeded'")
	players.now.Stopped = true
	f.s.Run(ctx, day3+86_400_000)
	if f.operations(t) != 3 {
		t.Fatal("waited on a stopped server")
	}
}

// At the end of a wait, a schedule set to go ahead runs with players
// online, and one set to skip records an unknown count as a skip that
// notifies, since it may mean a broken console.
func TestWaitForEmptyEnds(t *testing.T) {
	f := setup(t)
	f.withPlayers(1, true)
	if _, err := f.s.Create(ctx, admin, f.serverID, waitingRestart(10, WaitRun)); err != nil {
		t.Fatal(err)
	}
	f.s.Run(ctx, due)
	f.s.Run(ctx, due+10*minute)
	if f.operations(t) != 1 {
		t.Fatal("did not go ahead at the end of the wait")
	}

	f.s.DB.Exec("DELETE FROM schedules")
	f.s.DB.Exec("UPDATE operations SET status='succeeded'")
	var notified []string
	f.s.Notify = func(_ notify.Alert, key, message string) { notified = append(notified, key) }
	f.withPlayers(0, false)
	if _, err := f.s.Create(ctx, admin, f.serverID, waitingRestart(10, WaitSkip)); err != nil {
		t.Fatal(err)
	}
	f.s.Run(ctx, due)
	if f.first(t, admin).WaitingUntil == 0 {
		t.Fatal("an unknown count did not wait")
	}
	f.s.Run(ctx, due+10*minute)
	if last := f.first(t, admin); f.operations(t) != 1 || !strings.HasPrefix(last.LastResult, "Skipped: Ludock couldn't tell") || len(notified) != 1 {
		t.Fatal(last, notified)
	}
}

// A waiting run is saved state: a new service after a Ludock restart keeps
// waiting while the wait lasts, runs once, and a wait that ended more than
// five minutes before Ludock came back is recorded as missed.
func TestWaitingRunRecoversAfterRestart(t *testing.T) {
	f := setup(t)
	f.withPlayers(3, true)
	if _, err := f.s.Create(ctx, admin, f.serverID, waitingRestart(30, WaitRun)); err != nil {
		t.Fatal(err)
	}
	f.s.Run(ctx, due)
	restarted := &Service{DB: f.s.DB, Authz: f.s.Authz, Identity: f.s.Identity, Runner: f.s.Runner, Locks: f.s.Locks,
		Players: &fakePlayers{now: Occupancy{Known: true}, lists: true}}
	restarted.Run(ctx, due+20*minute)
	restarted.Run(ctx, due+20*minute+15_000)
	restarted.Run(ctx, due+20*minute+30_000)
	f.s.Run(ctx, due+21*minute)
	if f.operations(t) != 1 {
		t.Fatal("a recovered wait did not run exactly once", f.operations(t))
	}

	f.s.DB.Exec("DELETE FROM schedules")
	f.s.DB.Exec("DELETE FROM operations")
	if _, err := f.s.Create(ctx, admin, f.serverID, waitingRestart(30, WaitRun)); err != nil {
		t.Fatal(err)
	}
	f.s.Run(ctx, due)
	restarted.Run(ctx, due+36*minute)
	if last := f.first(t, admin); f.operations(t) != 0 || !strings.HasPrefix(last.LastResult, "Missed:") {
		t.Fatal(last)
	}
}

// Losing the action grant while a run waits ends the wait when it would
// act, and pausing or editing a schedule ends its wait.
func TestWaitEndsOnRevocationOrChange(t *testing.T) {
	f := setup(t)
	f.grant(t, authz.ServerView, authz.ServerRestart, authz.SchedulesManage)
	players := f.withPlayers(1, true)
	if _, err := f.s.Create(ctx, friend, f.serverID, waitingRestart(30, WaitRun)); err != nil {
		t.Fatal(err)
	}
	f.s.Run(ctx, due)
	f.grant(t, authz.ServerView, authz.SchedulesManage)
	players.now.Online = 0
	f.s.Run(ctx, due+minute)
	if last := f.first(t, friend); f.operations(t) != 0 || last.WaitingUntil != 0 ||
		!strings.HasPrefix(last.LastResult, "Suspended: the schedule owner can no longer do this action") {
		t.Fatal(last)
	}

	f.grant(t, authz.ServerView, authz.ServerRestart, authz.SchedulesManage)
	players.now.Online = 1
	tomorrow := due + 86_400_000
	f.s.Run(ctx, tomorrow)
	saved := f.first(t, friend)
	if saved.WaitingUntil == 0 {
		t.Fatal("did not wait")
	}
	paused, err := f.s.SetEnabled(ctx, friend, f.serverID, saved.ID, saved.Revision, false)
	if err != nil || paused.WaitingUntil != 0 || !strings.Contains(paused.LastResult, "paused while it waited") {
		t.Fatal(paused, err)
	}
	players.now.Online = 0
	f.s.Run(ctx, tomorrow+minute)
	if f.operations(t) != 0 {
		t.Fatal("a paused wait ran")
	}
}

// Waiting is offered only for actions that take the server down, on servers
// whose players Ludock can read, and countdown warnings need a run that goes
// ahead.
func TestWaitForEmptyValidation(t *testing.T) {
	f := setup(t)
	players := f.withPlayers(0, true)
	start := base
	start.WaitForEmpty = &WaitForEmpty{Minutes: 30, Then: WaitSkip}
	warned := waitingRestart(30, WaitSkip)
	warned.Announcements = &Announcements{LeadMinutes: []int{5}}
	for _, input := range []Input{start, waitingRestart(0, WaitSkip), waitingRestart(MaxWaitMinutes+1, WaitSkip),
		waitingRestart(30, "later"), warned} {
		if _, err := f.s.Create(ctx, admin, f.serverID, input); !apperr.Is(err, "INVALID_REQUEST") {
			t.Fatal(input, err)
		}
	}
	warned.WaitForEmpty.Then = WaitRun
	if _, err := f.s.Create(ctx, admin, f.serverID, warned); err != nil {
		t.Fatal(err)
	}
	players.lists = false
	if _, err := f.s.Create(ctx, admin, f.serverID, waitingRestart(30, WaitSkip)); err != errNoPlayerList {
		t.Fatal(err)
	}
}

// Warnings of a schedule that goes ahead after waiting count down to the
// end of the wait, not to the scheduled time.
func TestWaitingWarningsCountDownToTheEnd(t *testing.T) {
	f := setup(t)
	f.withPlayers(1, true)
	input := waitingRestart(10, WaitRun)
	input.Announcements = &Announcements{LeadMinutes: []int{5}}
	if _, err := f.s.Create(ctx, admin, f.serverID, input); err != nil {
		t.Fatal(err)
	}
	claims := func() int {
		var count int
		f.s.DB.QueryRow("SELECT COUNT(*) FROM schedule_announcements").Scan(&count)
		return count
	}
	f.s.Announce(ctx, due-5*minute)
	if claims() != 0 {
		t.Fatal("warned for the scheduled time")
	}
	f.s.Run(ctx, due)
	f.s.Announce(ctx, due+5*minute)
	f.s.Wait()
	if claims() != 1 {
		t.Fatal("did not warn before the end of the wait")
	}
}

// Two runs waiting on one server share its reading requests, and each still
// sees a settled reading even when the other asks first on every tick.
func TestWaitingRunsShareReadings(t *testing.T) {
	s := &Service{}
	for range 2 {
		if _, settled := s.checkPlayers(ctx, "server", due, due); settled {
			t.Fatal("a reading asked for just now counted as settled")
		}
	}
	for at := due + 15_000; at <= due+90_000; at += 15_000 {
		_, first := s.checkPlayers(ctx, "server", due, at)
		_, second := s.checkPlayers(ctx, "server", due, at)
		if settled := at >= due+readingSettleMs; first != settled || second != settled {
			t.Fatal(at-due, first, second)
		}
	}
	if _, settled := s.checkPlayers(ctx, "server", due+120_000, due+120_000); settled {
		t.Fatal("a reading from before the wait counted")
	}
}

// A wait set to skip never acts after its limit, even on a zero confirmed
// only then, which counts as unknown; and a
// stopped server busy with other work keeps the run's whole wait.
func TestWaitEdges(t *testing.T) {
	f := setup(t)
	players := f.withPlayers(0, true)
	if _, err := f.s.Create(ctx, admin, f.serverID, waitingRestart(10, WaitSkip)); err != nil {
		t.Fatal(err)
	}
	// Found waiting by a service that has asked for no reading yet, as
	// after a Ludock restart just before the end of the wait.
	if _, err := f.s.DB.Exec("UPDATE schedules SET last_slot=?,last_run_at=?,last_result=?", "2026-09-09T08:00:America/Los_Angeles", due, WaitingResult); err != nil {
		t.Fatal(err)
	}
	f.s.Run(ctx, due+10*minute-15_000)
	f.s.Run(ctx, due+10*minute)
	f.s.Run(ctx, due+10*minute+30_000)
	if last := f.first(t, admin); f.operations(t) != 0 || !strings.HasPrefix(last.LastResult, "Skipped: Ludock couldn't tell") {
		t.Fatal(last)
	}
	// Nor on a zero that settles after the end.
	if _, err := f.s.DB.Exec("UPDATE schedules SET last_result=?,last_run_at=?", WaitingResult, due); err != nil {
		t.Fatal(err)
	}
	f.s.Run(ctx, due+10*minute-20_000)
	f.s.Run(ctx, due+10*minute+10_000)
	if last := f.first(t, admin); f.operations(t) != 0 || last.WaitingUntil != 0 {
		t.Fatal(last)
	}

	f.s.DB.Exec("DELETE FROM schedules")
	players.now.Stopped = true
	if _, err := f.s.Create(ctx, admin, f.serverID, waitingRestart(30, WaitSkip)); err != nil {
		t.Fatal(err)
	}
	busy, err := f.s.Runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: admin.ID, Kind: "backup"})
	if err != nil {
		t.Fatal(err)
	}
	f.s.Run(ctx, due)
	f.s.Run(ctx, due+6*minute)
	if last := f.first(t, admin); f.operations(t) != 1 || last.WaitingUntil != due+30*minute {
		t.Fatal(last)
	}
	f.s.DB.Exec("UPDATE operations SET status='succeeded' WHERE id=?", busy.ID)
	f.s.Run(ctx, due+7*minute)
	if f.operations(t) != 2 {
		t.Fatal("did not run once the other work finished")
	}
}

// The reading asked for when a wait begins never lands, so the last one,
// an empty count from just before the scheduled time, is all Ludock has:
// it doesn't count, since someone may have joined after it, and the wait
// ends as unknown rather than restarting.
func TestWaitIgnoresEarlierEmptyReading(t *testing.T) {
	f := setup(t)
	players := f.withPlayers(0, true)
	players.now.ReadAt = due - 20_000
	if _, err := f.s.Create(ctx, admin, f.serverID, waitingRestart(1, WaitSkip)); err != nil {
		t.Fatal(err)
	}
	f.s.Run(ctx, due)
	f.s.Run(ctx, due+45_000)
	if first := f.first(t, admin); f.operations(t) != 0 || first.LastResult != WaitingResult {
		t.Fatal("acted on a reading from before the wait", first)
	}
	// A reading taken during the wait does count.
	players.now.ReadAt = due + 50_000
	f.s.Run(ctx, due+55_000)
	if f.operations(t) != 1 {
		t.Fatal("did not act on a reading from during the wait")
	}
}

// A stopped server is busy with other work for a whole wait set to skip:
// the run is recorded as blocked by that work, not as an unreadable player
// count, since a stopped server has no players to count.
func TestWaitStoppedServerBusyToTheEnd(t *testing.T) {
	f := setup(t)
	players := f.withPlayers(0, true)
	players.now.Stopped = true
	if _, err := f.s.Create(ctx, admin, f.serverID, waitingRestart(10, WaitSkip)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: admin.ID, Kind: "backup"}); err != nil {
		t.Fatal(err)
	}
	f.s.Run(ctx, due)
	f.s.Run(ctx, due+10*minute)
	f.s.Run(ctx, due+15*minute-15_000)
	if last := f.first(t, admin); f.operations(t) != 1 || last.LastResult != "Skipped: the server wasn’t ready, or other work on it was running" {
		t.Fatal(last.LastResult)
	}
}

// A running server is confirmed empty during a wait set to skip, but other
// work holds it until the wait ends, and its readings stop meanwhile. The
// run is recorded as blocked by that work, not as an unreadable count.
func TestWaitRunningServerBusyToTheEnd(t *testing.T) {
	f := setup(t)
	players := f.withPlayers(0, true)
	players.now.ReadAt = due + 1_000
	if _, err := f.s.Create(ctx, admin, f.serverID, waitingRestart(10, WaitSkip)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: admin.ID, Kind: "backup"}); err != nil {
		t.Fatal(err)
	}
	f.s.Run(ctx, due)
	f.s.Run(ctx, due+30_000)
	if last := f.first(t, admin); f.operations(t) != 1 || last.LastResult != WaitingResult {
		t.Fatal(last.LastResult)
	}
	f.s.Run(ctx, due+10*minute)
	if last := f.first(t, admin); f.operations(t) != 1 || last.LastResult != "Skipped: the server wasn’t ready, or other work on it was running" {
		t.Fatal(last.LastResult)
	}
}

// The first reading of a wait finds nobody, and someone joins before the
// next: that reading is older than the check just asked for, so the run
// waits for the new one instead of kicking whoever joined.
func TestWaitNeedsReadingSinceLatestCheck(t *testing.T) {
	f := setup(t)
	players := f.withPlayers(0, true)
	players.now.ReadAt = due + 2_000
	if _, err := f.s.Create(ctx, admin, f.serverID, waitingRestart(10, WaitRun)); err != nil {
		t.Fatal(err)
	}
	f.s.Run(ctx, due)
	f.s.Run(ctx, due+15_000)
	f.s.Run(ctx, due+30_000)
	if f.operations(t) != 0 || players.checks != 2 {
		t.Fatal("acted on a reading older than the latest check", f.operations(t), players.checks)
	}
	players.now.ReadAt = due + 32_000
	f.s.Run(ctx, due+45_000)
	if f.operations(t) != 1 {
		t.Fatal("did not act on the reading the latest check produced")
	}
}

// A backup schedule waits for players like a restart: with players online it
// waits, warns before the end of its wait, and backs up once the server
// empties. Like a restart, warnings need a run that goes ahead.
func TestBackupScheduleWaitsAndWarns(t *testing.T) {
	f := setup(t)
	players := f.withPlayers(2, true)
	input := base
	input.Action = "backup"
	input.Considerate(true, true)
	skipping := input
	skipping.WaitForEmpty = &WaitForEmpty{Minutes: 30, Then: WaitSkip}
	if _, err := f.s.Create(ctx, admin, f.serverID, skipping); !apperr.Is(err, "INVALID_REQUEST") {
		t.Fatal(err)
	}
	if _, err := f.s.Create(ctx, admin, f.serverID, input); err != nil {
		t.Fatal(err)
	}
	claims := func() int {
		var count int
		f.s.DB.QueryRow("SELECT COUNT(*) FROM schedule_announcements").Scan(&count)
		return count
	}
	f.s.Run(ctx, due)
	if first := f.first(t, admin); first.LastResult != WaitingResult || f.operations(t) != 0 {
		t.Fatal(first)
	}
	// One warning per server is sent at a time.
	f.s.Announce(ctx, due+25*minute)
	f.s.Wait()
	f.s.Announce(ctx, due+29*minute)
	f.s.Wait()
	if claims() != 2 {
		t.Fatal("warnings were not counted down to the end of the wait", claims())
	}
	players.now.Online = 0
	f.s.Run(ctx, due+29*minute+30_000)
	var kind string
	f.s.DB.QueryRow("SELECT kind FROM operations").Scan(&kind)
	if kind != "backup" || f.first(t, admin).LastOperation == nil {
		t.Fatal("the backup didn't run once the server emptied", kind)
	}
}

// New schedules start as considerate as the game allows, only for actions
// that take the server down, and the defaults always save.
func TestConsiderateDefaults(t *testing.T) {
	for _, action := range Actions {
		for _, warn := range []bool{false, true} {
			for _, wait := range []bool{false, true} {
				input := base
				input.Action = action
				if action == "command" {
					input.Command = "save-all"
				}
				input.Considerate(warn, wait)
				disconnects := action == "restart" || action == "stop" || action == "backup"
				if (input.Announcements != nil) != (warn && disconnects) || (input.WaitForEmpty != nil) != (wait && disconnects) {
					t.Fatal(action, warn, wait, input.Announcements, input.WaitForEmpty)
				}
				if wait && disconnects && (input.WaitForEmpty.Minutes != 30 || input.WaitForEmpty.Then != WaitRun) {
					t.Fatal(input.WaitForEmpty)
				}
				if warn && disconnects && !slices.Equal(input.Announcements.LeadMinutes, []int{5, 1}) {
					t.Fatal(input.Announcements)
				}
				if err := input.Validate(); err != nil {
					t.Fatal(action, warn, wait, err)
				}
			}
		}
	}
	for action, message := range map[string]string{"restart": "Server restart in 5 minutes.", "stop": "Server shutting down in 5 minutes.",
		"backup": "Server restart for a backup in 5 minutes.", "update": "Server restart for an update in 5 minutes."} {
		if got := WarningMessage(action, 5); got != message {
			t.Fatal(got)
		}
	}
	if got := WarningMessage("stop", 1); got != "Server shutting down in 1 minute." {
		t.Fatal(got)
	}
}

// Cancelling a run waiting for players to leave ends it without acting,
// even for a tick that read the run as waiting just before; the schedule's
// next run still happens.
func TestCancelWaitingRun(t *testing.T) {
	f := setup(t)
	players := f.withPlayers(1, true)
	if _, err := f.s.Create(ctx, admin, f.serverID, waitingRestart(30, WaitRun)); err != nil {
		t.Fatal(err)
	}
	f.s.Run(ctx, due)
	first := f.first(t, admin)
	stale, err := f.s.get(ctx, f.s.DB, first.ID, f.serverID)
	if err != nil || !waiting(stale) {
		t.Fatal(err, stale)
	}
	if err := f.s.CancelWait(ctx, admin, f.serverID, first.ID); err != nil {
		t.Fatal(err)
	}
	players.now.Online = 0
	f.s.runOne(ctx, stale, due+30*minute)
	if last := f.first(t, admin); f.operations(t) != 0 || last.WaitingUntil != 0 || !last.Enabled ||
		!strings.HasPrefix(last.LastResult, "Cancelled by ") {
		t.Fatal(last)
	}
	if err := f.s.CancelWait(ctx, admin, f.serverID, first.ID); !apperr.Is(err, "SCHEDULE_WAIT_OVER") {
		t.Fatal(err)
	}
	players.now.Online = 1
	f.s.Run(ctx, due+86_400_000)
	if next := f.first(t, admin); next.LastResult != WaitingResult || next.WaitingUntil != due+86_400_000+30*minute {
		t.Fatal("the next day's run didn't start", next)
	}
}
