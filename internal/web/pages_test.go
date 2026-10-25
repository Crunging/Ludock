package web

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/app"
	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/config"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
)

type pages struct {
	t       *testing.T
	engine  *fakedocker.Engine
	app     *app.App
	web     *Server
	handler http.Handler
	server  string // logical ID of the stopped "valheim" container
	docker  string
	admin   *auth.Principal
}

func newPages(t *testing.T) *pages {
	t.Helper()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	dockerID := engine.AddContainer(fakedocker.GameOptions{Name: "valheim", Image: "lloesche/valheim-server"})
	d := dbtest.Open(t)
	services, err := app.New(config.Config{DockerSocket: engine.Socket}, d, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Servers.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := &pages{t: t, engine: engine, app: services, docker: dockerID}
	p.web = &Server{App: services, DB: d, Auth: services.Auth, ClientIP: services.ClientIP,
		Docker: func(context.Context) error { return nil }}
	p.handler = p.web.Handler()
	admin := p.user("u-admin", "admin", db.RoleAdmin)
	views, err := services.Servers.List(context.Background(), &admin.User)
	if err != nil || len(views) != 1 {
		t.Fatal(views, err)
	}
	p.server = views[0].ID
	p.admin = admin
	return p
}

// user creates an account and returns a signed-in session.
func (p *pages) user(id, username string, role db.Role) *auth.Principal {
	p.t.Helper()
	ctx := context.Background()
	hash, _ := auth.Argon2{}.Hash("password-" + id)
	if err := p.app.DB.CreateUser(ctx, db.UserRecord{User: db.User{ID: id, Username: username, Role: role, CreatedAt: 1},
		PasswordHash: hash}); err != nil {
		p.t.Fatal(err)
	}
	principal, err := p.app.Auth.Login(ctx, username, "password-"+id, auth.Client{})
	if err != nil {
		p.t.Fatal(err)
	}
	return principal
}

func (p *pages) grant(principal *auth.Principal, capabilities ...authz.Capability) {
	p.t.Helper()
	if _, err := p.app.Authz.SetUserGrants(context.Background(), principal.User.ID,
		[]authz.GrantInput{{ServerID: p.server, Capabilities: capabilities}}, &p.admin.User, p.admin.TokenHash); err != nil {
		p.t.Fatal(err)
	}
}

func (p *pages) do(method, target string, principal *auth.Principal, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://panel.example"+target, body)
	if principal != nil {
		request.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: principal.Token})
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	p.handler.ServeHTTP(response, request)
	return response
}

func (p *pages) post(target string, principal *auth.Principal, csrf string) *httptest.ResponseRecorder {
	return p.do("POST", target, principal, strings.NewReader(url.Values{"_csrf": {csrf}}.Encode()), map[string]string{"HX-Request": "true"})
}

func (p *pages) running() bool {
	return p.engine.Container(p.docker).State.Running
}

func TestPagesRequireASession(t *testing.T) {
	p := newPages(t)
	response := p.do("GET", "/fragments/servers", nil, nil, map[string]string{"HX-Request": "true"})
	if response.Code != http.StatusUnauthorized || response.Header().Get("HX-Redirect") != "/login" {
		t.Fatal(response.Code, response.Header())
	}
	response = p.post("/servers/"+p.server+"/start", nil, "")
	if response.Code != http.StatusUnauthorized || p.running() {
		t.Fatal(response.Code, p.running())
	}
}

func TestActionsRequireTheSessionCSRFToken(t *testing.T) {
	p := newPages(t)
	admin, err := p.app.Auth.Login(context.Background(), "admin", "password-u-admin", auth.Client{})
	if err != nil {
		t.Fatal(err)
	}
	other := p.user("u-other", "other", db.RoleAdmin)
	for _, csrf := range []string{"", "not-a-token", CSRFToken(other.Token)} {
		if response := p.post("/servers/"+p.server+"/start", admin, csrf); response.Code != http.StatusForbidden || p.running() {
			t.Fatalf("csrf %q: %d running=%v", csrf, response.Code, p.running())
		}
	}
	// The header form is accepted as well as the field.
	response := p.do("POST", "/servers/"+p.server+"/start", admin, strings.NewReader(""),
		map[string]string{"HX-Request": "true", "X-CSRF-Token": CSRFToken(admin.Token)})
	if response.Code != http.StatusOK || response.Header().Get("HX-Trigger") != "servers-changed" || !p.running() {
		t.Fatal(response.Code, response.Header(), response.Body.String())
	}
}

