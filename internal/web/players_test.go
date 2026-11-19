package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
)

// playerGame is a running Minecraft server whose RCON console records every
// command it receives.
type playerGame struct {
	*pages
	server string
	mu     sync.Mutex
	sent   []string
}

func newPlayerGame(t *testing.T) *playerGame {
	t.Helper()
	g := &playerGame{pages: newPages(t)}
	port, err := fakedocker.ServeRCON("rcon-secret", func(command string) string {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.sent = append(g.sent, command)
		if strings.HasPrefix(command, "kick ") {
			return "Kicked " + strings.TrimPrefix(command, "kick ") + ": Kicked by an operator"
		}
		return "Server says rcon-secret in an odd reply"
	})
	if err != nil {
		t.Fatal(err)
	}
	g.engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true,
		Labels: map[string]string{"ludock.console.host": "127.0.0.1", "ludock.console.port": fmt.Sprint(port)},
		Env:    []string{"RCON_PASSWORD=rcon-secret"}})
	if _, err := g.app.Servers.RefreshNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	views, err := g.app.Servers.List(context.Background(), &g.admin.User)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		if view.Name == "mc" {
			g.server = view.ID
		}
	}
	if g.server == "" {
		t.Fatal("no Minecraft server", views)
	}
	return g
}

func (g *playerGame) act(principal *auth.Principal, action, player string) *http.Response {
	body := url.Values{"_csrf": {CSRFToken(principal.Token)}, "player": {player}}.Encode()
	return g.do("POST", "/servers/"+g.server+"/players/"+action, principal, strings.NewReader(body),
		map[string]string{"HX-Request": "true"}).Result()
}

func (g *playerGame) commands() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.sent...)
}

func (g *playerGame) grantOn(principal *auth.Principal, capabilities ...authz.Capability) {
	g.t.Helper()
	if _, err := g.app.Authz.SetUserGrants(context.Background(), principal.User.ID,
		[]authz.GrantInput{{ServerID: g.server, Capabilities: capabilities}}, &g.admin.User, g.admin.TokenHash); err != nil {
		g.t.Fatal(err)
	}
}

// Someone who may see a server but not use its console can't act on its
// players, and a request with a name the game doesn't take, or an action it
// doesn't have, sends nothing at all.
func TestPlayerActionRefusesForgedRequests(t *testing.T) {
	t.Parallel()
	g := newPlayerGame(t)
	viewer := g.user("u-viewer", "viewer", db.RoleOperator)
	g.grantOn(viewer, authz.ServerView, authz.LogsRead)
	if response := g.act(viewer, "kick", "Ada"); response.StatusCode < 400 {
		t.Fatal(response.StatusCode)
	}
	if response := g.do("GET", "/fragments/servers/"+g.server+"/player-lists", viewer, nil,
		map[string]string{"HX-Request": "true"}); response.Code < 400 {
		t.Fatal(response.Code)
	}

	operator := g.user("u-operator", "operator", db.RoleOperator)
	g.grantOn(operator, authz.ServerView, authz.ConsoleExecute)
	for _, name := range []string{"", "Ada\nop Eve", "Ada\rop Eve", "Ada op", "@a", "@a[name=Ada]", `"Ada"`, "Ada;stop",
		"Ada\x00", " Ada", "Ada ", "Ada\t", "Åda", "Ada op", "SeventeenLetters1", "Ada|op", "Ada&&stop", "Ada/op"} {
		if response := g.act(operator, "kick", name); response.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: %d", name, response.StatusCode)
		}
	}
	for _, action := range []string{"op", "deop", "stop", "KICK", "kick%20Ada"} {
		if response := g.act(operator, action, "Ada"); response.StatusCode < 400 {
			t.Errorf("%s: %d", action, response.StatusCode)
		}
	}
	if sent := g.commands(); len(sent) != 0 {
		t.Fatal(sent)
	}
}

// An action sends exactly the game's one command, audits who did what to
// whom without the game's reply, and shows the reply redacted.
func TestPlayerActionSendsOneCommand(t *testing.T) {
	t.Parallel()
	g := newPlayerGame(t)
	operator := g.user("u-operator", "operator", db.RoleOperator)
	g.grantOn(operator, authz.ServerView, authz.ConsoleExecute)
	read := func(response *http.Response) string {
		var page strings.Builder
		io.Copy(&page, response.Body)
		return page.String()
	}
	response := g.act(operator, "kick", "Ada_2")
	if page := read(response); response.StatusCode != http.StatusOK || !strings.Contains(page, "Ada_2 was kicked.") {
		t.Fatal(response.StatusCode, page)
	}
	// A reply Ludock doesn't recognize is shown, without the console password.
	response = g.act(operator, "ban", "Ada")
	if page := read(response); response.StatusCode != http.StatusOK || !strings.Contains(page, "Asked the game to ban Ada.") ||
		!strings.Contains(page, "[redacted]") || strings.Contains(page, "rcon-secret") {
		t.Fatal(response.StatusCode, page)
	}
	// Player readings, which follow each action, send "list".
	sent := slices.DeleteFunc(g.commands(), func(command string) bool { return command == "list" })
	if strings.Join(sent, "|") != "kick Ada_2|ban Ada" {
		t.Fatal(sent)
	}
	var details string
	if err := g.app.DB.QueryRow(`SELECT details_json FROM audit_log WHERE action = 'server.player-action.succeeded' AND user_id = ?
		ORDER BY id DESC LIMIT 1`, operator.User.ID).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, `"player":"Ada"`) || !strings.Contains(details, `"action":"ban"`) || strings.Contains(details, "odd") {
		t.Fatal(details)
	}
}

// When the audit entry can't be written, the command isn't sent.
func TestPlayerActionNeedsItsAuditEntry(t *testing.T) {
	t.Parallel()
	g := newPlayerGame(t)
	if _, err := g.app.DB.Exec(`CREATE TRIGGER no_player_audit BEFORE INSERT ON audit_log
		WHEN NEW.action = 'server.player-action.started' BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	if response := g.act(g.admin, "ban", "Ada"); response.StatusCode != http.StatusServiceUnavailable {
		t.Fatal(response.StatusCode)
	}
	if sent := g.commands(); len(sent) != 0 {
		t.Fatal(sent)
	}
}
