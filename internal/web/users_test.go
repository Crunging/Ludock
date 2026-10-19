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

func TestCreateUserAndShareAServer(t *testing.T) {
	p := newPages(t)
	created := p.adminPost("/users", url.Values{"username": {"friend"}, "password": {"a-long-enough-password"}, "role": {"viewer"}})
	if created.Code != http.StatusOK || !strings.Contains(created.Body, "Created friend.") || !strings.Contains(created.Body, `hx-trigger="load"`) {
		t.Fatal(created.Code, created.Body)
	}
	duplicate := p.adminPost("/users", url.Values{"username": {"Friend"}, "password": {"a-long-enough-password"}, "role": {"operator"}})
	if duplicate.Code < 400 || !strings.Contains(duplicate.Body, `value="Friend"`) || strings.Contains(duplicate.Body, "a-long-enough-password") {
		t.Fatal(duplicate.Code, duplicate.Body)
	}
	if self := p.adminPost("/users/"+p.admin.User.ID+"/delete", url.Values{}); self.Code < 400 {
		t.Fatal("deleted own account", self.Code)
	}
	user, _ := p.app.DB.FindUserByUsername(context.Background(), "friend")
	// A viewer cannot be given actions beyond their role.
	tooMuch := p.adminPost("/users/"+user.ID+"/grants", url.Values{"grant:" + p.server: {"server.view", "server.start"}})
	if !strings.Contains(tooMuch.Body, "alert--error") {
		t.Fatal(tooMuch.Body)
	}
	saved := p.adminPost("/users/"+user.ID+"/grants", url.Values{"grant:" + p.server: {"server.view", "logs.read"}})
	if !strings.Contains(saved.Body, "Server access saved for friend.") {
		t.Fatal(saved.Body)
	}
	grants, _ := p.app.Authz.UserGrants(context.Background(), user.ID)
	if len(grants) != 1 || len(grants[0].Capabilities) != 2 {
		t.Fatal(grants)
	}
	// Leaving every box empty removes the access.
	p.adminPost("/users/"+user.ID+"/grants", url.Values{})
	if grants, _ := p.app.Authz.UserGrants(context.Background(), user.ID); len(grants) != 0 {
		t.Fatal(grants)
	}
}

func TestGrantInputsNormalize(t *testing.T) {
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
