package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/web/views"
)

func (p *pages) adminPost(target string, values url.Values) *postResult {
	values.Set("_csrf", CSRFToken(p.admin.Token))
	response := p.do("POST", target, p.admin, strings.NewReader(values.Encode()), map[string]string{"HX-Request": "true"})
	return &postResult{response.Code, response.Body.String()}
}

type postResult struct {
	Code int
	Body string
}

func TestUsersPageNeedsAnAdministrator(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleOperator)
	if response := p.do("GET", "/users", operator, nil, nil); response.Code != http.StatusSeeOther {
		t.Fatal(response.Code)
	}
	form := url.Values{"_csrf": {CSRFToken(operator.Token)}, "username": {"intruder"}, "password": {"a-long-enough-password"}, "role": {"admin"}}
	if response := p.do("POST", "/users", operator, strings.NewReader(form.Encode()), map[string]string{"HX-Request": "true"}); response.Code != http.StatusForbidden {
		t.Fatal(response.Code)
	}
}

func TestGrantsStayWithinRoleCeiling(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	viewer := p.user("u-viewer", "viewer", db.RoleViewer)
	tooMuch := p.adminPost("/users/"+viewer.User.ID+"/grants", url.Values{"grant:" + p.server: {"server.view", "server.start"}})
	if !strings.Contains(tooMuch.Body, "alert--error") {
		t.Fatal(tooMuch.Body)
	}
	if grants, _ := p.app.Authz.UserGrants(context.Background(), viewer.User.ID); len(grants) != 0 {
		t.Fatal(grants)
	}
	if self := p.adminPost("/users/"+p.admin.User.ID+"/delete", url.Values{}); self.Code < 400 {
		t.Fatal("deleted own account", self.Code)
	}
}

func TestGrantInputsNormalize(t *testing.T) {
	t.Parallel()
	inputs := views.GrantInputs(map[string][]string{
		"grant:a": {"files.write"},                // no view: no access
		"grant:b": {"server.view", "files.write"}, // writing implies reading
		"grant:c": {"server.view", "console.shell"},
		"other":   {"server.view"},
	})
	if len(inputs) != 2 || inputs[0].ServerID != "b" || len(inputs[0].Capabilities) != 3 || inputs[1].ServerID != "c" ||
		len(inputs[1].Capabilities) != 1 || inputs[0].Capabilities[2] != authz.FilesWrite {
		t.Fatalf("%+v", inputs)
	}
}
