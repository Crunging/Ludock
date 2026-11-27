package monitor

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
)

var ctx = context.Background()

type recorder struct{ events []string }

func (r *recorder) queue(_ context.Context, _ *sql.Tx, _ notify.Alert, key, message string) (bool, error) {
	r.events = append(r.events, message)
	return true, nil
}

func setup(t *testing.T) (*Service, *recorder, identity.Server) {
	t.Helper()
	s, events, server, _ := setupWith(t, nil)
	return s, events, server
}

// setupWith monitors one server with the default policy and a 60-second
// grace period, changed by change. configure saves another policy.
func setupWith(t *testing.T, change func(*Policy)) (s *Service, events *recorder, server identity.Server, configure func(Policy)) {
	t.Helper()
	d := dbtest.Open(t)
	store := &identity.Store{DB: d}
	list, _ := store.Reconcile(ctx, []identity.Observation{{ContainerID: "mc", Name: "mc", DisplayName: "Minecraft", GameType: "minecraft"}}, "", 0)
	events = &recorder{}
	s = &Service{DB: d, Locks: locks.New(), Queue: events.queue, Store: store}
	if err := d.CreateUser(ctx, db.UserRecord{User: db.User{ID: "admin", Username: "admin", Role: db.RoleAdmin, CreatedAt: 1}, PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	session := dbtest.Session(t, d, "admin")
	configure = func(policy Policy) {
		t.Helper()
		if _, err := s.Configure(ctx, "admin", session, list[0].ID, policy); err != nil {
			t.Fatal(err)
		}
	}
	policy := DefaultPolicy()
	policy.Enabled, policy.GraceSeconds = true, 60
	if change != nil {
		change(&policy)
	}
	configure(policy)
	return s, events, list[0], configure
}

func state(value, health string) map[string]servers.Container {
	return map[string]servers.Container{"mc": {ID: "mc", State: value, Health: health}}
}

// An outage shows on pages once its grace ends, before the next tick alerts.
// A server that recovers in between raises no alert, but pages still have to
// hear that the outage is gone, and that the time it began was learned.
func TestClearedOutageUpdatesPagesWithoutAnAlert(t *testing.T) {
	s, events, server := setup(t)
	var changes []string
	s.Changed = func(serverID string) { changes = append(changes, serverID) }
	s.Check(ctx, state("exited", ""), 1000)
	if found, _ := problem(s, server.ID, 61_000); found == nil {
		t.Fatal("no problem after grace")
	}
	s.Check(ctx, state("running", ""), 62_000)
	if found, _ := problem(s, server.ID, 62_000); found != nil {
		t.Fatal("problem after recovery")
	}
	if len(events.events) != 0 || len(changes) != 1 || changes[0] != server.ID {
		t.Fatal(events.events, changes)
	}
	// An alerted outage whose stop time Docker reports only later, as after
	// Ludock restarts, tells pages so.
	s.Check(ctx, state("exited", ""), 100_000)
	s.Check(ctx, state("exited", ""), 161_000)
	before := len(changes)
	s.Check(ctx, map[string]servers.Container{"mc": {ID: "mc", State: "exited", DownSince: 90_000}}, 170_000)
	if found, _ := problem(s, server.ID, 170_000); found == nil || found.Since != 90_000 || len(changes) != before+1 {
		t.Fatal(found, changes)
	}
}

// An outage alerts once after its grace and once when it ends. A policy saved
// during it that keeps the server watched keeps it, timed from its start;
// turning watching off, or pausing it for maintenance, ends it, so it is
// alerted again once watched. A server that stopped long before Ludock first
// looked shows as down since Docker's time for its stop, but waits and
// alerts from when Ludock looked. Each alert is also logged for Activity,
// with when the outage began or how long it lasted.
func TestOneOutageAfterGraceAndOneRecovery(t *testing.T) {
	const down, back = "Minecraft: the server is down.", "Minecraft: the server is running again."
	for _, c := range []struct {
		name string
		// saved are policies saved after the check at savedAt. Nothing shows
		// or alerts through the check at quiet, 30 s if unset.
		saved          []Policy
		savedAt, quiet int64
		want           []string
		start          int64
		// stopped is when Docker says the container stopped, 0 for unknown;
		// restartAt, the check that a new Ludock process makes first.
		stopped, restartAt int64
	}{
		{name: "no change", want: []string{down, back}, start: 1000},
		{name: "stopped long before Ludock looked", stopped: 100, want: []string{down, back}, start: 1000},
		{name: "stopped long before, alerted after Ludock restarts", stopped: 100, restartAt: 61_000, want: []string{down, back}, start: 1000},
		{name: "disk threshold after the alert", saved: []Policy{testPolicy(func(p *Policy) { p.DiskPercent = 80 })}, savedAt: 61_000,
			want: []string{down, back}, start: 1000},
		{name: "longer grace after the alert", saved: []Policy{testPolicy(func(p *Policy) { p.GraceSeconds = 600 })}, savedAt: 61_000,
			want: []string{down, back}, start: 1000},
		{name: "longer grace before the alert", saved: []Policy{testPolicy(func(p *Policy) { p.GraceSeconds = 120 })}, savedAt: 30_000,
			quiet: 62_000, want: []string{down, back}, start: 1000},
		{name: "watching off and on", saved: []Policy{testPolicy(func(p *Policy) { p.Enabled = false }), testPolicy(nil)}, savedAt: 61_000,
			want: []string{down, down, back}, start: 62_000},
		{name: "maintenance and back", saved: []Policy{testPolicy(func(p *Policy) { p.Maintenance = true }), testPolicy(nil)}, savedAt: 61_000,
			want: []string{down, down, back}, start: 62_000},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, events, server, configure := setupWith(t, nil)
			exited := map[string]servers.Container{"mc": {ID: "mc", State: "exited", DownSince: c.stopped}}
			for _, at := range []int64{1000, 30_000, 61_000, 62_000, 130_000} {
				if at == c.restartAt {
					s = &Service{DB: s.DB, Locks: s.Locks, Queue: s.Queue, Store: s.Store}
				}
				s.Check(ctx, exited, at)
				if found, _ := problem(s, server.ID, at); at <= cmp.Or(c.quiet, 30_000) && (found != nil || len(events.events) != 0) {
					t.Fatal("outage before grace:", found, events.events)
				}
				if at == c.savedAt {
					for _, policy := range c.saved {
						configure(policy)
					}
				}
			}
			if found, _ := problem(s, server.ID, 130_000); found == nil || found.State != "exited" || found.OutageStartedAt != c.start ||
				found.Since != cmp.Or(c.stopped, c.start) {
				t.Fatal(found)
			}
			if shown, _ := s.Get(ctx, server.ID); shown.State.Since != cmp.Or(c.stopped, c.start) {
				t.Fatal("page shows down since", shown.State.Since)
			}
			s.Check(ctx, state("running", "starting"), 135_000)
			s.Check(ctx, state("running", ""), 140_000)
			logged := audited(s, server.ID)
			if since := cmp.Or(c.stopped, c.start); len(logged) != len(c.want) || logged[len(logged)-2] != fmt.Sprintf(`server.outage.alerted {"since":%d}`, since) ||
				logged[len(logged)-1] != fmt.Sprintf(`server.outage.ended {"downFor":%d}`, 140_000-since) {
				t.Fatal(logged)
			}
			s.Check(ctx, state("running", ""), 150_000)
			if !slices.Equal(events.events, c.want) {
				t.Fatal(events.events)
			}
		})
	}
}

