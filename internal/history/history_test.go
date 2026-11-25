package history

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/identity"
)

var ctx = context.Background()

func TestPagesRecheckAccessAndHideTokens(t *testing.T) {
	d := dbtest.Open(t)
	admin := &db.SessionUser{ID: "admin", Username: "admin", Role: db.RoleAdmin}
	viewer := &db.SessionUser{ID: "viewer", Username: "viewer", Role: db.RoleMember}
	for _, user := range []*db.SessionUser{admin, viewer} {
		d.CreateUser(ctx, db.UserRecord{User: db.User{ID: user.ID, Username: user.Username, Role: user.Role, CreatedAt: 1}, PasswordHash: "x"})
	}
	store := &identity.Store{DB: d}
	list, _ := store.Reconcile(ctx, []identity.Observation{{ContainerID: "a", Name: "a", DisplayName: "a"}, {ContainerID: "b", Name: "b", DisplayName: "b"}}, "", 0)
	authorization := &authz.Service{DB: d, Servers: store}
	authorization.Share(ctx, admin, dbtest.Session(t, d, admin.ID), viewer.ID, list[0].ID, []authz.Capability{authz.ServerView})
	for index := range 5 {
		server := list[index%2].ID
		actor := "admin"
		if index == 4 {
			actor = "viewer"
		}
		d.Exec(`INSERT INTO operations (id,server_id,actor_id,kind,status,phase,input_json,binding_revision,created_at,updated_at)
      VALUES (?,?,?,'start','succeeded','succeeded','{}',1,?,?)`, fmt.Sprintf("00000000-0000-4000-8000-%012d", index), server, actor, index, index)
	}
	s := &Service{DB: d, Authz: authorization, Identity: store}
	page, next, err := s.Operations(ctx, admin, Query{Limit: 2, From: -1, To: -1})
	if err != nil || len(page) != 2 || next == "" || page[0].Actor.ID != "viewer" || page[0].Actor.Name != "viewer" {
		t.Fatal(page, next, err)
	}
	rest, last, _ := s.Operations(ctx, admin, Query{Limit: 2, Cursor: next, From: -1, To: -1})
	if len(rest) != 2 || last == "" || rest[0].CreatedAt != 2 {
		t.Fatal(rest)
	}
	if _, _, err := s.Operations(ctx, admin, Query{Limit: 2, Cursor: next, Action: "stop", From: -1, To: -1}); !apperr.Is(err, "INVALID_HISTORY_CURSOR") {
		t.Fatal(err)
	}
	visible, _, _ := s.Operations(ctx, viewer, Query{From: -1, To: -1})
	if len(visible) != 3 {
		t.Fatal(len(visible))
	}
	if found, _, _ := s.Operations(ctx, admin, Query{Actor: "VIEW", From: -1, To: -1}); len(found) != 1 {
		t.Fatal(found)
	}
	d.WriteAudit(ctx, nil, db.AuditEntry{Action: "server.start.failed", TargetType: "server", TargetID: list[0].ID,
		Details: map[string]string{"operationId": "00000000-0000-4000-8000-000000000000", "actorId": "viewer"}})
	entries, _, err := s.Audit(ctx, Query{Status: "failed", From: -1, To: -1})
	if err != nil || len(entries) != 1 || entries[0].Status != "failed" || entries[0].OperationID == "" ||
		entries[0].Details.(map[string]any)["actorId"] != "viewer" {
		t.Fatal(entries, err)
	}
}

// A member's Activity is read as of one moment: a move that pauses sharing
// and brings in another server's work, committing just after access was
// checked, shows neither half.
func TestOperationsReadOneSnapshot(t *testing.T) {
	d := dbtest.Open(t)
	admin := &db.SessionUser{ID: "admin", Username: "admin", Role: db.RoleAdmin}
	member := &db.SessionUser{ID: "member", Username: "member", Role: db.RoleMember}
	for _, user := range []*db.SessionUser{admin, member} {
		d.CreateUser(ctx, db.UserRecord{User: db.User{ID: user.ID, Username: user.Username, Role: user.Role, CreatedAt: 1}, PasswordHash: "x"})
	}
	store := &identity.Store{DB: d}
	list, _ := store.Reconcile(ctx, []identity.Observation{{ContainerID: "a", Name: "a", DisplayName: "a"}, {ContainerID: "b", Name: "b", DisplayName: "b"}}, "", 0)
	authorization := &authz.Service{DB: d, Servers: store}
	authorization.Share(ctx, admin, dbtest.Session(t, d, admin.ID), member.ID, list[0].ID, []authz.Capability{authz.ServerView})
	const moved = "00000000-0000-4000-8000-000000000001"
	d.Exec(`INSERT INTO operations (id,server_id,actor_id,kind,status,phase,input_json,binding_revision,created_at,updated_at)
    VALUES (?,?,'admin','restore','succeeded','succeeded','{}',1,1,1)`, moved, list[1].ID)
	testHook = func(string) {
		if err := d.Tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.Exec("UPDATE logical_servers SET sharing_paused=1 WHERE id=?", list[0].ID); err != nil {
				return err
			}
			_, err := tx.Exec("UPDATE operations SET server_id=? WHERE id=?", list[0].ID, moved)
			return err
		}); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { testHook = nil })
	s := &Service{DB: d, Authz: authorization, Identity: store}
	if visible, _, err := s.Operations(ctx, member, Query{ServerID: list[0].ID, From: -1, To: -1}); err != nil || len(visible) != 0 {
		t.Fatal(visible, err)
	}
}
