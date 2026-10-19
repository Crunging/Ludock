package authz

import (
	"context"
	"slices"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/identity"
)

var ctx = context.Background()

var (
	admin    = &db.SessionUser{ID: "admin", Username: "owner", Role: db.RoleAdmin}
	operator = &db.SessionUser{ID: "operator", Username: "friend", Role: db.RoleOperator}
	viewer   = &db.SessionUser{ID: "viewer", Username: "reader", Role: db.RoleViewer}
)

func observations() []identity.Observation {
	var result []identity.Observation
	for _, name := range []string{"minecraft", "factorio"} {
		result = append(result, identity.Observation{ContainerID: "docker-" + name, Name: name, DisplayName: name, GameType: name})
	}
	return result
}

func setup(t *testing.T) (*Service, []identity.Server) {
	t.Helper()
	d := dbtest.Open(t)
	for _, user := range []*db.SessionUser{admin, operator, viewer} {
		if err := d.CreateUser(ctx, db.UserRecord{User: db.User{ID: user.ID, Username: user.Username, Role: user.Role, CreatedAt: 1},
			PasswordHash: "not-a-real-hash"}); err != nil {
			t.Fatal(err)
		}
	}
	store := &identity.Store{DB: d}
	servers, err := store.Reconcile(ctx, observations(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return &Service{DB: d, Servers: store}, servers
}

func setGrant(t *testing.T, s *Service, userID, serverID string, capabilities ...Capability) {
	t.Helper()
	grants, _ := s.UserGrants(ctx, userID)
	var inputs []GrantInput
	for _, grant := range grants {
		if grant.ServerID != serverID {
			inputs = append(inputs, GrantInput{grant.ServerID, grant.Capabilities})
		}
	}
	if _, err := s.SetUserGrants(ctx, userID, append(inputs, GrantInput{serverID, capabilities}), admin); err != nil {
		t.Fatal(err)
	}
}

func effective(t *testing.T, s *Service, actor *db.SessionUser, serverID string) []Capability {
	t.Helper()
	capabilities, err := s.Effective(ctx, actor, serverID)
	if err != nil {
		t.Fatal(err)
	}
	return capabilities
}

func TestViewerCeilingWithMalformedGrants(t *testing.T) {
	s, servers := setup(t)
	s.DB.Exec("INSERT INTO server_grants VALUES (?, ?, ?, ?)", viewer.ID, servers[0].ID,
		`["server.view","logs.read","files.read","files.write","console.shell","server.start"]`, 1)
	if got := effective(t, s, viewer, servers[0].ID); !slices.Equal(got, []Capability{ServerView, LogsRead, FilesRead}) {
		t.Fatal(got)
	}
	s.DB.Exec("UPDATE server_grants SET capabilities_json = 'invalid json'")
	if got := effective(t, s, viewer, servers[0].ID); len(got) != 0 {
		t.Fatal(got)
	}
	// Writing without reading never takes effect, and nothing works without view.
	s.DB.Exec(`UPDATE server_grants SET capabilities_json = '["server.view","files.write"]'`)
	if got := effective(t, s, viewer, servers[0].ID); !slices.Equal(got, []Capability{ServerView}) {
		t.Fatal(got)
	}
	s.DB.Exec(`UPDATE server_grants SET capabilities_json = '["logs.read"]'`)
	if got := effective(t, s, viewer, servers[0].ID); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestStaleActorsAreRechecked(t *testing.T) {
	s, servers := setup(t)
	id := servers[0].ID
	setGrant(t, s, operator.ID, id, ServerView, ServerStop)
	s.DB.UpdateUserAccess(ctx, operator.ID, db.RoleViewer, false)
	if ok, _ := s.Has(ctx, operator, id, ServerStop); ok {
		t.Fatal("downgraded role kept stop")
	}
	if ok, _ := s.Has(ctx, operator, id, ServerView); !ok {
		t.Fatal("lost view")
	}
	s.DB.UpdateUserAccess(ctx, operator.ID, db.RoleOperator, true)
	if got := effective(t, s, operator, id); len(got) != 0 {
		t.Fatal("disabled user kept access")
	}
	s.DB.UpdateUserAccess(ctx, operator.ID, db.RoleOperator, false)
	if ok, _ := s.Has(ctx, operator, id, ServerStop); !ok {
		t.Fatal("re-enabled user lost stop")
	}
	s.SetUserGrants(ctx, operator.ID, nil, admin)
	if got := effective(t, s, operator, id); len(got) != 0 {
		t.Fatal("revoked grant kept access")
	}
	s.DB.UpdateUserAccess(ctx, admin.ID, db.RoleViewer, false)
	if _, err := s.AssertAdministrator(ctx, admin); !apperr.Is(err, "FORBIDDEN") {
		t.Fatal(err)
	}
	if got := effective(t, s, admin, id); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestMaterialChangeSuspendsGrantsUntilReview(t *testing.T) {
	s, servers := setup(t)
	first := servers[slices.IndexFunc(servers, func(server identity.Server) bool { return server.GameType == "minecraft" })]
	setGrant(t, s, operator.ID, first.ID, ServerView, ServerStop)
	replaced := observations()
	for index := range replaced {
		replaced[index].ContainerID += "-replacement"
	}
	s.Servers.Reconcile(ctx, replaced, "", 0)
	if ok, _ := s.Has(ctx, operator, first.ID, ServerStop); !ok {
		t.Fatal("recreation dropped grant")
	}
	replaced[0].GameType = "unrelated-server"
	result, _ := s.Servers.Reconcile(ctx, replaced, "", 0)
	pending := result[slices.IndexFunc(result, func(server identity.Server) bool { return server.ID == first.ID })]
	if ok, _ := s.Has(ctx, operator, first.ID, ServerView); ok {
		t.Fatal("suspended server visible")
	}
	if got := effective(t, s, admin, first.ID); !slices.Equal(got, []Capability{ServerView}) {
		t.Fatal(got)
	}
	if _, err := s.Assert(ctx, operator, first.ID, ServerStop); !apperr.Is(err, "SERVER_NOT_FOUND") {
		t.Fatal(err)
	}
	if grants, _ := s.UserGrants(ctx, operator.ID); len(grants) != 1 {
		t.Fatal("grant deleted")
	}
	s.Servers.ReviewBinding(ctx, first.ID, pending.PendingFingerprint, -1)
	if ok, _ := s.Has(ctx, operator, first.ID, ServerStop); !ok {
		t.Fatal("review did not restore grant")
	}
}

func TestAssertDistinguishesHiddenFromForbiddenAndAudits(t *testing.T) {
	s, servers := setup(t)
	setGrant(t, s, viewer.ID, servers[0].ID, ServerView)
	if _, err := s.Assert(ctx, viewer, servers[1].ID, ServerView); !apperr.Is(err, "SERVER_NOT_FOUND") {
		t.Fatal(err)
	}
	if _, err := s.Assert(ctx, viewer, "nonexistent", ServerStop); !apperr.Is(err, "SERVER_NOT_FOUND") {
		t.Fatal(err)
	}
	if _, err := s.Assert(ctx, viewer, servers[0].ID, ServerStop); !apperr.Is(err, "FORBIDDEN") {
		t.Fatal(err)
	}
	if server, err := s.Assert(ctx, viewer, servers[0].ID, ServerView); err != nil || server.ID != servers[0].ID {
		t.Fatal(err)
	}
	var denials int
	s.DB.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action = 'authorization.denied'").Scan(&denials)
	if denials != 2 {
		t.Fatalf("denials = %d", denials)
	}
}

func TestGrantReplacementIsAtomic(t *testing.T) {
	s, servers := setup(t)
	setGrant(t, s, operator.ID, servers[0].ID, ServerView)
	_, err := s.SetUserGrants(ctx, operator.ID, []GrantInput{{servers[1].ID, []Capability{ServerView}},
		{"nonexistent", []Capability{ServerView}}}, admin)
	if !apperr.Is(err, "SERVER_NOT_FOUND") {
		t.Fatal(err)
	}
	if grants, _ := s.UserGrants(ctx, operator.ID); len(grants) != 1 || grants[0].ServerID != servers[0].ID {
		t.Fatal(grants)
	}
	if _, err := s.SetUserGrants(ctx, viewer.ID, []GrantInput{{servers[0].ID, []Capability{ServerView}}}, operator); !apperr.Is(err, "FORBIDDEN") {
		t.Fatal(err)
	}
	if _, err := s.SetUserGrants(ctx, admin.ID, nil, admin); err == nil || err.Error() != "Administrators already have access to all eligible servers" {
		t.Fatal(err)
	}
	for _, c := range []struct {
		capabilities []Capability
		message      string
	}{
		{[]Capability{ServerView, ServerStop}, "A capability exceeds this user's role"},
		{[]Capability{LogsRead}, "Server access is required for every server capability"},
	} {
		if _, err := s.SetUserGrants(ctx, viewer.ID, []GrantInput{{servers[0].ID, c.capabilities}}, admin); err == nil || err.Error() != c.message {
			t.Fatal(err)
		}
	}
	if _, err := s.SetUserGrants(ctx, operator.ID, []GrantInput{{servers[0].ID, []Capability{ServerView, FilesWrite}}}, admin); err == nil ||
		err.Error() != "File writing also requires file reading" {
		t.Fatal(err)
	}
	if _, err := s.SetUserGrants(ctx, operator.ID, []GrantInput{{servers[0].ID, nil}, {servers[0].ID, nil}}, admin); err == nil ||
		err.Error() != "Duplicate server assignment" {
		t.Fatal(err)
	}
}