// Pages are compressed, so each one masks the CSRF token differently. A
// token read from a page works; another session's token fails masked too.
func TestCSRFTokenMaskedPerPage(t *testing.T) {
	p := newPages(t)
	other := p.user("u-other", "other", db.RoleAdmin)
	pageToken := func(principal *auth.Principal) string {
		response := p.do("GET", "/", principal, nil, map[string]string{"Accept-Encoding": "gzip"})
		if response.Header().Get("Content-Encoding") != "gzip" {
			t.Fatal("page is not compressed", response.Header())
		}
		reader, err := gzip.NewReader(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		page, _ := io.ReadAll(reader)
		token := regexp.MustCompile(`<meta name="csrf-token" content="([0-9a-f]+)"`).FindSubmatch(page)
		if token == nil {
			t.Fatal("page has no CSRF token")
		}
		return string(token[1])
	}
	first, second := pageToken(p.admin), pageToken(p.admin)
	if first == second || first == CSRFToken(p.admin.Token) {
		t.Fatal("pages share a token", first, second)
	}
	if response := p.post("/servers/"+p.server+"/start", p.admin, pageToken(other)); response.Code != http.StatusForbidden || p.running() {
		t.Fatal("another session's masked token", response.Code, p.running())
	}
	if response := p.post("/servers/"+p.server+"/start", p.admin, second); response.Code != http.StatusOK || !p.running() {
		t.Fatal(response.Code, response.Body.String())
	}
}

func TestFormsRejectMultipartBodies(t *testing.T) {
	p := newPages(t)
	admin, err := p.app.Auth.Login(context.Background(), "admin", "password-u-admin", auth.Client{})
	if err != nil {
		t.Fatal(err)
	}
	body := "--x\r\nContent-Disposition: form-data; name=\"_csrf\"\r\n\r\n" + CSRFToken(admin.Token) + "\r\n--x--\r\n"
	for _, target := range []string{"/login", "/servers/" + p.server + "/start"} {
		response := p.do("POST", target, admin, strings.NewReader(body), map[string]string{"Content-Type": "multipart/form-data; boundary=x"})
		if response.Code != http.StatusUnsupportedMediaType || p.running() {
			t.Fatalf("%s: %d running=%v", target, response.Code, p.running())
		}
	}
}

func TestFailedSignOutKeepsTheCookie(t *testing.T) {
	p := newPages(t)
	admin, err := p.app.Auth.Login(context.Background(), "admin", "password-u-admin", auth.Client{})
	if err != nil {
		t.Fatal(err)
	}
	// The audit entry commits with the deletion, so its failure keeps the
	// session too.
	if _, err := p.app.DB.Exec(`CREATE TRIGGER no_logout_audit BEFORE INSERT ON audit_log WHEN NEW.action = 'auth.logout'
		BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	logout := func() *httptest.ResponseRecorder {
		return p.do("POST", "/logout", admin, strings.NewReader(url.Values{"_csrf": {CSRFToken(admin.Token)}}.Encode()), nil)
	}
	response := logout()
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Header().Get("Set-Cookie"), auth.SessionCookie) {
		t.Fatal(response.Code, response.Header())
	}
	if principal, _ := p.app.Auth.Session(context.Background(), admin.Token); principal == nil {
		t.Fatal("failed sign-out ended the session")
	}
}

func TestDiscoveryIsNotPermission(t *testing.T) {
	p := newPages(t)
	viewer := p.user("u-viewer", "viewer", db.RoleViewer)
	page := p.do("GET", "/fragments/servers", viewer, nil, map[string]string{"HX-Request": "true"})
	if page.Code != http.StatusOK || strings.Contains(page.Body.String(), "valheim") {
		t.Fatal(page.Code, page.Body.String())
	}
	// An unknown server and an ungranted one look the same.
	missing := p.post("/servers/srv_missing/start", viewer, CSRFToken(viewer.Token))
	hidden := p.post("/servers/"+p.server+"/start", viewer, CSRFToken(viewer.Token))
	if hidden.Code != http.StatusNotFound || missing.Code != hidden.Code || missing.Body.String() != hidden.Body.String() || p.running() {
		t.Fatal(hidden.Code, hidden.Body.String(), missing.Body.String())
	}

	operator := p.user("u-operator", "operator", db.RoleOperator)
	p.grant(operator, authz.ServerView)
	page = p.do("GET", "/fragments/servers", operator, nil, map[string]string{"HX-Request": "true"})
	if !strings.Contains(page.Body.String(), "valheim") || strings.Contains(page.Body.String(), "/servers/"+p.server+"/start") {
		t.Fatal(page.Body.String())
	}
	if response := p.post("/servers/"+p.server+"/start", operator, CSRFToken(operator.Token)); response.Code != http.StatusForbidden || p.running() {
		t.Fatal(response.Code, response.Body.String())
	}
	p.grant(operator, authz.ServerView, authz.ServerStart)
	if response := p.post("/servers/"+p.server+"/start", operator, CSRFToken(operator.Token)); response.Code != http.StatusOK || !p.running() {
		t.Fatal(response.Code, response.Body.String())
	}
}

// revokingBody signs the session out while the form is still arriving.
type revokingBody struct {
	io.Reader
	revoke func()
}

func (b *revokingBody) Read(buffer []byte) (int, error) {
	if b.revoke != nil {
		b.revoke()
		b.revoke = nil
	}
	return b.Reader.Read(buffer)
}

// Session revoked during the body stops the action.
func TestSessionRevokedDuringBody(t *testing.T) {
	p := newPages(t)
	admin, err := p.app.Auth.Login(context.Background(), "admin", "password-u-admin", auth.Client{})
	if err != nil {
		t.Fatal(err)
	}
	body := &revokingBody{Reader: strings.NewReader(url.Values{"_csrf": {CSRFToken(admin.Token)}}.Encode()), revoke: func() {
		if err := p.app.Auth.Logout(context.Background(), admin.Token, auth.Client{}); err != nil {
			t.Error(err)
		}
	}}
	response := p.do("POST", "/servers/"+p.server+"/start", admin, body, map[string]string{"HX-Request": "true"})
	if response.Code != http.StatusUnauthorized || p.running() {
		t.Fatal(response.Code, p.running())
	}
}
