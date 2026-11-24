package monitor

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/notify"
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
// hear that the outage is gone.
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
}

func TestOneOutageAfterGraceAndOneRecovery(t *testing.T) {
	s, events, server := setup(t)
	s.Check(ctx, state("exited", ""), 1000)
	s.Check(ctx, state("exited", ""), 30_000)
	if len(events.events) != 0 {
		t.Fatal("notified before grace")
	}
	if found, _ := problem(s, server.ID, 30_000); found != nil {
		t.Fatal("problem before grace")
	}
	s.Check(ctx, state("exited", ""), 61_000)
	s.Check(ctx, state("exited", ""), 90_000)
	if found, _ := problem(s, server.ID, 90_000); found == nil || found.State != "exited" || found.OutageStartedAt != 1000 {
		t.Fatal(found)
	}
	s.Check(ctx, state("running", "starting"), 100_000)
	s.Check(ctx, state("running", ""), 110_000)
	s.Check(ctx, state("running", ""), 120_000)
	if len(events.events) != 2 || events.events[0] != "Minecraft: the server is down." || events.events[1] != "Minecraft: the server is back up." {
		t.Fatal(events.events)
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
// back even within the pause, and never alerts again, unless a stop or an
// automatic restart, which announces its own outcome, ends it.
func TestMonitorSuppressionKeepsAnAlertedOutage(t *testing.T) {
	const down, silent, back = "Minecraft: the server is down.", "Minecraft: running, but the game stopped answering.", "Minecraft: the server is back up."
	start := func(s *Service, id string) { s.Suppress(ctx, id); s.SetIntentionalStop(ctx, id, false) }
	restart := func(s *Service, id string) {
		s.Suppress(ctx, id)
		s.SetIntentionalStop(ctx, id, false)
		s.SetIntentionalStop(ctx, id, false)
		s.Suppress(ctx, id)
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
	}{
		{name: "started", alerted: true, act: start, after: "running", health: "healthy", want: []string{down, back}},
		{name: "started but unhealthy", alerted: true, act: start, after: "running", health: "unhealthy", want: []string{down}, stillDown: true},
		{name: "restarted", silent: true, alerted: true, act: restart, after: "running", game: GameWaiting, want: []string{silent},
			stillDown: true, answersBack: true},
		{name: "restarted and answering", silent: true, alerted: true, act: restart, after: "running", game: GameAnswering, want: []string{silent, back}},
		{name: "left stopped by other work", alerted: true, act: func(s *Service, id string) { s.Suppress(ctx, id); s.Suppress(ctx, id) },
			after: "exited", want: []string{down}, stillDown: true},
		{name: "stopped", silent: true, alerted: true, after: "exited", want: []string{silent}, act: func(s *Service, id string) {
			s.Suppress(ctx, id)
			s.SetIntentionalStop(ctx, id, true)
			s.Suppress(ctx, id)
		}},
		{name: "started within grace", act: start, after: "running"},
		{name: "restarted automatically", silent: true, alerted: true, after: "running", game: GameAnswering, want: []string{silent},
			act: func(s *Service, id string) {
				s.Suppress(ctx, id)
				s.SetIntentionalStop(ctx, id, false)
				s.SetIntentionalStop(ctx, id, false)
				s.SuppressRecovered(ctx, id)
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
			s.Check(ctx, state(c.after, c.health), now+10_000)
			if !slices.Equal(events.events, c.want) {
				t.Fatal("within the pause:", events.events)
			}
			for _, at := range []int64{now + 90_000, now + 100_000, now + 200_000} {
				if found, _ := problem(s, server.ID, at); (found != nil) != c.stillDown {
					t.Fatal("outage shown:", found)
				}
				s.Check(ctx, state(c.after, c.health), at)
			}
			if c.answersBack {
				game = GameAnswering
				s.Check(ctx, state(c.after, ""), now+210_000)
				s.Check(ctx, state(c.after, ""), now+220_000)
				c.want = append(c.want, back)
			}
			if !slices.Equal(events.events, c.want) {
				t.Fatal(events.events)
			}
		})
	}
}

// Docker outage is reported without rebinding.
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
		s.Suppress(ctx, server.ID)
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
		events.events[1] != "Minecraft: the server is back up." {
		t.Fatal(events.events)
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
		events.events[1] != "Minecraft: the server is back up." {
		t.Fatal(events.events)
	}
}

// An outage already shown on pages that turns out to be a silent game tells
// open pages its new reason, though no new alert goes out.
func TestOutageReasonChangeUpdatesPages(t *testing.T) {
	s, events, _ := setup(t)
	var changes int
	s.Changed = func(string) { changes++ }
	game := GameAnswering
	s.Game = func(string, servers.Container) GameAnswer { return game }
	s.Check(ctx, state("running", "unhealthy"), 1000)
	s.Check(ctx, state("running", "unhealthy"), 62_000)
	if len(events.events) != 1 || changes != 1 {
		t.Fatal(events.events, changes)
	}
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
