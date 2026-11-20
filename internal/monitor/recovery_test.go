package monitor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
)

// recovering monitors one server with recovery on and a 60-second grace,
// whose game answers as game says.
func recovering(t *testing.T, change func(*Policy)) (*Service, *recorder, string, *GameAnswer) {
	t.Helper()
	s, events, server, _ := setupWith(t, func(p *Policy) {
		p.Recover = true
		if change != nil {
			change(p)
		}
	})
	s.Runner = &ops.Runner{DB: s.DB}
	game := new(GameAnswer)
	*game = GameNotAnswering
	s.Game = func(string, servers.Container) GameAnswer { return *game }
	return s, events, server.ID, game
}

func run(state string, since int64) map[string]servers.Container {
	return map[string]servers.Container{"mc": {ID: "mc", State: state, StateSince: since}}
}

// recoveryJobs lists the server's recovery restarts, oldest first.
func recoveryJobs(t *testing.T, s *Service, serverID string) []string {
	t.Helper()
	rows, err := s.DB.Query("SELECT id FROM operations WHERE server_id=? AND actor_id=? AND kind='restart' ORDER BY created_at,rowid",
		serverID, ops.RecoveryActor)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids
}

// finish ends a restart as its operation would, which also resets the
// outage it acted on. It ends as soon as it was queued, in the tests' time.
func finish(t *testing.T, s *Service, id, status string) {
	t.Helper()
	if _, err := s.DB.Exec("UPDATE operations SET status=?,updated_at=0 WHERE id=?", status, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE availability SET outage_started_at=NULL,notified=0"); err != nil {
		t.Fatal(err)
	}
}

// hang runs one hang from its start at `at` past its grace, and returns the
// recovery restarts queued.
func hang(t *testing.T, s *Service, serverID string, game *GameAnswer, at, since int64) []string {
	t.Helper()
	*game = GameNotAnswering
	s.Check(ctx, run("running", since), at)
	s.Check(ctx, run("running", since), at+61_000)
	return recoveryJobs(t, s, serverID)
}

// A hung game is restarted once, past its grace, announced instead of the
// outage, and its answer is reported.
func TestRecoveryRestartsAHungGameAndReportsItsAnswer(t *testing.T) {
	s, events, serverID, game := recovering(t, nil)
	if jobs := hang(t, s, serverID, game, 1000, 500); len(jobs) != 1 {
		t.Fatal(jobs)
	}
	if len(events.events) != 1 || events.events[0] != "Minecraft stopped answering, so Ludock is restarting it." {
		t.Fatal(events.events)
	}
	// The queued restart holds the server; nothing more is queued.
	s.Check(ctx, run("running", 500), 70_000)
	jobs := recoveryJobs(t, s, serverID)
	if len(jobs) != 1 {
		t.Fatal(jobs)
	}
	finish(t, s, jobs[0], ops.Succeeded)
	*game = GameWaiting
	s.Check(ctx, run("running", 65_000), 80_000)
	*game = GameAnswering
	s.Check(ctx, run("running", 65_000), 120_000)
	state, _ := s.Recovery(ctx, serverID)
	if state.Last != RecoveryAnswered || state.Pending || state.RestartedAt != 62_000 {
		t.Fatalf("%+v", state)
	}
	if len(events.events) != 2 || events.events[1] != "Minecraft is answering again after Ludock restarted it." {
		t.Fatal(events.events)
	}
}

// Recovery never restarts a server it can't see running with a game that
// answered in this run and has since stopped, or that it may not touch now.
func TestRecoveryNeverActs(t *testing.T) {
	for name, scenario := range map[string]struct {
		policy  func(*Policy)
		game    GameAnswer
		noQuery bool
		current func() map[string]servers.Container
		before  func(s *Service, serverID string)
	}{
		"switched off":          {policy: func(p *Policy) { p.Recover = false }},
		"paused":                {policy: func(p *Policy) { p.Maintenance = true }},
		"not monitored":         {policy: func(p *Policy) { p.Enabled = false }},
		"not answered yet":      {game: GameWaiting},
		"no recent reading":     {game: GameUnknown},
		"no status query":       {noQuery: true},
		"exited by itself":      {current: func() map[string]servers.Container { return run("exited", 500) }},
		"restarting":            {current: func() map[string]servers.Container { return run("restarting", 500) }},
		"Docker unreachable":    {current: func() map[string]servers.Container { return nil }},
		"health check starting": {current: func() map[string]servers.Container { return state("running", "starting") }},
		"stopped through Ludock": {current: func() map[string]servers.Container { return run("exited", 500) },
			before: func(s *Service, serverID string) { s.SetIntentionalStop(ctx, serverID, true) }},
		"another operation": {before: func(s *Service, serverID string) {
			if _, err := s.Runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: serverID, ActorID: "admin", Kind: "backup"}); err != nil {
				t.Fatal(err)
			}
		}},
		"awaiting review": {before: func(s *Service, serverID string) {
			if _, err := s.DB.Exec("UPDATE logical_servers SET review_required=1 WHERE id=?", serverID); err != nil {
				t.Fatal(err)
			}
		}},
		"server busy": {before: func(s *Service, serverID string) {
			if _, err := s.Locks.Acquire([]string{"server:" + serverID}, nil); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, serverID, game := recovering(t, scenario.policy)
			if scenario.game != GameNotChecked {
				*game = scenario.game
			}
			if scenario.noQuery {
				s.Game = nil
			}
			if scenario.before != nil {
				scenario.before(s, serverID)
			}
			current := run("running", 500)
			if scenario.current != nil {
				current = scenario.current()
			}
			for _, at := range []int64{1000, 62_000, 200_000, 2 * time.Hour.Milliseconds()} {
				s.Check(ctx, current, at)
			}
			if jobs := recoveryJobs(t, s, serverID); len(jobs) != 0 {
				t.Fatal(jobs)
			}
		})
	}
}

