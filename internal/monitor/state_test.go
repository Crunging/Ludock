package monitor

import (
	"context"
	"testing"

	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/ops"
)

// hookAt runs fn once, the first time Check or a capacity check reaches
// point.
func hookAt(t *testing.T, point string, fn func()) {
	t.Helper()
	testHook = func(at string) {
		if at == point {
			testHook = nil
			fn()
		}
	}
	t.Cleanup(func() { testHook = nil })
}

func testPolicy(change func(*Policy)) Policy {
	policy := DefaultPolicy()
	policy.GraceSeconds = 60
	if change != nil {
		change(&policy)
	}
	return policy
}

// An administrator changes a disk alert's threshold and back while a disk is
// measured: the reset it made is kept, the measurement isn't saved over it,
// and the next one alerts again under its own start.
func TestDiskResetDuringAMeasurementIsKept(t *testing.T) {
	s, events, server, configure := setupWith(t, nil)
	reset := false
	s.MeasureDisk = func(context.Context, identity.Server) ([]DiskSpace, error) {
		if reset {
			reset = false
			configure(testPolicy(func(p *Policy) { p.DiskPercent = 80 }))
			configure(testPolicy(nil))
		}
		return space(95), nil
	}
	s.CheckDisk(ctx, state("running", ""), 1000, true)
	reset = true
	s.CheckDisk(ctx, state("running", ""), 70_000, true)
	if disk, _ := s.Disk(ctx, server.ID); disk.CheckedAt != 0 || disk.alerting() || len(events.events) != 1 {
		t.Fatalf("%+v %v", disk, events.events)
	}
	s.CheckDisk(ctx, state("running", ""), 80_000, true)
	if disk, _ := s.Disk(ctx, server.ID); disk.Folders[0].AlertSince != 80_000 || len(events.events) != 2 {
		t.Fatalf("%+v %v", disk, events.events)
	}
}

// Monitoring turned off while memory is judged keeps the check from raising
// an alert under the old policy.
func TestPolicySavedDuringAMemoryCheck(t *testing.T) {
	s, events, server, configure := setupWith(t, func(p *Policy) { p.MemoryMinutes = 1 })
	const minute = 10 * 60_000
	addSamples(t, s, server.ID, minute, 950)
	hookAt(t, "watched", func() { configure(testPolicy(func(p *Policy) { p.Enabled, p.MemoryMinutes = false, 1 })) })
	s.CheckMemory(ctx, map[string]int64{server.ID: 1000}, state("running", ""), minute)
	if problems, _ := s.ResourceProblems(ctx); len(problems) != 0 || len(events.events) != 0 {
		t.Fatal(problems, events.events)
	}
}

// An administrator changes a memory alert's settings and back while memory
// is judged: the alert they ended stays ended without a recovery message.
func TestMemoryResetDuringACheckIsKept(t *testing.T) {
	s, events, server, configure := setupWith(t, func(p *Policy) { p.MemoryMinutes = 1 })
	const minute = 10 * 60_000
	addSamples(t, s, server.ID, minute, 950, 100)
	limits := map[string]int64{server.ID: 1000}
	s.CheckMemory(ctx, limits, state("running", ""), minute-60_000)
	if len(events.events) != 1 {
		t.Fatal(events.events)
	}
	hookAt(t, "watched", func() {
		configure(testPolicy(func(p *Policy) { p.MemoryMinutes = 2 }))
		configure(testPolicy(func(p *Policy) { p.MemoryMinutes = 1 }))
	})
	s.CheckMemory(ctx, limits, state("running", ""), minute)
	if problems, _ := s.ResourceProblems(ctx); len(problems) != 0 || len(events.events) != 1 {
		t.Fatal(problems, events.events)
	}
}

