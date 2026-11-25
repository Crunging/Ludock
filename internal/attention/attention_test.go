package attention

import (
	"context"
	"slices"
	"testing"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/monitor"
	"github.com/Crunging/Ludock/internal/schedules"
)

var ctx = context.Background()

func TestAServerRevokedWhileListingIsLeftOut(t *testing.T) {
	d := dbtest.Open(t)
	admin := &db.SessionUser{ID: "11111111-1111-4111-8111-111111111111", Username: "owner", Role: db.RoleAdmin}
	friend := &db.SessionUser{ID: "22222222-2222-4222-8222-222222222222", Username: "friend", Role: db.RoleMember}
	for _, user := range []*db.SessionUser{admin, friend} {
		if err := d.CreateUser(ctx, db.UserRecord{User: db.User{ID: user.ID, Username: user.Username, Role: user.Role, CreatedAt: 1},
			PasswordHash: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	session := dbtest.Session(t, d, admin.ID)
	store := &identity.Store{DB: d}
	list, err := store.Reconcile(ctx, []identity.Observation{{ContainerID: "mc", Name: "mc", DisplayName: "Minecraft", GameType: "minecraft"}}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	serverID := list[0].ID
	authorization := &authz.Service{DB: d, Servers: store}
	if err := authorization.Share(ctx, admin, session, friend.ID, serverID, []authz.Capability{authz.ServerView, authz.SchedulesManage}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO operations (id,server_id,actor_id,kind,status,phase,input_json,recovery_json,binding_revision,created_at,updated_at)
    VALUES ('00000000-0000-4000-8000-000000000001',?,?,'stop','failed','failed','{}','{}',1,1,1)`, serverID, admin.ID); err != nil {
		t.Fatal(err)
	}
	s := &Service{DB: d, Authz: authorization, Identity: store, Monitor: &monitor.Service{DB: d, Store: store},
		Schedules: &schedules.Service{DB: d, Authz: authorization, Identity: store}}
	// The friend loses the server after the list checked it.
	testHook = func(string) {
		if err := authorization.Share(ctx, admin, session, friend.ID, serverID, nil); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { testHook = nil })
	result, err := s.List(ctx, friend, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 0 {
		t.Fatalf("showed a revoked server's items: %+v", result.Items)
	}
}

// An interrupted backup whose result has no forceStopped field, such as one
// that only recorded its save, is listed like any other.
func TestAttentionListsAResultWithoutForceStopped(t *testing.T) {
	d := dbtest.Open(t)
	admin := &db.SessionUser{ID: "11111111-1111-4111-8111-111111111111", Username: "owner", Role: db.RoleAdmin}
	if err := d.CreateUser(ctx, db.UserRecord{User: db.User{ID: admin.ID, Username: admin.Username, Role: admin.Role, CreatedAt: 1},
		PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	store := &identity.Store{DB: d}
	list, err := store.Reconcile(ctx, []identity.Observation{{ContainerID: "mc", Name: "mc", DisplayName: "Minecraft", GameType: "minecraft"}}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO operations (id,server_id,actor_id,kind,status,phase,input_json,recovery_json,result_json,binding_revision,created_at,updated_at)
    VALUES ('00000000-0000-4000-8000-000000000001',?,?,'backup','interrupted','interrupted','{}','{}','{"saveStatus":"sent"}',1,1,1)`,
		list[0].ID, admin.ID); err != nil {
		t.Fatal(err)
	}
	authorization := &authz.Service{DB: d, Servers: store}
	s := &Service{DB: d, Authz: authorization, Identity: store, Monitor: &monitor.Service{DB: d, Store: store}}
	result, err := s.List(ctx, admin, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].Kind != "operation" || result.Items[0].ForceStopped {
		t.Fatalf("%+v", result.Items)
	}
}

// A failure leaves Needs attention once the same kind of work succeeds on
// the server later; a force-stop and an error that asks for an
// administrator stay, even on an undone restore, and so does a failure after
// the success. A restore whose data is settled leaves once a backup, restore
// or update succeeds; one stopped partway, one whose end Ludock doesn't
// know, and one whose undo found a stage folder gone stay.
func TestAttentionSettlesFailuresByALaterSuccess(t *testing.T) {
	d := dbtest.Open(t)
	admin := &db.SessionUser{ID: "11111111-1111-4111-8111-111111111111", Username: "owner", Role: db.RoleAdmin}
	if err := d.CreateUser(ctx, db.UserRecord{User: db.User{ID: admin.ID, Username: admin.Username, Role: admin.Role, CreatedAt: 1},
		PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	store := &identity.Store{DB: d}
	list, err := store.Reconcile(ctx, []identity.Observation{{ContainerID: "mc", Name: "mc", DisplayName: "Minecraft", GameType: "minecraft"}}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	const (
		undone   = `{"restore":{"status":"rolled_back","putBack":true}}`
		stranded = `{"restore":{"status":"replacing","roots":[{"phase":"old_moved"}]}}`
	)
	operations := []struct {
		id, kind, status, error, result, recovery string
	}{
		{"01", "update", "failed", "The new server stopped right after starting.", "{}", "{}"},
		{"02", "backup", "failed", "The backup folder is full.", "{}", "{}"},
		{"03", "restore", "interrupted", "Recovery requires administrator attention. The server has been left stopped where possible.", "{}", undone},
		{"04", "stop", "interrupted", "Recovery requires administrator attention. The server has been left stopped where possible.", "{}", "{}"},
		{"05", "restart", "succeeded", "", `{"forceStopped":true}`, "{}"},
		{"13", "restart", "failed", "The server didn't start again.", `{"forceStopped":true}`, "{}"},
		{"06", "start", "interrupted", "Ludock restarted while this ran. Check the server before trying again.", "{}", "{}"},
		{"07", "update", "succeeded", "", "{}", "{}"},
		{"08", "restart", "succeeded", "", "{}", "{}"},
		{"09", "restore", "succeeded", "", "{}", "{}"},
		{"10", "stop", "succeeded", "", "{}", "{}"},
		{"11", "start", "succeeded", "", "{}", "{}"},
		{"12", "restart", "failed", "The game didn't stop.", "{}", "{}"},
		{"14", "restore", "interrupted", "Ludock restarted while this ran. Check the server before trying again.", "{}", undone},
		{"15", "restore", "failed", "The server was started while this ran.", "{}", stranded},
		{"16", "restore", "failed", "The backup is damaged.", "{}", `{"restore":{"status":"unknown"}}`},
		{"19", "restore", "failed", "The server was started while this ran.", "{}", `{"restore":{"status":"rolled_back","stageMissing":true}}`},
		{"17", "backup", "succeeded", "", "{}", "{}"},
		{"18", "restore", "failed", "The server was started while this ran.", "{}", undone},
		{"20", "restore", "failed", "The backup is damaged.", "{}", `{"restore":{"status":"rolled_back"}}`},
	}
	for index, operation := range operations {
		if _, err := d.Exec(`INSERT INTO operations (id,server_id,actor_id,kind,status,phase,input_json,recovery_json,result_json,error,binding_revision,created_at,updated_at)
      VALUES (?,?,?,?,?,?,'{}',?,?,NULLIF(?,''),1,?,?)`, "00000000-0000-4000-8000-0000000000"+operation.id, list[0].ID, admin.ID,
			operation.kind, operation.status, operation.status, operation.recovery, operation.result, operation.error, index+1, index+1); err != nil {
			t.Fatal(err)
		}
	}
	authorization := &authz.Service{DB: d, Servers: store}
	s := &Service{DB: d, Authz: authorization, Identity: store, Monitor: &monitor.Service{DB: d, Store: store}}
	result, err := s.List(ctx, admin, 2)
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, item := range result.Items {
		listed = append(listed, item.OperationID[len(item.OperationID)-2:])
	}
	if want := []string{"15", "20", "18", "19", "16", "12", "13", "05", "04", "03"}; !slices.Equal(listed, want) {
		t.Fatalf("listed %v, want %v", listed, want)
	}
	if result.Items[0].Kind != "restore" || result.Items[1].Undone || !result.Items[2].Undone || result.Items[3].Undone || result.Items[4].Undone {
		t.Fatalf("%+v", result.Items[:5])
	}
}