// After three restarts in six hours recovery gives up, says so once, and
// still counts after Ludock restarts. A restart is never followed by another
// within the pause, even when the game hangs again at once.
func TestRecoveryAttemptWindowSurvivesARestart(t *testing.T) {
	s, events, serverID, game := recovering(t, nil)
	wait := RecoveryWait(Policy{GraceSeconds: 60})
	at := int64(1000)
	for attempt := 1; attempt <= RecoveryAttempts; attempt++ {
		jobs := hang(t, s, serverID, game, at, at-500)
		if len(jobs) != attempt {
			t.Fatal(attempt, jobs)
		}
		finish(t, s, jobs[attempt-1], ops.Succeeded)
		restarted := at + 61_000
		*game = GameAnswering
		s.Check(ctx, run("running", restarted+1), restarted+wait/2)
		if attempt == 1 {
			// Hung again before the pause ends: alerted, not restarted.
			hang(t, s, serverID, game, restarted+wait/2+1000, restarted+1)
			if jobs := recoveryJobs(t, s, serverID); len(jobs) != 1 {
				t.Fatal(jobs)
			}
			*game = GameAnswering
			s.Check(ctx, run("running", restarted+1), restarted+wait-1000)
		}
		at = restarted + wait
		// Ludock restarts: a new service on the same database.
		s = &Service{DB: s.DB, Locks: s.Locks, Runner: s.Runner, Notify: s.Notify, Store: s.Store, Game: s.Game}
	}
	*game = GameNotAnswering
	before := len(events.events)
	hang(t, s, serverID, game, at, at-500)
	s.Check(ctx, run("running", at-500), at+120_000)
	if jobs := recoveryJobs(t, s, serverID); len(jobs) != RecoveryAttempts {
		t.Fatal(jobs)
	}
	gaveUp := events.events[before:]
	if len(gaveUp) != 1 || !strings.Contains(gaveUp[0], "won’t restart it again") {
		t.Fatal(gaveUp)
	}
	if state, _ := s.Recovery(ctx, serverID); state.Last != RecoveryGaveUp {
		t.Fatalf("%+v", state)
	}
	// Once the oldest restart leaves the window, the next hang is restarted.
	later := 1000 + RecoveryWindow.Milliseconds()
	*game = GameAnswering
	s.Check(ctx, run("running", later-1000), later)
	if jobs := hang(t, s, serverID, game, later+1000, later-1000); len(jobs) != RecoveryAttempts+1 {
		t.Fatal(jobs)
	}
}

// A game that never answers after its restart is reported once and never
// restarted again: it can't stop answering without having answered.
func TestRecoveryReportsAGameThatDoesntAnswerAfterItsRestart(t *testing.T) {
	s, events, serverID, game := recovering(t, nil)
	jobs := hang(t, s, serverID, game, 1000, 500)
	finish(t, s, jobs[0], ops.Succeeded)
	*game = GameWaiting
	wait := RecoveryWait(Policy{GraceSeconds: 60})
	for at := int64(70_000); at < 62_000+3*wait; at += 30_000 {
		s.Check(ctx, run("running", 65_000), at)
	}
	if jobs := recoveryJobs(t, s, serverID); len(jobs) != 1 {
		t.Fatal(jobs)
	}
	if len(events.events) != 2 || !strings.Contains(events.events[1], "still isn’t answering after Ludock restarted it") {
		t.Fatal(events.events)
	}
	if state, _ := s.Recovery(ctx, serverID); state.Last != RecoveryNotAnswered {
		t.Fatalf("%+v", state)
	}
	// It is still followed, so a late answer is reported.
	*game = GameAnswering
	s.Check(ctx, run("running", 65_000), 62_000+4*wait)
	if state, _ := s.Recovery(ctx, serverID); state.Last != RecoveryAnswered || state.Pending || len(events.events) != 3 {
		t.Fatalf("%+v %v", state, events.events)
	}
}

