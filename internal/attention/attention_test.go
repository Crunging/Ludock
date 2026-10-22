package attention

import (
	"context"
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
	friend := &db.SessionUser{ID: "22222222-2222-4222-8222-222222222222", Username: "friend", Role: db.RoleOperator}
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
	if _, err := authorization.SetUserGrants(ctx, friend.ID, []authz.GrantInput{{ServerID: serverID,
		Capabilities: []authz.Capability{authz.ServerView, authz.SchedulesManage}}}, admin, session); err != nil {
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
		if _, err := authorization.SetUserGrants(ctx, friend.ID, nil, admin, session); err != nil {
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