// Recovery turned off while a check judges a hung game queues no restart and
// sends nothing under the old policy; the next check alerts as usual.
func TestRecoveryTurnedOffDuringACheck(t *testing.T) {
	s, events, serverID, _ := recovering(t, nil)
	s.Check(ctx, run("running", 500), 1000)
	hookAt(t, "read", func() {
		if err := s.SetRecovery(ctx, "admin", "test-session-admin", serverID, false); err != nil {
			t.Fatal(err)
		}
	})
	s.Check(ctx, run("running", 500), 62_000)
	if jobs := recoveryJobs(t, s, serverID); len(jobs) != 0 || len(events.events) != 0 {
		t.Fatal(jobs, events.events)
	}
	s.Check(ctx, run("running", 500), 63_000)
	if jobs := recoveryJobs(t, s, serverID); len(jobs) != 0 || len(events.events) != 1 ||
		events.events[0] != "Minecraft: running, but the game stopped answering." {
		t.Fatal(jobs, events.events)
	}
}

// A recovery restart that finishes after a check read the operations is
// settled by the next check, once.
func TestRecoveryRestartFinishingDuringACheck(t *testing.T) {
	s, events, serverID, game := recovering(t, nil)
	jobs := hang(t, s, serverID, game, 1000, 500)
	*game = GameAnswering
	hookAt(t, "active", func() { finish(t, s, jobs[0], ops.Succeeded) })
	s.Check(ctx, run("running", 65_000), 70_000)
	if state, _ := s.Recovery(ctx, serverID); !state.Pending || len(events.events) != 1 {
		t.Fatalf("%+v %v", state, events.events)
	}
	s.Check(ctx, run("running", 65_000), 80_000)
	s.Check(ctx, run("running", 65_000), 90_000)
	if state, _ := s.Recovery(ctx, serverID); state.Last != RecoveryAnswered || state.Pending || len(events.events) != 2 {
		t.Fatalf("%+v %v", state, events.events)
	}
}

// A check that settles a skipped restart restarts the still-hung game in the
// same check, judging the state it just saved.
func TestRecoverySettlesAndRestartsInOneCheck(t *testing.T) {
	s, _, serverID, game := recovering(t, nil)
	jobs := hang(t, s, serverID, game, 1000, 500)
	if _, err := s.DB.Exec("UPDATE operations SET status=?,updated_at=66000 WHERE id=?", ops.Skipped, jobs[0]); err != nil {
		t.Fatal(err)
	}
	s.Check(ctx, run("running", 500), 66_000+RecoveryWait(Policy{GraceSeconds: 60}))
	if jobs := recoveryJobs(t, s, serverID); len(jobs) != 2 {
		t.Fatal(jobs)
	}
	if state, _ := s.Recovery(ctx, serverID); !state.Pending || len(state.Attempts) != 1 {
		t.Fatalf("%+v", state)
	}
}

// A server without a monitoring row reads as the default policy with no
// state, and turning recovery on gives it the default policy with recovery.
func TestServerWithoutAMonitoringRow(t *testing.T) {
	s, _, server := setup(t)
	if _, err := s.DB.Exec("DELETE FROM availability"); err != nil {
		t.Fatal(err)
	}
	availability, _ := s.Get(ctx, server.ID)
	disk, _ := s.Disk(ctx, server.ID)
	recovery, _ := s.Recovery(ctx, server.ID)
	problems, _ := s.ResourceProblems(ctx)
	if availability.Policy != DefaultPolicy() || availability.State != (State{}) || len(disk.Folders) != 0 || recovery.Last != "" ||
		len(problems) != 0 {
		t.Fatalf("%+v %+v %+v %v", availability, disk, recovery, problems)
	}
	if err := s.SetRecovery(ctx, "admin", "test-session-admin", server.ID, true); err != nil {
		t.Fatal(err)
	}
	want := DefaultPolicy()
	want.Recover = true
	if availability, _ := s.Get(ctx, server.ID); availability.Policy != want {
		t.Fatalf("%+v", availability)
	}
	var states int
	s.DB.QueryRow("SELECT COUNT(disk_json)+COUNT(memory_json)+COUNT(recovery_json) FROM availability").Scan(&states)
	if states != 0 {
		t.Fatal(states)
	}
}