// The wait is timed from the game's own silence, not from an outage with
// another cause, such as Docker being unreachable.
func TestRecoveryTimesTheGamesOwnSilence(t *testing.T) {
	s, _, serverID, game := recovering(t, nil)
	s.Check(ctx, nil, 1000)
	s.Check(ctx, nil, 200_000)
	*game = GameNotAnswering
	s.Check(ctx, run("running", 500), 210_000)
	s.Check(ctx, run("running", 500), 240_000)
	if jobs := recoveryJobs(t, s, serverID); len(jobs) != 0 {
		t.Fatal("restarted before the game was silent for its wait", jobs)
	}
	s.Check(ctx, run("running", 500), 271_000)
	if jobs := recoveryJobs(t, s, serverID); len(jobs) != 1 {
		t.Fatal(jobs)
	}
}

// Recovery that gave up on an outage leaves it to a person even once the
// window has room again.
func TestRecoveryGivesUpOnAnOutageForGood(t *testing.T) {
	s, _, serverID, game := recovering(t, nil)
	var attempts []int64
	for index := range RecoveryAttempts {
		attempts = append(attempts, int64(index+1)*60_000)
	}
	state, _ := json.Marshal(Recovery{Attempts: attempts, RestartedAt: attempts[len(attempts)-1], Last: RecoveryAnswered})
	if _, err := s.DB.Exec("INSERT INTO settings(key,value_json) VALUES(?,?)", recoveryKey(serverID), string(state)); err != nil {
		t.Fatal(err)
	}
	hang(t, s, serverID, game, time.Hour.Milliseconds(), 500)
	if got, _ := s.Recovery(ctx, serverID); got.Last != RecoveryGaveUp {
		t.Fatalf("%+v", got)
	}
	// An unrelated settings change restarts the outage, not the silence.
	policy := DefaultPolicy()
	policy.GraceSeconds, policy.Recover, policy.DiskPercent = 60, true, 80
	if _, err := s.Configure(ctx, "admin", "test-session-admin", serverID, policy); err != nil {
		t.Fatal(err)
	}
	for _, at := range []int64{7 * time.Hour.Milliseconds(), 7*time.Hour.Milliseconds() + 61_000, 8 * time.Hour.Milliseconds()} {
		s.Check(ctx, run("running", 500), at)
	}
	if jobs := recoveryJobs(t, s, serverID); len(jobs) != 0 {
		t.Fatal(jobs)
	}
}

// A restart that failed is reported; one skipped because the game answered
// or the server stopped first is not.
func TestRecoveryReportsAFailedRestart(t *testing.T) {
	for status, want := range map[string]string{ops.Failed: RecoveryFailed, ops.Skipped: RecoverySkipped} {
		t.Run(status, func(t *testing.T) {
			s, events, serverID, game := recovering(t, nil)
			jobs := hang(t, s, serverID, game, 1000, 500)
			finish(t, s, jobs[0], status)
			s.Check(ctx, run("running", 500), 70_000)
			// A skipped restart never acted, so it isn't counted.
			if state, _ := s.Recovery(ctx, serverID); state.Last != want || (len(state.Attempts) == 0) != (status == ops.Skipped) {
				t.Fatalf("%+v", state)
			}
			if failed := len(events.events) == 2 && strings.HasPrefix(events.events[1], "Ludock couldn’t restart Minecraft"); failed != (status == ops.Failed) {
				t.Fatal(events.events)
			}
		})
	}
}

// Turning recovery off forgets a pending outcome, and SetRecovery keeps the
// rest of the policy.
func TestSetRecoveryKeepsThePolicy(t *testing.T) {
	s, _, serverID, game := recovering(t, func(p *Policy) { p.DiskPercent = 70 })
	hang(t, s, serverID, game, 1000, 500)
	session := "test-session-admin"
	if err := s.SetRecovery(ctx, "admin", session, serverID, false); err != nil {
		t.Fatal(err)
	}
	availability, _ := s.Get(ctx, serverID)
	state, _ := s.Recovery(ctx, serverID)
	if availability.Policy.Recover || availability.Policy.DiskPercent != 70 || state.Pending || availability.State.OutageStartedAt == 0 {
		t.Fatalf("%+v %+v", availability, state)
	}
	if err := s.SetRecovery(ctx, "admin", "stale", serverID, true); err == nil {
		t.Fatal("saved without a valid session")
	}
}

