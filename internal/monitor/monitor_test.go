package monitor

import (
	"context"
	"testing"

	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/servers"
)

var ctx = context.Background()

type recorder struct{ events []string }

func (r *recorder) notify(key, message string) { r.events = append(r.events, message) }

func setup(t *testing.T) (*Service, *recorder, identity.Server) {
	t.Helper()
	d := dbtest.Open(t)
	store := &identity.Store{DB: d}
	list, _ := store.Reconcile(ctx, []identity.Observation{{ContainerID: "mc", Name: "mc", DisplayName: "Minecraft", GameType: "minecraft"}}, "", 0)
	events := &recorder{}
	s := &Service{DB: d, Locks: locks.New(), Notify: events.notify, Store: store}
	if err := d.CreateUser(ctx, db.UserRecord{User: db.User{ID: "admin", Username: "admin", Role: db.RoleAdmin, CreatedAt: 1}, PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Configure(ctx, "admin", dbtest.Session(t, d, "admin"), list[0].ID, Policy{Enabled: true, GraceSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	return s, events, list[0]
}

func state(value, health string) map[string]servers.Container {
	return map[string]servers.Container{"mc": {ID: "mc", State: value, Health: health}}
}

func TestOneOutageAfterGraceAndOneRecovery(t *testing.T) {
	s, events, server := setup(t)
	s.Check(ctx, state("exited", ""), 1000)
	s.Check(ctx, state("exited", ""), 30_000)
	if len(events.events) != 0 {
		t.Fatal("notified before grace")
	}
	if problem, _ := s.Problem(ctx, server.ID, 30_000); problem != nil {
		t.Fatal("problem before grace")
	}
	s.Check(ctx, state("exited", ""), 61_000)
	s.Check(ctx, state("exited", ""), 90_000)
	if problem, _ := s.Problem(ctx, server.ID, 90_000); problem == nil || problem.State != "exited" || problem.OutageStartedAt != 1000 {
		t.Fatal(problem)
	}
	s.Check(ctx, state("running", "starting"), 100_000)
	s.Check(ctx, state("running", ""), 110_000)
	s.Check(ctx, state("running", ""), 120_000)
	if len(events.events) != 2 || events.events[0] != "Minecraft is unavailable." || events.events[1] != "Minecraft recovered." {
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
	release, _ := s.Locks.Acquire([]string{"server:" + server.ID}, nil)
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

// Docker outage is reported without rebinding.
func TestDockerOutage(t *testing.T) {
	s, events, server := setup(t)
	s.Check(ctx, nil, 1000)
	s.Check(ctx, nil, 70_000)
	if len(events.events) != 1 || events.events[0] != "Ludock cannot reach Docker to verify Minecraft." {
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
	if len(events.events) != 1 || events.events[0] != "Minecraft is unavailable." {
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
	if _, err := s.Problem(ctx, server.ID, 200_000); err == nil {
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
	if changed, err := s.transition(ctx, server.ID, read, "outage_started_at=?,notified=1", 1000); err != nil || changed {
		t.Fatal(changed, err)
	}
	if problem, _ := s.Problem(ctx, server.ID, 200_000); problem != nil {
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