// Intentional stops and operations suppress.
func TestMonitorSuppression(t *testing.T) {
	s, events, server := setup(t)
	s.SetIntentionalStop(ctx, server.ID, true)
	s.Check(ctx, state("exited", ""), 1000)
	s.Check(ctx, state("exited", ""), 200_000)
	if len(events.events) != 0 {
		t.Fatal(events.events)
	}
	// Observed running again: monitoring resumes.
	s.Check(ctx, state("running", ""), 300_000)
	s.Check(ctx, state("exited", ""), 400_000)
	release, _ := s.Locks.Acquire([]string{locks.ServerKey(server.ID)}, nil)
	s.Check(ctx, state("exited", ""), 500_000)
	release()
	if len(events.events) != 0 {
		t.Fatal("notified while an action held the server")
	}
	s.Check(ctx, state("exited", ""), 500_000)
	if len(events.events) != 1 {
		t.Fatal(events.events)
	}
}

// Ludock acting on a server pauses its outage tracking. An outage already
// alerted still ends with one recovery alert, sent as soon as the server is
// back even within the pause, and never alerts again, unless an automatic
// restart, which announces its own outcome, ends it. A stop through Ludock
// leaves it owed that recovery alert, sent once the server runs and answers
// again; a stop before the alert ends the outage silently. Work queued
// while a check runs holds that alert. Once the pause ends, the outage shows,
// and pages hear of it, only after a check has looked at the server again,
// not one that skipped it as busy, or a minute later if none has.
func TestMonitorSuppressionKeepsAnAlertedOutage(t *testing.T) {
	const down, silent, back = "Minecraft: the server is down.", "Minecraft: running, but the game stopped answering.", "Minecraft: the server is running again."
	const answering = "Minecraft: the game is answering again."
	start := func(s *Service, id string) { s.Suppress(ctx, id, nil); s.SetIntentionalStop(ctx, id, false) }
	restart := func(s *Service, id string) {
		s.Suppress(ctx, id, nil)
		s.SetIntentionalStop(ctx, id, false)
		s.SetIntentionalStop(ctx, id, false)
		s.Suppress(ctx, id, nil)
	}
	stop := func(s *Service, id string) {
		s.Suppress(ctx, id, nil)
		s.SetIntentionalStop(ctx, id, true)
		s.Suppress(ctx, id, nil)
	}
	for _, c := range []struct {
		name            string
		silent, alerted bool
		act             func(*Service, string)
		// after is the container, its health check and game within the pause,
		// once stopped, and after it, checked twice; answersBack: a game that
		// then answers ends it.
		after, health string
		game          GameAnswer
		want          []string
		stillDown     bool
		answersBack   bool
		// startsBack: the stopped server is then started and answers.
		startsBack bool
		// late: the server stays stopped within the pause. queued: other work
		// is queued while the first check that can end the outage runs, within
		// the pause or, if late, after it.
		late, queued bool
	}{
		{name: "started", alerted: true, act: start, after: "running", health: "healthy", want: []string{down, back}},
		{name: "started but unhealthy", alerted: true, act: start, after: "running", health: "unhealthy", want: []string{down}, stillDown: true},
		{name: "restarted", silent: true, alerted: true, act: restart, after: "running", game: GameWaiting, want: []string{silent},
			stillDown: true, answersBack: true},
		{name: "restarted and answering", silent: true, alerted: true, act: restart, after: "running", game: GameAnswering, want: []string{silent, answering}},
		{name: "left stopped by other work", alerted: true, act: func(s *Service, id string) { s.Suppress(ctx, id, nil); s.Suppress(ctx, id, nil) },
			after: "exited", want: []string{down}, stillDown: true},
		{name: "stopped", silent: true, alerted: true, after: "exited", want: []string{silent}, startsBack: true, act: stop},
		{name: "stopped within grace", silent: true, after: "exited", startsBack: true, act: stop},
		{name: "started within grace", act: start, after: "running"},
		{name: "started after the pause as other work is queued", alerted: true, act: start, after: "running", health: "healthy", want: []string{down},
			late: true, queued: true},
		{name: "started as other work is queued", alerted: true, act: start, after: "running", health: "healthy", want: []string{down}, queued: true},
		{name: "restarted automatically", silent: true, alerted: true, after: "running", game: GameAnswering, want: []string{silent},
			act: func(s *Service, id string) {
				s.Suppress(ctx, id, nil)
				s.SetIntentionalStop(ctx, id, false)
				s.SetIntentionalStop(ctx, id, false)
				s.SuppressRecovered(ctx, id, nil)
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, events, server := setup(t)
			game := GameAnswering
			s.Game = func(string, servers.Container) GameAnswer { return game }
			now := db.Now()
			before := state("exited", "")
			if c.silent {
				s.Check(ctx, state("running", ""), now-300_000)
				game, before = GameNotAnswering, state("running", "")
			}
			s.Check(ctx, before, now-200_000)
			if c.alerted {
				s.Check(ctx, before, now-100_000)
			}
			c.act(s, server.ID)
			game = c.game
			s.Check(ctx, state("exited", ""), now+5_000)
			queue := func() {
				hookAt(t, "active", func() {
					if _, err := (&ops.Runner{DB: s.DB}).Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: server.ID, ActorID: "admin", Kind: "backup"}); err != nil {
						t.Fatal(err)
					}
				})
			}
			within := state(c.after, c.health)
			if c.late {
				within = state("exited", "")
			} else if c.queued {
				queue()
			}
			s.Check(ctx, within, now+10_000)
			if !slices.Equal(events.events, c.want) {
				t.Fatal("within the pause:", events.events)
			}
			// A page reads during the pause, its read done 5 s after it began;
			// nothing reads or checks again until the outage shows, a minute
			// after the pause, which pages hear of then.
			var timers []func()
			afterFunc = func(d time.Duration, f func()) {
				if d < 94*time.Second || d > 96*time.Second {
					t.Error("pages told at the wrong time:", d)
				}
				timers = append(timers, f)
			}
			clock := db.Now
			db.Now = func() int64 { return now + 25_000 }
			t.Cleanup(func() { afterFunc, db.Now = func(d time.Duration, f func()) { time.AfterFunc(d, f) }, clock })
			changes := 0
			s.Changed = func(string) { changes++ }
			problem(s, server.ID, now+20_000)
			kept, _ := s.Get(ctx, server.ID)
			if (len(timers) == 1) != kept.State.Notified || len(timers) > 1 {
				t.Fatal("pages told", len(timers), "times")
			}
			// Other work holds the server through the first check after the
			// pause, which skips it.
			release, _ := s.Locks.Acquire([]string{locks.ServerKey(server.ID)}, nil)
			s.Check(ctx, state(c.after, c.health), now+80_000)
			release()
			for _, at := range []int64{now + 90_000, now + 91_000} {
				if found, _ := problem(s, server.ID, at); found != nil {
					t.Fatal("outage shown before a check after the pause:", found)
				}
			}
			db.Now = clock
			if len(timers) > 1 {
				t.Fatal("pages told", len(timers), "times")
			}
			for _, f := range timers {
				f()
			}
			if changes != len(timers) {
				t.Fatal("pages weren't told when the outage shows again")
			}
			changes = 0
			if found, _ := problem(s, server.ID, now+130_000); (found != nil) != (c.stillDown || c.late) {
				t.Fatal("outage shown with no check:", found)
			}
			hookAt(t, "read", func() {
				if found, _ := problem(s, server.ID, now+90_000); found != nil {
					t.Error("outage shown while the check after the pause runs:", found)
				}
			})
			if c.late && c.queued {
				queue()
			}
			for _, at := range []int64{now + 90_000, now + 100_000, now + 200_000} {
				s.Check(ctx, state(c.after, c.health), at)
				if found, _ := problem(s, server.ID, at); (found != nil) != c.stillDown {
					t.Fatal("outage shown:", found)
				}
				if c.stillDown && changes == 0 {
					t.Fatal("pages weren't told the outage shows again")
				}
			}
			if c.answersBack {
				game = GameAnswering
				s.Check(ctx, state(c.after, ""), now+210_000)
				s.Check(ctx, state(c.after, ""), now+220_000)
				c.want = append(c.want, answering)
			}
			if c.startsBack {
				game = GameAnswering
				s.Check(ctx, state("running", ""), now+210_000)
				s.Check(ctx, state("running", ""), now+220_000)
				if c.alerted {
					c.want = append(c.want, answering)
				}
			}
			if !slices.Equal(events.events, c.want) {
				t.Fatal(events.events)
			}
		})
	}
}

