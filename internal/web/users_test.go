package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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
	operator := p.user("u-operator", "operator", db.RoleMember)
	if response := p.do("GET", "/users", operator, nil, nil); response.Code != http.StatusSeeOther {
		t.Fatal(response.Code)
	}
	form := url.Values{"_csrf": {CSRFToken(operator.Token)}, "username": {"intruder"}, "password": {"a-long-enough-password"}, "role": {"admin"}}
	if response := p.do("POST", "/users", operator, strings.NewReader(form.Encode()), map[string]string{"HX-Request": "true"}); response.Code != http.StatusForbidden {
		t.Fatal(response.Code)
	}
}

// Requests the Sharing tab never sends: a member, even one with full
// access, can't share or change access, and an administrator can't give a
// member what only administrators do.
func TestSharingRefusesForgedRequests(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	ctx := context.Background()
	member := p.user("u-member", "member", db.RoleMember)
	other := p.user("u-other", "other", db.RoleMember)
	p.grant(member, authz.LevelFull.Capabilities()...)
	for _, target := range []string{"/servers/" + p.server + "/sharing", "/servers/" + p.server + "/sharing/" + other.User.ID} {
		form := url.Values{"_csrf": {CSRFToken(member.Token)}, "person": {other.User.ID}, "level": {"full"}}
		if response := p.do("POST", target, member, strings.NewReader(form.Encode()), map[string]string{"HX-Request": "true"}); response.Code != http.StatusForbidden {
			t.Fatal(target, response.Code)
		}
	}
	if page := p.do("GET", "/servers/"+p.server+"?tab=sharing", member, nil, nil); strings.Contains(page.Body.String(), "Share with someone") {
		t.Fatal("member offered sharing")
	}
	for _, form := range []url.Values{
		{"person": {other.User.ID}, "level": {"custom"}, "capability": {"server.view", "console.shell"}},
		{"person": {other.User.ID}, "level": {"custom"}, "capability": {"backups.restore"}},
		{"person": {other.User.ID}, "level": {"admin"}},
		{"person": {other.User.ID}, "level": {"custom"}},
		{"person": {p.admin.User.ID}, "level": {"full"}},
	} {
		if result := p.adminPost("/servers/"+p.server+"/sharing", form); result.Code < 400 || !strings.Contains(result.Body, "alert--error") {
			t.Fatal(form, result.Code)
		}
	}
	if result := p.adminPost("/servers/"+p.server+"/sharing/"+member.User.ID, url.Values{"level": {"custom"},
		"capability": {"server.view", "server.update"}}); result.Code < 400 {
		t.Fatal(result.Code)
	}
	if grants, _ := p.app.Authz.UserGrants(ctx, other.User.ID); len(grants) != 0 {
		t.Fatal(grants)
	}
	if grants, _ := p.app.Authz.UserGrants(ctx, member.User.ID); len(grants) != 1 || authz.LevelOf(grants[0].Capabilities) != authz.LevelFull {
		t.Fatal(grants)
	}
	if self := p.adminPost("/users/"+p.admin.User.ID+"/delete", url.Values{}); self.Code < 400 {
		t.Fatal("deleted own account", self.Code)
	}
}

// Inviting someone new makes their account and access together: a refused
// access leaves no account behind.
func TestInviteAndShareIsAtomic(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	send := func(form url.Values) *httptest.ResponseRecorder {
		form.Set("_csrf", CSRFToken(p.admin.Token))
		form.Set("person", "new")
		return p.do("POST", "/servers/"+p.server+"/sharing", p.admin, strings.NewReader(form.Encode()),
			map[string]string{"HX-Request": "true", "Origin": "https://ludock.example", "Sec-Fetch-Site": "same-origin"})
	}
	refused := send(url.Values{"username": {"newbie"}, "level": {"custom"}, "capability": {"server.view", "console.shell"}})
	if refused.Code != http.StatusBadRequest {
		t.Fatal(refused.Code, refused.Body.String())
	}
	if user, _ := p.app.DB.FindUserByUsername(context.Background(), "newbie"); user != nil {
		t.Fatal("account made without its access")
	}
	shared := send(url.Values{"username": {"newbie"}, "level": {"see"}})
	user, _ := p.app.DB.FindUserByUsername(context.Background(), "newbie")
	if shared.Code != http.StatusOK || user == nil || user.Role != db.RoleMember || !strings.Contains(shared.Body.String(), `value="https://ludock.example/link/`) {
		t.Fatal(shared.Code, user, shared.Body.String())
	}
	if grants, _ := p.app.Authz.UserGrants(context.Background(), user.ID); len(grants) != 1 || authz.LevelOf(grants[0].Capabilities) != authz.LevelSee {
		t.Fatal(grants)
	}
}

// A custom choice includes seeing the server and reading files it changes;
// values the form never offers pass through, to be refused when saved.
func TestShareCapabilitiesNormalize(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		level  authz.Level
		custom []string
		want   []authz.Capability
		ok     bool
	}{
		{authz.LevelSee, []string{"console.shell"}, []authz.Capability{authz.ServerView}, true},
		{authz.LevelCustom, []string{"files.write"}, []authz.Capability{authz.ServerView, authz.FilesRead, authz.FilesWrite}, true},
		{authz.LevelCustom, []string{"console.shell"}, []authz.Capability{authz.ServerView, authz.ConsoleShell}, true},
		{authz.LevelCustom, []string{"bogus"}, []authz.Capability{authz.ServerView, "bogus"}, true},
		{authz.LevelCustom, nil, nil, false},
		{"admin", nil, nil, false},
		{authz.LevelNone, nil, nil, false},
	} {
		got, ok := views.ShareCapabilities(c.level, c.custom)
		if ok != c.ok || !slices.Equal(got, c.want) {
			t.Fatal(c.level, c.custom, got, ok)
		}
	}
}

// A link is built from the browser's Origin, never from the Host header,
// and isn't made at all when the request names no origin.
func TestLinkUsesTheBrowserOrigin(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	target := p.user("u-target", "target", db.RoleMember)
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
	target := p.user("u-target", "target", db.RoleMember)
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
	first := p.user("u-first", "first", db.RoleMember)
	second := p.user("u-second", "second", db.RoleMember)
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

// A link made from an address only this computer, or only its network,
// reaches warns that the person it's for may not be able to open it.
func TestLinkWarnsOfAnAddressFriendsCantReach(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		host              string
		computer, network bool
	}{
		{"127.0.0.1:3000", true, true}, {"localhost:3000", true, true}, {"[::1]:3000", true, true},
		{"192.168.1.20:3000", false, true}, {"nas.lan", false, true}, {"nas:3000", false, true},
		{"ludock.example.com", false, false}, {"203.0.113.7:3000", false, false},
	} {
		request := httptest.NewRequest("POST", "http://"+test.host+"/users", nil)
		request.Header.Set("Origin", "http://"+test.host)
		request.Header.Set("Sec-Fetch-Site", "same-origin")
		link := linkView(request, "dana", &auth.Link{Token: "token", Purpose: db.LinkInvite})
		if link.ThisComputer != test.computer || link.LocalNetwork != test.network {
			t.Errorf("%s: this computer %v, local network %v", test.host, link.ThisComputer, link.LocalNetwork)
		}
	}
}
