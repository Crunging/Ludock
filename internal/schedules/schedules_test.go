package schedules

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/ops"
)

var ctx = context.Background()

var (
	admin  = &db.SessionUser{ID: "11111111-1111-4111-8111-111111111111", Username: "owner", Role: db.RoleAdmin}
	friend = &db.SessionUser{ID: "22222222-2222-4222-8222-222222222222", Username: "friend", Role: db.RoleOperator}
)

var base = Input{Kind: "recurring", Action: "start", Enabled: true, Time: "08:00", Days: []int{0, 1, 2, 3, 4, 5, 6}, Timezone: "America/Los_Angeles"}

func at(value string) int64 {
	t, _ := time.Parse(time.RFC3339, value)
	return t.UnixMilli()
}

var due = at("2026-09-09T15:00:00Z")

type fixture struct {
	s        *Service
	serverID string
	obs      identity.Observation
}

func setup(t *testing.T) *fixture {
	t.Helper()
	d := dbtest.Open(t)
	for _, user := range []*db.SessionUser{admin, friend} {
		d.CreateUser(ctx, db.UserRecord{User: db.User{ID: user.ID, Username: user.Username, Role: user.Role, CreatedAt: 1}, PasswordHash: "x"})
	}
	store := &identity.Store{DB: d}
	obs := identity.Observation{ContainerID: "original", Name: "world", DisplayName: "World", GameType: "minecraft"}
	list, _ := store.Reconcile(ctx, []identity.Observation{obs}, "", 0)
	authorization := &authz.Service{DB: d, Servers: store}
	f := &fixture{s: &Service{DB: d, Authz: authorization, Identity: store, Runner: &ops.Runner{DB: d, Locks: locks.New()}, Locks: locks.New()},
		serverID: list[0].ID, obs: obs}
	f.grant(t, authz.ServerView, authz.ServerStart, authz.SchedulesManage)
	previous := db.Now
	db.Now = func() int64 { return due - 3600_000 }
	t.Cleanup(func() { db.Now = previous })
	return f
}

func (f *fixture) grant(t *testing.T, capabilities ...authz.Capability) {
	t.Helper()
	if _, err := f.s.Authz.SetUserGrants(ctx, friend.ID, []authz.GrantInput{{ServerID: f.serverID, Capabilities: capabilities}}, admin); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) operations(t *testing.T) int {
	var count int
	f.s.DB.QueryRow("SELECT COUNT(*) FROM operations").Scan(&count)
	return count
}

func (f *fixture) first(t *testing.T, actor *db.SessionUser) Schedule {
	t.Helper()
	list, err := f.s.List(ctx, actor, f.serverID)
	if err != nil || len(list) == 0 {
		t.Fatal(list, err)
	}
	return list[0]
}

func TestBackupOptionOnlyForRestarts(t *testing.T) {
	f := setup(t)
	withBackup := base
	withBackup.CreateBackup = true
	if _, err := f.s.Create(ctx, admin, f.serverID, withBackup); err == nil || !strings.Contains(err.Error(), "only available for restart") {
		t.Fatal(err)
	}
	withBackup.Action = "restart"
	schedule, err := f.s.Create(ctx, admin, f.serverID, withBackup)
	if err != nil || !schedule.CreateBackup {
		t.Fatal(err)
	}
	plain := withBackup
	plain.CreateBackup = false
	updated, _ := f.s.Update(ctx, admin, f.serverID, schedule.ID, 1, plain)
	if updated.Revision != 2 || updated.CreateBackup {
		t.Fatal(updated)
	}
	// Backup restarts need backups.create for the requester.
	f.grant(t, authz.ServerView, authz.ServerRestart, authz.SchedulesManage)
	if _, err := f.s.Create(ctx, friend, f.serverID, withBackup); !apperr.Is(err, "FORBIDDEN") {
		t.Fatal(err)
	}
}