// Docker outage is reported without rebinding. Docker answering again with
// the server running ends it as just that: Ludock reaches Docker again and
// the server runs, which it may have done all along.
func TestDockerOutage(t *testing.T) {
	s, events, server := setup(t)
	s.Check(ctx, nil, 1000)
	s.Check(ctx, nil, 70_000)
	if len(events.events) != 1 || events.events[0] != "Minecraft: Ludock can’t reach Docker to check it." {
		t.Fatal(events.events)
	}
	availability, _ := s.Get(ctx, server.ID)
	if availability.State.LastState != "docker_unavailable" {
		t.Fatal(availability.State)
	}
	current, _ := s.Store.Get(ctx, server.ID)
	if current.Status != identity.StatusActive {
		t.Fatal("binding changed")
	}
	s.Check(ctx, state("running", ""), 90_000)
	if len(events.events) != 2 || events.events[1] != "Minecraft: Ludock can reach Docker again, and the server is running." {
		t.Fatal(events.events)
	}
	logged := audited(s, server.ID)
	if !slices.Equal(logged, []string{`server.unreachable.alerted {"since":1000}`, `server.unreachable.ended {"downFor":89000}`}) {
		t.Fatal(logged)
	}
}

func TestPolicyParsing(t *testing.T) {
	if policy, err := ParsePolicy([]byte(`{"enabled":true}`)); err != nil || policy.GraceSeconds != 120 {
		t.Fatal(policy, err)
	}
	for _, raw := range []string{`{}`, `{"enabled":true,"graceSeconds":5}`, `{"enabled":true,"extra":1}`, `{"enabled":true,"graceSeconds":10.5}`} {
		if _, err := ParsePolicy([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestNoAlertWithoutSavedState(t *testing.T) {
	s, events, _ := setup(t)
	s.Check(ctx, state("exited", ""), 1000)
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_availability BEFORE UPDATE ON availability
		BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	s.Check(ctx, state("exited", ""), 61_000)
	if len(events.events) != 0 {
		t.Fatal("alerted before its state was saved:", events.events)
	}
	s.DB.Exec("DROP TRIGGER fail_availability")
	s.Check(ctx, state("exited", ""), 62_000)
	s.Check(ctx, state("exited", ""), 63_000)
	if len(events.events) != 1 || events.events[0] != "Minecraft: the server is down." {
		t.Fatal(events.events)
	}
}

// Unreadable operations do not raise an outage.
func TestUnreadableOperations(t *testing.T) {
	s, events, server := setup(t)
	s.Check(ctx, state("exited", ""), 1000)
	if _, err := s.DB.Exec("ALTER TABLE operations RENAME TO operations_unreadable"); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(ctx, state("exited", ""), 200_000); err == nil || len(events.events) != 0 {
		t.Fatal(err, events.events)
	}
	if _, err := problem(s, server.ID, 200_000); err == nil {
		t.Fatal("reported an outage without reading active operations")
	}
}

func TestPolicySavedDuringACheckIsKept(t *testing.T) {
	s, _, server := setup(t)
	read, _ := s.row(ctx, server.ID)
	// An administrator disables monitoring after the check read the row.
	if _, err := s.DB.Exec(`UPDATE availability SET policy_json='{"enabled":false,"graceSeconds":60}' WHERE server_id=?`, server.ID); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.transition(ctx, server.ID, read, nil, "outage_started_at=?,notified=1", 1000); err != nil || changed {
		t.Fatal(changed, err)
	}
	if found, _ := problem(s, server.ID, 200_000); found != nil {
		t.Fatal("restored an outage under the old policy")
	}
}

func TestAStopRecordedDuringACheckIsKept(t *testing.T) {
	s, events, server := setup(t)
	s.SetIntentionalStop(ctx, server.ID, true)
	// The server was started outside Ludock; a Ludock stop finishes after
	// the check read the row but before it saved the start it observed.
	testHook = func(string) {
		testHook = nil
		s.Suppress(ctx, server.ID, nil)
		s.SetIntentionalStop(ctx, server.ID, true)
	}
	t.Cleanup(func() { testHook = nil })
	s.Check(ctx, state("running", ""), db.Now())
	if availability, _ := s.Get(ctx, server.ID); !availability.State.IntentionallyStopped {
		t.Fatal("cleared a stop recorded during the check")
	}
	later := db.Now() + 1_000_000
	s.Check(ctx, state("exited", ""), later)
	s.Check(ctx, state("exited", ""), later+200_000)
	if len(events.events) != 0 {
		t.Fatal(events.events)
	}
}

// problem reads one server's outage from Problems.
func problem(s *Service, serverID string, now int64) (*Problem, error) {
	problems, err := s.Problems(ctx, now)
	return problems[serverID], err
}

// A game that keeps exiting under its restart policy shows restarting, with
// runs too short for anyone to join between restarts. That is one outage,
// timed from the first check that sees it, even on a server stopped through
// Ludock before, though shown as down since the loop's first exit discovery
// saw, and alerted once its grace ends even during such a run: a
// run counts as the server back only once the game answers or it has stayed
// up for the grace period, or an answer recorded in it. A stop through Ludock
// after the alert keeps the outage owed its recovery alert, sent once the
// server runs again and has shown it is back the same way, even started by
// hand.
func TestCrashLoopIsOneOutage(t *testing.T) {
	const down, back = "Minecraft: the server is down.", "Minecraft: the server is running again."
	for _, stoppedFirst := range []bool{false, true} {
		s, events, server := setup(t)
		game := GameWaiting
		s.Game = func(string, servers.Container) GameAnswer { return game }
		// Discovery reports the loop's first exit it saw, at 900, for its
		// restarts and its runs alike.
		runSince := func(started int64) map[string]servers.Container {
			return map[string]servers.Container{"mc": {ID: "mc", State: "running", StateSince: started, RestartCount: 3, DownSince: 900}}
		}
		restarting := map[string]servers.Container{"mc": {ID: "mc", State: "restarting", DownSince: 900}}
		if stoppedFirst {
			s.SetIntentionalStop(ctx, server.ID, true)
			s.Check(ctx, state("exited", ""), 500)
		}
		s.Check(ctx, restarting, 1000)
		s.Check(ctx, runSince(58_000), 61_000)
		if !slices.Equal(events.events, []string{down}) {
			t.Fatal(stoppedFirst, events.events)
		}
		s.Check(ctx, map[string]servers.Container{"mc": {ID: "mc", State: "running", Health: "starting", StateSince: 78_000, RestartCount: 4}}, 80_000)
		s.Check(ctx, runSince(78_000), 85_000)
		s.Check(ctx, nil, 87_000)
		if found, _ := problem(s, server.ID, 87_000); found == nil || found.State != "restarting" {
			t.Fatal("Docker unreachable during a crash loop:", found)
		}
		s.Check(ctx, runSince(78_000), 88_000)
		s.Check(ctx, restarting, 90_000)
		s.Check(ctx, restarting, 151_000)
		if found, _ := problem(s, server.ID, 151_000); found == nil || found.State != "restarting" || found.OutageStartedAt != 1000 ||
			found.Since != 900 {
			t.Fatal(found)
		}
		s.Check(ctx, runSince(155_000), 160_000)
		// An answer recorded in this run, though no recent query says.
		game = GameUnknown
		s.LastAnswer = func(string, servers.Container) int64 { return 162_000 }
		s.Check(ctx, runSince(155_000), 165_000)
		// The same run, no longer the loop's, later fails its health check:
		// that outage began when Ludock saw it.
		unhealthy := runSince(155_000)
		unhealthy["mc"] = servers.Container{ID: "mc", State: "running", Health: "unhealthy", StateSince: 155_000, RestartCount: 3, DownSince: 900}
		s.Check(ctx, unhealthy, 170_000)
		if shown, _ := s.Get(ctx, server.ID); shown.State.Since != 170_000 {
			t.Fatal("an outage after the loop shown since", shown.State.Since)
		}
		s.Check(ctx, runSince(155_000), 175_000)
		s.LastAnswer = nil
		if !slices.Equal(events.events, []string{down, back}) {
			t.Fatal(events.events)
		}
		// Without a status check, staying up as long as the grace period does.
		s.Game = nil
		s.Check(ctx, state("restarting", ""), 200_000)
		s.Check(ctx, state("restarting", ""), 261_000)
		s.Check(ctx, runSince(270_000), 300_000)
		s.Check(ctx, runSince(270_000), 330_000)
		// Stopped through Ludock once alerted, then started by hand.
		s.Check(ctx, state("restarting", ""), 400_000)
		s.Check(ctx, state("restarting", ""), 461_000)
		s.SetIntentionalStop(ctx, server.ID, true)
		s.Check(ctx, state("exited", ""), 470_000)
		s.Check(ctx, state("exited", ""), 600_000)
		if found, _ := problem(s, server.ID, 600_000); found != nil {
			t.Fatal("a server stopped through Ludock shown down:", found)
		}
		startedByHand := map[string]servers.Container{"mc": {ID: "mc", State: "running", StateSince: 695_000}}
		s.Check(ctx, startedByHand, 700_000)
		if len(events.events) != 5 {
			t.Fatal("running again before the run showed it:", events.events)
		}
		s.Check(ctx, startedByHand, 760_000)
		if !slices.Equal(events.events, []string{down, back, down, back, down, back}) {
			t.Fatal(events.events)
		}
		// Another loop is dated from its first exit, even once one of its
		// runs outlives the grace period while failing its health check, or
		// its restart policy gives up; a crash just after it recovers that
		// discovery still counts as the loop's is not, but the recovered run's
		// own exit, however soon, is.
		since := func(want int64) {
			t.Helper()
			if shown, _ := s.Get(ctx, server.ID); shown.State.Since != want {
				t.Fatal("shown since", shown.State.Since, "want", want)
			}
		}
		s.Check(ctx, map[string]servers.Container{"mc": {ID: "mc", State: "restarting", DownSince: 790_000}}, 800_000)
		since(790_000)
		failing := map[string]servers.Container{"mc": {ID: "mc", State: "running", Health: "unhealthy", StateSince: 802_000, RestartCount: 1, DownSince: 790_000}}
		s.Check(ctx, failing, 805_000)
		failing["mc"] = servers.Container{ID: "mc", State: "running", Health: "unhealthy", StateSince: 802_000, RestartCount: 1}
		s.Check(ctx, failing, 900_000)
		since(790_000)
		s.Check(ctx, map[string]servers.Container{"mc": {ID: "mc", State: "exited", DownSince: 902_500}}, 903_000)
		since(790_000)
		s.Check(ctx, map[string]servers.Container{"mc": {ID: "mc", State: "running", StateSince: 904_000}}, 905_000)
		s.Check(ctx, map[string]servers.Container{"mc": {ID: "mc", State: "restarting", DownSince: 790_000}}, 910_000)
		since(910_000)
		s.Check(ctx, map[string]servers.Container{"mc": {ID: "mc", State: "running", StateSince: 912_000}}, 975_000)
		s.Check(ctx, nil, 978_000)
		s.Check(ctx, map[string]servers.Container{"mc": {ID: "mc", State: "exited", DownSince: 974_000}}, 980_000)
		since(974_000)
		if !slices.Equal(events.events, []string{down, back, down, back, down, back, down, back}) {
			t.Fatal(events.events)
		}
	}
}

// A game that stops answering while its container runs alerts with its own
// message. While no recent query says it answers, as after Ludock restarts,
// the outage stays rather than recovering unseen; an answer recovers it.
func TestSilentGameOutage(t *testing.T) {
	s, events, server := setup(t)
	game := GameAnswering
	s.Game = func(string, servers.Container) GameAnswer { return game }
	s.Check(ctx, state("running", "healthy"), 1000)
	game = GameNotAnswering
	s.Check(ctx, state("running", "healthy"), 2000)
	s.Check(ctx, state("running", "unhealthy"), 62_000)
	if found, _ := problem(s, server.ID, 62_000); found == nil || found.State != NotAnswering || found.OutageStartedAt != 2000 {
		t.Fatal(found)
	}
	game = GameUnknown
	s.Check(ctx, state("running", "unhealthy"), 65_000)
	s.Check(ctx, state("running", "healthy"), 70_000)
	if found, _ := problem(s, server.ID, 70_000); found == nil {
		t.Fatal("an unknown game recovered")
	}
	game = GameAnswering
	s.Check(ctx, state("running", "healthy"), 80_000)
	if len(events.events) != 2 || events.events[0] != "Minecraft: running, but the game stopped answering." ||
		events.events[1] != "Minecraft: the game is answering again." {
		t.Fatal(events.events)
	}
	logged := audited(s, server.ID)
	if !slices.Equal(logged, []string{`server.silence.alerted {"since":2000}`, `server.silence.ended {"downFor":78000}`}) {
		t.Fatal(logged)
	}
}

// A game stops answering, then Docker becomes unreachable for a while. When
// Docker returns and no recent query says whether the game answers, the
// outage stays; only an answer recovers it.
func TestSilentOutageSurvivesDockerOutage(t *testing.T) {
	s, events, server := setup(t)
	game := GameAnswering
	s.Game = func(string, servers.Container) GameAnswer { return game }
	s.Check(ctx, state("running", "healthy"), 1000)
	game = GameNotAnswering
	s.Check(ctx, state("running", "healthy"), 2000)
	s.Check(ctx, state("running", "healthy"), 62_000)
	s.Check(ctx, nil, 70_000)
	game = GameUnknown
	s.Check(ctx, state("running", "healthy"), 80_000)
	if found, _ := problem(s, server.ID, 80_000); found == nil || found.State != NotAnswering || found.OutageStartedAt != 2000 {
		t.Fatal("an unknown game recovered", found)
	}
	game = GameAnswering
	s.Check(ctx, state("running", "healthy"), 90_000)
	if len(events.events) != 2 || events.events[0] != "Minecraft: running, but the game stopped answering." ||
		events.events[1] != "Minecraft: the game is answering again." {
		t.Fatal(events.events)
	}
}

// An outage already shown on pages that turns out to be a silent game tells
// open pages its new reason, though no new alert goes out, even after its
// wait before alerting was raised past how long it has lasted.
func TestOutageReasonChangeUpdatesPages(t *testing.T) {
	s, events, _, configure := setupWith(t, nil)
	var changes int
	s.Changed = func(string) { changes++ }
	game := GameAnswering
	s.Game = func(string, servers.Container) GameAnswer { return game }
	s.Check(ctx, state("running", "unhealthy"), 1000)
	s.Check(ctx, state("running", "unhealthy"), 62_000)
	if len(events.events) != 1 || changes != 1 {
		t.Fatal(events.events, changes)
	}
	configure(testPolicy(func(p *Policy) { p.GraceSeconds = 600 }))
	game = GameNotAnswering
	s.Check(ctx, state("running", "unhealthy"), 70_000)
	if len(events.events) != 1 || changes != 2 {
		t.Fatal(events.events, changes)
	}
}

// A server nobody configured is watched: one running when first seen alerts
// once it goes down, one already stopped stays quiet until it runs, and a
// server whose monitoring an administrator turned off stays off.
func TestNewServersAreWatched(t *testing.T) {
	d := dbtest.Open(t)
	store := &identity.Store{DB: d}
	list, _ := store.Reconcile(ctx, []identity.Observation{{ContainerID: "a", Name: "a", DisplayName: "Alpha", GameType: "minecraft"},
		{ContainerID: "b", Name: "b", DisplayName: "Beta", GameType: "minecraft"}}, "", 0)
	events := &recorder{}
	s := &Service{DB: d, Locks: locks.New(), Queue: events.queue, Store: store}
	both := func(a, b string) map[string]servers.Container {
		return map[string]servers.Container{"a": {ID: "a", State: a}, "b": {ID: "b", State: b}}
	}
	s.Check(ctx, nil, 0)
	if monitored, _ := s.Monitored(ctx); len(monitored) != 0 {
		t.Fatal("adopted while Docker was unreachable", monitored)
	}
	s.Check(ctx, both("running", "exited"), 1000)
	s.Check(ctx, both("exited", "exited"), 2000)
	s.Check(ctx, both("exited", "exited"), 200_000)
	if len(events.events) != 1 || events.events[0] != "Alpha: the server is down." {
		t.Fatal(events.events)
	}
	s.Check(ctx, both("exited", "running"), 300_000)
	s.Check(ctx, both("exited", "exited"), 301_000)
	s.Check(ctx, both("exited", "exited"), 500_000)
	if len(events.events) != 2 || events.events[1] != "Beta: the server is down." {
		t.Fatal(events.events)
	}

	if err := d.CreateUser(ctx, db.UserRecord{User: db.User{ID: "admin", Username: "admin", Role: db.RoleAdmin, CreatedAt: 1}, PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	off := DefaultPolicy()
	off.Enabled = false
	if _, err := s.Configure(ctx, "admin", dbtest.Session(t, d, "admin"), list[0].ID, off); err != nil {
		t.Fatal(err)
	}
	s.Check(ctx, both("exited", "exited"), 600_000)
	if saved, _ := s.Get(ctx, list[0].ID); saved.Policy.Enabled {
		t.Fatal("monitoring turned itself back on")
	}
}

// audited lists what Ludock logged for Activity about a server, as
// "action details".
func audited(s *Service, serverID string) []string {
	var logged []string
	rows, _ := s.DB.Query("SELECT action, details_json FROM audit_log WHERE target_id=? ORDER BY id", serverID)
	for rows.Next() {
		var action, details string
		rows.Scan(&action, &details)
		logged = append(logged, action+" "+details)
	}
	rows.Close()
	return logged
}
