package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/auth"
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

// A link is built from the browser's Origin, never from the Host header,
// and isn't made at all when the request names no origin.
func TestLinkUsesTheBrowserOrigin(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	target := p.user("u-target", "target", db.RoleViewer)
	form := url.Values{"_csrf": {CSRFToken(p.admin.Token)}}.Encode()
	send := func(headers map[string]string) string {
		request := httptest.NewRequest("POST", "http://panel.example/users/"+target.User.ID+"/link", strings.NewReader(form))
		request.Host = "attacker.example"
		request.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: p.admin.Token})
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("HX-Request", "true")
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response := httptest.NewRecorder()
		p.handler.ServeHTTP(response, request)
		return response.Body.String()
	}
	if body := send(nil); !strings.Contains(body, "couldn&#39;t tell which address") || strings.Contains(body, "/link/") {
		t.Fatal(body)
	}
	body := send(map[string]string{"Origin": "https://ludock.example", "Sec-Fetch-Site": "same-origin"})
	if !strings.Contains(body, `value="https://ludock.example/link/`) || strings.Contains(body, "attacker.example") {
		t.Fatal(body)
	}
}

// Opening a link moves its token into a cookie scoped to the link page, and
// the response keeps the token out of referrers, caches, and request logs.
func TestOpeningALinkHidesItsToken(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	target := p.user("u-target", "target", db.RoleViewer)
	link, err := p.app.Auth.CreateLink(context.Background(), p.admin, target.User.ID, auth.Client{})
	if err != nil {
		t.Fatal(err)
	}
	response := p.do("GET", "/link/"+link.Token, nil, nil, nil)
	header := response.Header()
	if response.Code != http.StatusSeeOther || header.Get("Location") != "/link" || header.Get("Referrer-Policy") != "no-referrer" ||
		header.Get("Cache-Control") != "no-store" {
		t.Fatal(response.Code, header)
	}
	cookie := response.Result().Cookies()[0]
	if cookie.Name != auth.LinkCookie || cookie.Value != link.Token || cookie.Path != "/link" || !cookie.HttpOnly {
		t.Fatal(cookie)
	}
	request := httptest.NewRequest("GET", "http://panel.example/link", nil)
	request.AddCookie(cookie)
	page := httptest.NewRecorder()
	p.handler.ServeHTTP(page, request)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Choose a new password") || strings.Contains(page.Body.String(), link.Token) {
		t.Fatal(page.Code, page.Body.String())
	}
	for _, path := range []string{"/link/" + link.Token, "//link/" + link.Token, "/link/" + link.Token + "/.."} {
		if logged := loggedPath(httptest.NewRequest("GET", "http://panel.example"+path, nil)); strings.Contains(logged, link.Token) {
			t.Fatal(logged)
		}
	}
	if unknown := p.do("GET", "/link/unknown-token", nil, nil, nil); unknown.Code != http.StatusNotFound ||
		!strings.Contains(unknown.Body.String(), "This link doesn't work") {
		t.Fatal(unknown.Code, unknown.Body.String())
	}
}

// A link form left open in one tab can't set a password once the browser
// opened another link, which replaced the cookie.
func TestLinkFormBelongsToItsLink(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	first := p.user("u-first", "first", db.RoleViewer)
	second := p.user("u-second", "second", db.RoleViewer)
	firstLink, _ := p.app.Auth.CreateLink(context.Background(), p.admin, first.User.ID, auth.Client{})
	secondLink, _ := p.app.Auth.CreateLink(context.Background(), p.admin, second.User.ID, auth.Client{})
	submit := func(key string) *httptest.ResponseRecorder {
		form := url.Values{"link": {key}, "password": {"a new long password"}, "confirmation": {"a new long password"}}
		request := httptest.NewRequest("POST", "http://panel.example/link", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.AddCookie(&http.Cookie{Name: auth.LinkCookie, Value: secondLink.Token})
		response := httptest.NewRecorder()
		p.handler.ServeHTTP(response, request)
		return response
	}
	if response := submit(linkFormKey(firstLink.Token)); response.Code != http.StatusConflict || len(response.Result().Cookies()) != 0 {
		t.Fatal(response.Code, response.Result().Cookies(), response.Body.String())
	}
	if response := submit(linkFormKey(secondLink.Token)); response.Code != http.StatusSeeOther {
		t.Fatal(response.Code, response.Body.String())
	}
	if _, err := p.app.Auth.Login(context.Background(), "first", "a new long password", auth.Client{}); err == nil {
		t.Fatal("first account's password changed")
	}
}