func TestRunsOncePerSlotWithoutCatchingUp(t *testing.T) {
	f := setup(t)
	if _, err := f.s.Create(ctx, friend, f.serverID, base); err != nil {
		t.Fatal(err)
	}
	f.s.Run(ctx, due)
	f.s.Run(ctx, due+30_000)
	if f.operations(t) != 1 {
		t.Fatal(f.operations(t))
	}
	f.s.DB.Exec("UPDATE operations SET status='succeeded'")
	f.s.Run(ctx, due+86400_000+60_000)
	if f.operations(t) != 1 {
		t.Fatal("caught up a missed slot")
	}
	var input string
	f.s.DB.QueryRow("SELECT input_json FROM operations").Scan(&input)
	if !strings.Contains(input, `"scheduleId"`) || strings.Contains(input, "createBackup") {
		t.Fatal(input)
	}
}

func TestRevokedActionsDisabledOwnersAndBindingChanges(t *testing.T) {
	f := setup(t)
	f.s.Create(ctx, friend, f.serverID, base)
	if f.first(t, friend).NextRunAt == 0 {
		t.Fatal("no next run")
	}
	f.grant(t, authz.ServerView, authz.SchedulesManage)
	f.s.Run(ctx, due)
	schedule := f.first(t, admin)
	if f.operations(t) != 0 || !strings.HasPrefix(schedule.LastResult, "Suspended: required access") ||
		schedule.NextRunUnavailableReason != ActionAccessRemoved {
		t.Fatal(schedule)
	}
	f.grant(t, authz.ServerView, authz.ServerStart, authz.SchedulesManage)
	f.s.DB.UpdateUserAccess(ctx, friend.ID, db.RoleOperator, true)
	f.s.Run(ctx, due+86400_000)
	if f.operations(t) != 0 || f.first(t, admin).NextRunUnavailableReason != OwnerDisabled {
		t.Fatal(f.first(t, admin))
	}
	f.s.DB.UpdateUserAccess(ctx, friend.ID, db.RoleOperator, false)
	// A reviewed material change keeps the schedule suspended.
	changed := f.obs
	changed.GameType = "factorio"
	pending, _ := f.s.Identity.Reconcile(ctx, []identity.Observation{changed}, "", 0)
	f.s.Identity.ReviewBinding(ctx, f.serverID, pending[0].PendingFingerprint, -1)
	f.s.Run(ctx, due+2*86400_000)
	if f.operations(t) != 0 || !strings.Contains(f.first(t, admin).LastResult, "server configuration changed") {
		t.Fatal(f.first(t, admin))
	}
	f.s.DB.DeleteUser(ctx, friend.ID)
	if list, _ := f.s.List(ctx, admin, f.serverID); len(list) != 0 {
		t.Fatal("schedule of a deleted owner kept")
	}
}

func TestStaleEditsAndOwnership(t *testing.T) {
	f := setup(t)
	schedule, _ := f.s.Create(ctx, friend, f.serverID, base)
	if _, err := f.s.SetEnabled(ctx, friend, f.serverID, schedule.ID, 1, false); err != nil {
		t.Fatal(err)
	}
	later := base
	later.Time = "09:00"
	if _, err := f.s.Update(ctx, friend, f.serverID, schedule.ID, 1, later); !apperr.Is(err, "SCHEDULE_CHANGED") {
		t.Fatal(err)
	}
	if _, err := f.s.SetEnabled(ctx, friend, f.serverID, schedule.ID, 1, true); !apperr.Is(err, "SCHEDULE_CHANGED") {
		t.Fatal(err)
	}
	current := f.first(t, friend)
	if current.Revision != 2 || current.Enabled || current.Time != "08:00" {
		t.Fatal(current)
	}
	// Another operator can't see or change the friend's schedule; a stale
	// admin principal is rechecked.
	owned, _ := f.s.Create(ctx, admin, f.serverID, base)
	stale := &db.SessionUser{ID: friend.ID, Username: friend.Username, Role: db.RoleAdmin}
	if _, err := f.s.Update(ctx, stale, f.serverID, owned.ID, 1, later); !apperr.Is(err, "NOT_FOUND") {
		t.Fatal(err)
	}
	backup := base
	backup.Action = "backup"
	if _, err := f.s.Update(ctx, friend, f.serverID, schedule.ID, 2, backup); !apperr.Is(err, "FORBIDDEN") {
		t.Fatal(err)
	}
	if _, err := f.s.Update(ctx, admin, f.serverID, schedule.ID, 2, backup); err == nil || !strings.Contains(err.Error(), "required access") {
		t.Fatal(err)
	}
}

