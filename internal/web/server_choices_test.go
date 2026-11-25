package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/history"
	"github.com/Crunging/Ludock/internal/schedules"
)

func (p *pages) addServer(principal *auth.Principal, container string, game ...string) *http.Response {
	form := url.Values{"_csrf": {CSRFToken(principal.Token)}, "container": {container}, "game": game}
	return p.do("POST", "/servers/add", principal, strings.NewReader(form.Encode()), map[string]string{"HX-Request": "true"}).Result()
}

func (p *pages) managed() int {
	p.t.Helper()
	current, err := p.app.Servers.RefreshNow(context.Background())
	if err != nil {
		p.t.Fatal(err)
	}
	return len(current)
}

// Adding a server is for administrators only, and a forged request can't
// add a container the panel never offers: one its owner opted out, one that
// controls Docker, or Ludock's own.
func TestAddServerRefusesForgedRequests(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	game := p.engine.AddContainer(fakedocker.GameOptions{Name: "satisfactory", Image: "ich777/steamcmd:satisfactory"})
	optedOut := p.engine.AddContainer(fakedocker.GameOptions{Name: "old", Image: "ich777/steamcmd:valheim",
		Labels: map[string]string{"ludock.enable": "false"}})
	tool := p.engine.AddContainer(fakedocker.GameOptions{Name: "portainer", Image: "portainer/portainer-ce",
		Mounts: []docker.MountPoint{{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"}}})
	self := p.engine.AddContainer(fakedocker.GameOptions{Name: "ludock", Image: "ghcr.io/crunging/ludock:0.7.0"})

	operator := p.user("u-operator", "operator", db.RoleMember)
	if response := p.do("GET", "/fragments/unmanaged", operator, nil, map[string]string{"HX-Request": "true"}); response.Code != http.StatusForbidden ||
		strings.Contains(response.Body.String(), "satisfactory") {
		t.Fatal(response.Code, response.Body.String())
	}
	if response := p.addServer(operator, game); response.StatusCode != http.StatusForbidden || p.managed() != 1 {
		t.Fatal(response.StatusCode)
	}

	list := p.do("GET", "/fragments/unmanaged", p.admin, nil, map[string]string{"HX-Request": "true"}).Body.String()
	if !strings.Contains(list, "satisfactory") || !strings.Contains(list, `value="satisfactory" selected`) {
		t.Fatal(list)
	}
	for _, hidden := range []string{"old", "portainer", "ludock:0.7.0", "valheim-server"} {
		if strings.Contains(list, hidden) {
			t.Fatal(hidden, list)
		}
	}
	for _, id := range []string{optedOut, tool, self, p.docker, "not-a-container"} {
		response := p.addServer(p.admin, id)
		if response.StatusCode != http.StatusOK || response.Header.Get("HX-Trigger") != "" || p.managed() != 1 {
			t.Fatal(id, response.StatusCode)
		}
	}
	if response := p.addServer(p.admin, game, "valheim"); response.Header.Get("HX-Trigger") != "servers-changed" || p.managed() != 2 {
		t.Fatal(response.StatusCode)
	}
	// A new server is hidden from everyone else until it is shared.
	if page := p.do("GET", "/fragments/servers", operator, nil, map[string]string{"HX-Request": "true"}); strings.Contains(page.Body.String(), "satisfactory") {
		t.Fatal(page.Body.String())
	}

	// Removed, it is offered again with the game it was added as.
	servers, _ := p.app.Identity.List(context.Background(), "")
	for _, server := range servers {
		if server.GameType == "valheim" && server.ContainerID == game {
			if response := p.post("/servers/"+server.ID+"/remove", operator, CSRFToken(operator.Token)); response.Code != http.StatusForbidden {
				t.Fatal(response.Code)
			}
			if response := p.post("/servers/"+server.ID+"/remove", p.admin, CSRFToken(p.admin.Token)); response.Header().Get("HX-Redirect") != "/" {
				t.Fatal(response.Code, response.Body.String())
			}
		}
	}
	list = p.do("GET", "/fragments/unmanaged", p.admin, nil, map[string]string{"HX-Request": "true"}).Body.String()
	if p.managed() != 1 || !strings.Contains(list, `value="valheim" selected`) {
		t.Fatal(list)
	}
}

// A standalone container renamed in Docker is a new server, and the old one
// is missing with a member's grant and schedule, a backup, and finished
// work. Its page and Needs attention suggest the new server when it has the
// same game and data folders; the move gives it everything, the old server
// leaves Ludock, and its work shows in the new server's Activity. A new
// server with its own schedule or grant for the same person keeps its own.
// One with other data folders is offered only by name: moved sharing waits
// for a confirmation, and the backups say the data lived elsewhere. Work on
// either server, a member, or an administrator demoted at the write is
// refused with nothing changed.
func TestRenamedServerTakesOver(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const backupID, operationID = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	restart := schedules.Input{Kind: "recurring", Action: "restart", Enabled: true, Time: "04:00", Timezone: "UTC", Days: []int{0, 1, 2, 3, 4, 5, 6}}
	for _, test := range []struct {
		name string
		// data moves the renamed container's data to another host folder.
		data bool
		// before runs once the renamed server is found; it returns who moves.
		before func(t *testing.T, p *pages, member *auth.Principal, renamed string) *auth.Principal
		status int
		check  func(t *testing.T, p *pages, member *auth.Principal, old, renamed, body string)
	}{
		{name: "same data", status: http.StatusOK, check: func(t *testing.T, p *pages, member *auth.Principal, old, renamed, body string) {
			if !strings.Contains(body, "Moved valheim to valheim-world: 1 backup, 1 schedule, and sharing with 1 person.") {
				t.Fatal(body)
			}
			page := p.do("GET", "/servers/"+renamed+"?tab=backups&restore="+backupID, p.admin, nil, nil).Body.String()
			if strings.Contains(page, "lived somewhere else") || !strings.Contains(page, `name="confirmation"`) {
				t.Fatal(page)
			}
			restore := url.Values{"backupId": {backupID}, "confirmation": {"valheim-world"}}
			if response := p.action("/servers/"+renamed+"/restores", p.admin, restore); response.StatusCode != http.StatusOK {
				t.Fatal(response.StatusCode, readBody(t, response))
			}
			var owner string
			p.app.DB.QueryRow("SELECT owner_id FROM schedules WHERE server_id=?", renamed).Scan(&owner)
			if can, _ := p.app.Authz.Has(ctx, &member.User, renamed, authz.ServerRestart); !can || owner != member.User.ID {
				t.Fatal(can, owner)
			}
			operations, _, err := p.app.History.Operations(ctx, &member.User, history.Query{ServerID: renamed, From: -1, To: -1})
			if err != nil || !slices.ContainsFunc(operations, func(o history.Operation) bool { return o.ID == operationID }) {
				t.Fatal(operations, err)
			}
			var details string
			p.app.DB.QueryRow("SELECT details_json FROM audit_log WHERE action='server.taken-over' AND target_id=?", renamed).Scan(&details)
			if !strings.Contains(details, `"from":"valheim"`) || !strings.Contains(details, `"to":"valheim-world"`) {
				t.Fatal(details)
			}
		}},
		{name: "new server has its own", status: http.StatusOK,
			before: func(t *testing.T, p *pages, member *auth.Principal, renamed string) *auth.Principal {
				if _, err := p.app.Schedules.CreateAsAdmin(ctx, &p.admin.User, p.admin.TokenHash, renamed, restart); err != nil {
					t.Fatal(err)
				}
				if err := p.app.Authz.Share(ctx, &p.admin.User, p.admin.TokenHash, member.User.ID, renamed, []authz.Capability{authz.ServerView}); err != nil {
					t.Fatal(err)
				}
				return p.admin
			},
			check: func(t *testing.T, p *pages, member *auth.Principal, old, renamed, body string) {
				for _, want := range []string{"0 schedules, and sharing with 0 people", "already has the same schedule", "Restart, 04:00",
					"friend kept the access they already had"} {
					if !strings.Contains(body, want) {
						t.Fatal(want, body)
					}
				}
				var count int
				p.app.DB.QueryRow("SELECT COUNT(*) FROM schedules").Scan(&count)
				if can, _ := p.app.Authz.Has(ctx, &member.User, renamed, authz.ServerRestart); can || count != 1 {
					t.Fatal(can, count)
				}
			}},
		{name: "other data folders", data: true, status: http.StatusOK, check: func(t *testing.T, p *pages, member *auth.Principal, old, renamed, body string) {
			if !strings.Contains(body, "once you confirm the change") {
				t.Fatal(body)
			}
			if can, _ := p.app.Authz.Has(ctx, &member.User, renamed, authz.ServerView); can {
				t.Fatal("shared before the change was confirmed")
			}
			page := p.do("GET", "/servers/"+renamed+"?tab=backups&restore="+backupID, p.admin, nil, nil).Body.String()
			if !strings.Contains(page, "Taken when the server’s data lived somewhere else") {
				t.Fatal(page)
			}
		}},
		{name: "work on the missing server", status: http.StatusConflict,
			before: func(t *testing.T, p *pages, member *auth.Principal, renamed string) *auth.Principal {
				p.app.DB.Exec("UPDATE operations SET status='queued'")
				return p.admin
			}},
		{name: "work on the new server", status: http.StatusConflict,
			before: func(t *testing.T, p *pages, member *auth.Principal, renamed string) *auth.Principal {
				p.app.DB.Exec("UPDATE operations SET server_id=?, status='running'", renamed)
				return p.admin
			}},
		{name: "member", status: http.StatusForbidden,
			before: func(t *testing.T, p *pages, member *auth.Principal, renamed string) *auth.Principal { return member }},
		{name: "demoted at the write", status: http.StatusNotFound,
			before: func(t *testing.T, p *pages, member *auth.Principal, renamed string) *auth.Principal {
				p.app.Authz.Rechecking = func(tx *sql.Tx) { tx.Exec("UPDATE users SET role='member' WHERE id=?", p.admin.User.ID) }
				return p.admin
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			folder, elsewhere := t.TempDir(), t.TempDir()
			p := newPagesWith(t, []docker.MountPoint{{Type: "bind", Source: folder, Destination: "/config", RW: true}})
			member := p.user("u-friend", "friend", db.RoleMember)
			p.grant(member, authz.ServerView, authz.ServerRestart, authz.SchedulesManage)
			if _, err := p.app.Schedules.Create(ctx, &member.User, member.TokenHash, p.server, restart); err != nil {
				t.Fatal(err)
			}
			context, err := p.app.Servers.ResolveAuthorized(ctx, &p.admin.User, p.server, authz.BackupsRead, -1)
			if err != nil {
				t.Fatal(err)
			}
			roots, _ := json.Marshal(backups.CurrentRoots(context.Container.FileRoots))
			p.backup(t, backupID, p.server, "")
			p.app.DB.Exec("UPDATE backups SET roots_json=?", string(roots))
			p.app.DB.Exec(`INSERT INTO operations (id,server_id,actor_id,kind,status,phase,input_json,binding_revision,created_at,updated_at)
        VALUES (?,?,?,'restart','succeeded','done','{}',1,1,1)`, operationID, p.server, member.User.ID)
			rename := url.Values{"name": {"valheim-world"}}
			if test.data {
				rename.Set("mount", "/config="+elsewhere)
			}
			p.engine.Recreate(p.docker, rename)
			if _, err := p.app.Servers.RefreshNow(ctx); err != nil {
				t.Fatal(err)
			}
			var renamed string
			p.app.DB.QueryRow("SELECT id FROM logical_servers WHERE id<>? AND status='active'", p.server).Scan(&renamed)
			suggested := "Looks like it was renamed to valheim-world"
			page := p.do("GET", "/servers/"+p.server, p.admin, nil, nil).Body.String()
			attention := p.do("GET", "/fragments/attention", p.admin, nil, map[string]string{"HX-Request": "true"}).Body.String()
			if renamed == "" || strings.Contains(page, suggested) == test.data || strings.Contains(attention, suggested) == test.data ||
				!strings.Contains(page, `value="`+renamed+`"`) {
				t.Fatal(renamed, page, attention)
			}
			who := p.admin
			if test.before != nil {
				who = test.before(t, p, member, renamed)
			}
			response := p.action("/servers/"+p.server+"/take-over", who, url.Values{"to": {renamed}})
			body := readBody(t, response)
			if response.StatusCode != test.status {
				t.Fatal(response.StatusCode, body)
			}
			var left, kept int
			p.app.DB.QueryRow("SELECT COUNT(*) FROM logical_servers WHERE id=?", p.server).Scan(&left)
			p.app.DB.QueryRow("SELECT COUNT(*) FROM backups WHERE server_id=?", p.server).Scan(&kept)
			if moved := test.status == http.StatusOK; moved != (left == 0) || moved != (kept == 0) {
				t.Fatal(left, kept)
			}
			if test.check != nil {
				test.check(t, p, member, p.server, renamed, body)
			}
		})
	}
}