// An answer between two checks restarts the silence clock.
func TestRecoveryAnswerBetweenChecksResetsTheSilence(t *testing.T) {
	s, _, serverID, game := recovering(t, nil)
	answered := int64(0)
	s.LastAnswer = func(string, servers.Container) int64 { return answered }
	s.Check(ctx, run("running", 500), 1000)
	answered = 30_000
	s.Check(ctx, run("running", 500), 62_000)
	if jobs := recoveryJobs(t, s, serverID); len(jobs) != 0 {
		t.Fatal("restarted a game that answered within its wait", jobs)
	}
	*game = GameNotAnswering
	s.Check(ctx, run("running", 500), 123_000)
	if jobs := recoveryJobs(t, s, serverID); len(jobs) != 1 {
		t.Fatal(jobs)
	}
}

// The pause and the game's time to answer run from when the restart ended,
// and nothing about the restart is decided while Docker is unreachable.
func TestRecoveryWaitsFromTheRestartsEnd(t *testing.T) {
	s, events, serverID, game := recovering(t, nil)
	jobs := hang(t, s, serverID, game, 1000, 500)
	wait := RecoveryWait(Policy{GraceSeconds: 60})
	ended := 62_000 + 2*wait
	if _, err := s.DB.Exec("UPDATE operations SET status=?,updated_at=? WHERE id=?", ops.Succeeded, ended, jobs[0]); err != nil {
		t.Fatal(err)
	}
	*game = GameWaiting
	s.Check(ctx, nil, ended+wait+1000)
	s.Check(ctx, run("running", ended), ended+wait/2)
	if state, _ := s.Recovery(ctx, serverID); !state.Pending || state.Last != RecoveryRestarted || len(events.events) != 1 {
		t.Fatalf("%+v %v", state, events.events)
	}
	s.Check(ctx, run("running", ended), ended+wait+1000)
	if state, _ := s.Recovery(ctx, serverID); state.Last != RecoveryNotAnswered {
		t.Fatalf("%+v", state)
	}
}

// A restart interrupted before the server ran again couldn't restart it, and
// a game reported not answering is followed only in the run Ludock started.
func TestRecoveryFollowsOnlyItsOwnRun(t *testing.T) {
	s, events, serverID, game := recovering(t, nil)
	jobs := hang(t, s, serverID, game, 1000, 500)
	finish(t, s, jobs[0], ops.Interrupted)
	*game = GameAnswering
	s.Check(ctx, run("running", 500), 70_000)
	if state, _ := s.Recovery(ctx, serverID); state.Last != RecoveryFailed || len(events.events) != 2 {
		t.Fatalf("%+v %v", state, events.events)
	}

	s, events, serverID, game = recovering(t, nil)
	jobs = hang(t, s, serverID, game, 1000, 500)
	finish(t, s, jobs[0], ops.Succeeded)
	*game = GameWaiting
	wait := RecoveryWait(Policy{GraceSeconds: 60})
	s.Check(ctx, run("running", 65_000), 62_000+wait+1000)
	// Someone else restarts it, and it answers.
	*game = GameAnswering
	s.Check(ctx, run("running", 62_000+2*wait), 62_000+3*wait)
	if state, _ := s.Recovery(ctx, serverID); state.Last != RecoveryNotAnswered || state.Pending || len(events.events) != 2 {
		t.Fatalf("%+v %v", state, events.events)
	}
}

// Recovery turned off and on again during a restart still pauses from that
// restart's end.
func TestRecoveryPausesFromARestartTurnedOffMidway(t *testing.T) {
	s, _, serverID, game := recovering(t, nil)
	jobs := hang(t, s, serverID, game, 1000, 500)
	for _, on := range []bool{false, true} {
		if err := s.SetRecovery(ctx, "admin", "test-session-admin", serverID, on); err != nil {
			t.Fatal(err)
		}
	}
	wait := RecoveryWait(Policy{GraceSeconds: 60})
	ended := 62_000 + 3*wait
	if _, err := s.DB.Exec("UPDATE operations SET status=?,updated_at=? WHERE id=?", ops.Succeeded, ended, jobs[0]); err != nil {
		t.Fatal(err)
	}
	*game = GameAnswering
	s.Check(ctx, run("running", ended), ended+1000)
	if jobs := hang(t, s, serverID, game, ended+2000, ended); len(jobs) != 1 {
		t.Fatal("restarted again within the pause", jobs)
	}
	if jobs := hang(t, s, serverID, game, ended+wait, ended); len(jobs) != 2 {
		t.Fatal(jobs)
	}
}