func TestPausingAllowedAfterRevocationButNotResume(t *testing.T) {
	f := setup(t)
	schedule, _ := f.s.Create(ctx, friend, f.serverID, base)
	f.grant(t, authz.ServerView, authz.SchedulesManage)
	paused, err := f.s.SetEnabled(ctx, friend, f.serverID, schedule.ID, 1, false)
	if err != nil || paused.Enabled || paused.NextRunAt != 0 {
		t.Fatal(paused, err)
	}
	for _, actor := range []*db.SessionUser{friend, admin} {
		if _, err := f.s.SetEnabled(ctx, actor, f.serverID, schedule.ID, 2, true); err == nil {
			t.Fatal("resumed without action access")
		}
	}
}

func TestOneTimeSchedules(t *testing.T) {
	f := setup(t)
	once := Input{Kind: "once", Action: "stop", Enabled: true, Date: "2026-09-09", Time: "08:00", Timezone: "America/Los_Angeles"}
	f.grant(t, authz.ServerView, authz.ServerStop, authz.SchedulesManage)
	past := once
	past.Date = "2026-09-08"
	if _, err := f.s.Create(ctx, friend, f.serverID, past); !apperr.Is(err, "SCHEDULE_TIME_PAST") {
		t.Fatal(err)
	}
	gap := once
	gap.Date, gap.Time = "2027-03-14", "02:30"
	if _, err := f.s.Create(ctx, friend, f.serverID, gap); !apperr.Is(err, "SCHEDULE_TIME_INVALID") {
		t.Fatal(err)
	}
	schedule, err := f.s.Create(ctx, friend, f.serverID, once)
	if err != nil || schedule.NextRunAt != due {
		t.Fatal(schedule, err)
	}
	f.s.Run(ctx, due+60_000)
	f.s.DB.Exec("UPDATE operations SET status='succeeded'")
	f.s.Run(ctx, due+120_000)
	current := f.first(t, friend)
	if f.operations(t) != 1 || current.CompletedAt == 0 || current.NextRunAt != 0 {
		t.Fatal(current)
	}
	// A run missed while Ludock was stopped is recorded, not replayed.
	missedRun := once
	missedRun.Date = "2026-09-10"
	second, _ := f.s.Create(ctx, friend, f.serverID, missedRun)
	f.s.Run(ctx, due+86400_000+schedtimeGrace()+60_000)
	list, _ := f.s.List(ctx, friend, f.serverID)
	for _, s := range list {
		if s.ID == second.ID && (s.LastResult != "Missed: Ludock was not running at the scheduled time" || s.CompletedAt == 0) {
			t.Fatal(s)
		}
	}
	if f.operations(t) != 1 {
		t.Fatal("missed run replayed")
	}
}

func TestInvalidSavedSchedulesAreReportedNotRun(t *testing.T) {
	f := setup(t)
	f.s.DB.Exec(`INSERT INTO schedules(id,server_id,owner_id,input_json,binding_revision,created_at) VALUES('33333333-3333-4333-8333-333333333333',?,?,'{"kind":"recurring","action":"explode"}',1,1)`,
		f.serverID, friend.ID)
	f.s.Run(ctx, due)
	var result string
	f.s.DB.QueryRow("SELECT last_result FROM schedules").Scan(&result)
	if result != "Suspended: saved schedule configuration is invalid; recreate this schedule" || f.operations(t) != 0 {
		t.Fatal(result)
	}
	if list, err := f.s.List(ctx, admin, f.serverID); err != nil || len(list) != 1 {
		t.Fatal("damaged schedule hides the list", err)
	}
}

func TestV050ScheduleParses(t *testing.T) {
	d := dbtest.OpenV050(t)
	var raw string
	d.QueryRow("SELECT input_json FROM schedules").Scan(&raw)
	input, err := ParseSaved(raw)
	if err != nil || input.Action != "restart" || input.Timezone != "Europe/Berlin" || !input.Enabled {
		t.Fatal(input, err)
	}
}

func schedtimeGrace() int64 { return 5 * 60_000 }
