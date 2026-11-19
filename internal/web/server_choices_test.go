package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
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
