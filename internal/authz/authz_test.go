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
	operator = &db.SessionUser{ID: "operator", Username: "friend", Role: db.RoleMember}
	viewer   = &db.SessionUser{ID: "viewer", Username: "reader", Role: db.RoleMember}
	// adminSession is the admin's token hash in the current setup.
	adminSession string
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
	adminSession = dbtest.Session(t, d, admin.ID)
	store := &identity.Store{DB: d}
	servers, err := store.Reconcile(ctx, observations(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return &Service{DB: d, Servers: store}, servers
}

func setGrant(t *testing.T, s *Service, userID, serverID string, capabilities ...Capability) {
	t.Helper()
	if err := s.Share(ctx, admin, adminSession, userID, serverID, capabilities); err != nil {
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

// A stored grant never gives a member an administrator's capability, and a
// corrupt one gives nothing.
func TestMemberCeilingWithMalformedGrants(t *testing.T) {
	s, servers := setup(t)
	s.DB.Exec("INSERT INTO server_grants VALUES (?, ?, ?, ?)", viewer.ID, servers[0].ID,
		`["server.view","logs.read","files.read","console.shell","server.update","backups.restore","backups.read","backups.delete","server.recreate"]`, 1)
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
	// An unknown role is given nothing.
	s.DB.Exec(`UPDATE server_grants SET capabilities_json = '["server.view"]'`)
	if got := roleCapabilities(&db.SessionUser{ID: viewer.ID, Role: "viewer"}, &servers[0], []Capability{ServerView}); len(got) != 0 {
		t.Fatal(got)
	}
}

// Each level includes the one before; full access is everything a member
// may have, and none reaches what only administrators do.
func TestLevelCapabilities(t *testing.T) {
	want := map[Level][]Capability{
		LevelSee: {ServerView},
		LevelRun: {ServerView, ServerStart, ServerStop, ServerRestart, LogsRead, ConsoleExecute},
		LevelFull: {ServerView, ServerStart, ServerStop, ServerRestart, LogsRead, ConsoleExecute, FilesRead, FilesWrite,
			BackupsCreate, SchedulesManage},
	}
	for index, level := range Levels {
		got := level.Capabilities()
		if !slices.Equal(got, want[level]) || LevelOf(got) != level {
			t.Fatal(level, got)
		}
		if index > 0 && slices.ContainsFunc(Levels[index-1].Capabilities(), func(c Capability) bool { return !slices.Contains(got, c) }) {
			t.Fatal(level, "does not include", Levels[index-1])
		}
		for _, capability := range []Capability{ServerUpdate, ServerRecreate, BackupsRestore, BackupsRead, BackupsDelete, ConsoleShell} {
			if slices.Contains(got, capability) {
				t.Fatal(level, capability)
			}
		}
	}
	if !slices.Equal(LevelFull.Capabilities(), RoleCeiling(db.RoleMember)) {
		t.Fatal("full access is not the member ceiling")
	}
	// A level's capabilities in any order still name it; anything else is custom.
	for _, c := range []struct {
		capabilities []Capability
		level        Level
	}{
		{[]Capability{ConsoleExecute, ServerView, LogsRead, ServerRestart, ServerStop, ServerStart}, LevelRun},
		{[]Capability{ServerView, ServerStart, ServerStop}, LevelCustom},
		{[]Capability{ServerView, ServerStart, ServerStop, ServerRestart, LogsRead, ConsoleExecute, ConsoleShell}, LevelCustom},
		{[]Capability{LogsRead}, LevelNone},
		{nil, LevelNone},
	} {
		if got := LevelOf(c.capabilities); got != c.level {
			t.Fatal(c.capabilities, got)
		}
	}
	if Level("admin").Capabilities() != nil || LevelCustom.Capabilities() != nil {
		t.Fatal("an unnamed level allows something")
	}
}

func TestStaleActorsAreRechecked(t *testing.T) {
	s, servers := setup(t)
	id := servers[0].ID
	setGrant(t, s, operator.ID, id, ServerView, ServerStop)
	dbtest.UpdateUserAccess(ctx, s.DB, operator.ID, db.RoleMember, true)
	if got := effective(t, s, operator, id); len(got) != 0 {
		t.Fatal("disabled user kept access")
	}
	dbtest.UpdateUserAccess(ctx, s.DB, operator.ID, db.RoleMember, false)
	if ok, _ := s.Has(ctx, operator, id, ServerStop); !ok {
		t.Fatal("re-enabled user lost stop")
	}
	setGrant(t, s, operator.ID, id)
	if got := effective(t, s, operator, id); len(got) != 0 {
		t.Fatal("revoked grant kept access")
	}
	dbtest.UpdateUserAccess(ctx, s.DB, admin.ID, db.RoleMember, false)
	if _, err := s.AssertAdministrator(ctx, admin); !apperr.Is(err, "FORBIDDEN") {
		t.Fatal(err)
	}
	if got := effective(t, s, admin, id); len(got) != 0 {
		t.Fatal(got)
	}
}

// A material change hides a shared server from its operator, not from an
// administrator, until an administrator confirms it. The operator is told
// it is paused; someone it was never shared with still isn't told it exists.
func TestMaterialChangeSuspendsGrants(t *testing.T) {
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
		t.Fatal("paused server visible")
	}
	if got := effective(t, s, admin, first.ID); !slices.Equal(got, All) {
		t.Fatal("administrator lost the paused server:", got)
	}
	if _, err := s.Assert(ctx, operator, first.ID, ServerStop); !apperr.Is(err, "SERVER_PAUSED") ||
		err.Error() != PausedMessage(first.DisplayName) {
		t.Fatal(err)
	}
	if _, err := s.Assert(ctx, viewer, first.ID, ServerView); !apperr.Is(err, "SERVER_NOT_FOUND") {
		t.Fatal(err)
	}
	// Still paused while its container is gone, as between a compose down
	// and up.
	s.DB.Exec("UPDATE logical_servers SET status = 'missing' WHERE id = ?", first.ID)
	if _, err := s.Assert(ctx, operator, first.ID, ServerView); !apperr.Is(err, "SERVER_PAUSED") {
		t.Fatal(err)
	}
	s.DB.Exec("UPDATE logical_servers SET status = 'active' WHERE id = ?", first.ID)
	if grants, _ := s.UserGrants(ctx, operator.ID); len(grants) != 1 {
		t.Fatal("grant deleted")
	}
	if err := s.Servers.ResumeSharing(ctx, admin.ID, adminSession, first.ID, pending.BindingRevision); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Has(ctx, operator, first.ID, ServerStop); !ok {
		t.Fatal("confirming did not restore grant")
	}
}

// Assert distinguishes hidden from forbidden and audits. A member whose
// access was removed is told it ended, even once the server is missing and
// the grant's audit entry is pruned;
// another member, never given it but "removed" from it, that the server
// isn't found.
func TestAssertHiddenVersusForbidden(t *testing.T) {
	s, servers := setup(t)
	setGrant(t, s, viewer.ID, servers[0].ID, ServerView)
	if _, err := s.Assert(ctx, viewer, servers[1].ID, ServerView); !apperr.Is(err, "SERVER_NOT_FOUND") {
		t.Fatal(err)
	}
	for _, capabilities := range [][]Capability{{ServerView}, nil} {
		if err := s.Share(ctx, admin, adminSession, operator.ID, servers[1].ID, capabilities); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Share(ctx, admin, adminSession, viewer.ID, servers[1].ID, nil); err != nil {
		t.Fatal(err)
	}
	s.DB.Exec("UPDATE logical_servers SET status = 'missing', container_id = NULL WHERE id = ?", servers[1].ID)
	// The removal alone says so once the grant's own entry is pruned.
	s.DB.Exec("DELETE FROM audit_log WHERE action = 'server.sharing.updated' AND json_array_length(details_json, '$.capabilities') > 0")
	if _, err := s.Assert(ctx, operator, servers[1].ID, ServerView); !apperr.Is(err, "ACCESS_ENDED") {
		t.Fatal(err)
	}
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

// Sharing requires the admin session still valid.
func TestShareNeedsAdminSession(t *testing.T) {
	s, servers := setup(t)
	if err := s.DB.DeleteSession(ctx, adminSession); err != nil {
		t.Fatal(err)
	}
	if err := s.Share(ctx, admin, adminSession, operator.ID, servers[0].ID, []Capability{ServerView}); !apperr.Is(err, "AUTHENTICATION_REQUIRED") {
		t.Fatal(err)
	}
	if grants, _ := s.UserGrants(ctx, operator.ID); len(grants) != 0 {
		t.Fatal(grants)
	}
}

// Requests the page never sends: a member sharing, an administrator's
// capability, a share with an administrator, and unknown or removed servers.
func TestShareRefusesMoreThanAMemberMayHave(t *testing.T) {
	s, servers := setup(t)
	setGrant(t, s, operator.ID, servers[0].ID, LevelRun.Capabilities()...)
	if err := s.Share(ctx, operator, "", viewer.ID, servers[0].ID, []Capability{ServerView}); !apperr.Is(err, "FORBIDDEN") {
		t.Fatal(err)
	}
	for _, c := range []struct {
		userID, serverID string
		capabilities     []Capability
		message          string
	}{
		{viewer.ID, servers[0].ID, []Capability{ServerView, ConsoleShell}, "Only administrators can do that"},
		{viewer.ID, servers[0].ID, []Capability{ServerView, BackupsRestore}, "Only administrators can do that"},
		{viewer.ID, servers[0].ID, []Capability{"server.everything"}, "Only administrators can do that"},
		{viewer.ID, servers[0].ID, []Capability{LogsRead}, "Every kind of access includes seeing the server"},
		{viewer.ID, servers[0].ID, []Capability{ServerView, FilesWrite}, "Changing files also needs reading them"},
		{admin.ID, servers[0].ID, []Capability{ServerView}, "Administrators can already use every server"},
		{"nobody", servers[0].ID, []Capability{ServerView}, "User not found"},
		{viewer.ID, "nonexistent", []Capability{ServerView}, "Server not found"},
	} {
		if err := s.Share(ctx, admin, adminSession, c.userID, c.serverID, c.capabilities); err == nil || err.Error() != c.message {
			t.Fatal(c.capabilities, err)
		}
	}
	if grants, _ := s.UserGrants(ctx, viewer.ID); len(grants) != 0 {
		t.Fatal(grants)
	}
	if grants, _ := s.UserGrants(ctx, admin.ID); len(grants) != 0 {
		t.Fatal(grants)
	}
	// A server removed in the panel takes no new shares.
	s.DB.Exec("UPDATE logical_servers SET status = 'missing' WHERE id = ?", servers[1].ID)
	s.DB.Exec(`INSERT INTO settings VALUES ('server.choice:' || (SELECT external_identity FROM logical_servers WHERE id = ?), '{"managed":false}')`, servers[1].ID)
	if err := s.Share(ctx, admin, adminSession, viewer.ID, servers[1].ID, []Capability{ServerView}); !apperr.Is(err, "SERVER_NOT_FOUND") {
		t.Fatal(err)
	}
}

// Sharing lists members by name with their level, and every change is
// audited, including removal.
func TestSharingListsLevelsAndAudits(t *testing.T) {
	s, servers := setup(t)
	id := servers[0].ID
	setGrant(t, s, viewer.ID, id, ServerView)
	setGrant(t, s, operator.ID, id, ServerView, ServerStart)
	shares, err := s.Sharing(ctx, id)
	if err != nil || len(shares) != 2 || shares[0].Username != "friend" || shares[0].Level != LevelCustom ||
		shares[1].Username != "reader" || shares[1].Level != LevelSee {
		t.Fatal(shares, err)
	}
	setGrant(t, s, operator.ID, id)
	if shares, _ := s.Sharing(ctx, id); len(shares) != 1 {
		t.Fatal(shares)
	}
	var audits int
	s.DB.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action = 'server.sharing.updated' AND target_id = ? AND user_id = ?", id, admin.ID).Scan(&audits)
	if audits != 3 {
		t.Fatalf("audits = %d", audits)
	}
}
