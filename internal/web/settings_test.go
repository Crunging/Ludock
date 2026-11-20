package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/db"
)

func TestSettingsNeedAnAdministrator(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleMember)
	for _, target := range []string{"/settings", "/settings/system"} {
		if response := p.do("GET", target, operator, nil, nil); response.Code != http.StatusSeeOther {
			t.Fatal(target, response.Code)
		}
	}
	form := url.Values{"_csrf": {CSRFToken(operator.Token)}, "host": {"evil.example"}}
	for _, target := range []string{"/settings/connection", "/settings/ludock-check"} {
		if response := p.do("POST", target, operator, strings.NewReader(form.Encode()), map[string]string{"HX-Request": "true"}); response.Code != http.StatusForbidden {
			t.Fatal(target, response.Code)
		}
	}
}

// Advanced settings are admin-only writes: an operator, or an administrator's
// request without the CSRF token, changes nothing.
func TestAdvancedSettingsNeedAnAdministrator(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleMember)
	for target, field := range map[string]string{"/settings/upload-limit": "limit", "/settings/logging": "verbose",
		"/settings/proxies": "trusted"} {
		value := map[string]string{"limit": "1 MB", "verbose": "true", "trusted": "0.0.0.0/0"}[field]
		form := url.Values{"_csrf": {CSRFToken(operator.Token)}, field: {value}}
		if response := p.do("POST", target, operator, strings.NewReader(form.Encode()), map[string]string{"HX-Request": "true"}); response.Code != http.StatusForbidden {
			t.Error(target, "operator", response.Code)
		}
		form = url.Values{field: {value}}
		if response := p.do("POST", target, p.admin, strings.NewReader(form.Encode()), map[string]string{"HX-Request": "true"}); response.Code != http.StatusForbidden {
			t.Error(target, "no csrf", response.Code)
		}
	}
	ctx := context.Background()
	if limit, _ := p.app.Advanced.UploadLimit(ctx); limit != 0 {
		t.Fatal("upload limit changed", limit)
	}
	if p.app.Advanced.Logging(ctx).VerboseUntil != 0 || p.app.Advanced.Proxies(ctx).Trusted != "" {
		t.Fatal("settings changed")
	}
}

// Trusted proxies saved in the panel are read exactly as
// LUDOCK_TRUSTED_PROXIES: invalid lists are refused when saved, and a saved
// proxy may supply the client address from then on.
func TestTrustedProxiesFromThePanel(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	save := func(value string) int {
		form := url.Values{"_csrf": {CSRFToken(p.admin.Token)}, "trusted": {value}}
		return p.do("POST", "/settings/proxies", p.admin, strings.NewReader(form.Encode()), map[string]string{"HX-Request": "true"}).Code
	}
	proxied := httptest.NewRequest("GET", "http://panel.example/", nil)
	proxied.RemoteAddr = "172.18.0.2:41000"
	proxied.Header.Set("X-Forwarded-For", "203.0.113.9")
	throughProxy := func() string { return p.app.ClientIP.Request(proxied) }
	if got := throughProxy(); got != "172.18.0.2" {
		t.Fatal("untrusted proxy", got)
	}
	if peer, ok := p.app.ClientIP.UntrustedProxy(proxied); !ok || peer != "172.18.0.2" {
		t.Fatal("proxy not noticed", peer, ok)
	}
	// One click trusts only the proxy the request came through: this one came
	// straight from a public address.
	form := url.Values{"_csrf": {CSRFToken(p.admin.Token)}, "trust": {"192.0.2.1"}}
	forged := p.do("POST", "/settings/proxies", p.admin, strings.NewReader(form.Encode()), map[string]string{"HX-Request": "true", "X-Forwarded-For": "198.51.100.7"})
	if forged.Code != http.StatusConflict || strings.Contains(forged.Body.String(), `value="192.0.2.1"`) {
		t.Fatal("forged trust", forged.Code)
	}
	for _, invalid := range []string{"proxy.example", "172.18.0.2:80", "172.18.0.0/33", "172.18.0.2,,10.0.0.1", "010.0.0.1"} {
		if code := save(invalid); code != http.StatusBadRequest {
			t.Error(invalid, code)
		}
	}
	if got := p.app.Advanced.Proxies(context.Background()).Trusted; got != "" {
		t.Fatal("invalid value saved", got)
	}
	if code := save(" 172.18.0.2 ,2001:db8::/32"); code != http.StatusOK {
		t.Fatal(code)
	}
	if got := p.app.Advanced.Proxies(context.Background()).Trusted; got != "172.18.0.2, 2001:db8::/32" {
		t.Fatal(got)
	}
	if got := throughProxy(); got != "203.0.113.9" {
		t.Fatal("trusted proxy", got)
	}
	if _, ok := p.app.ClientIP.UntrustedProxy(proxied); ok {
		t.Fatal("hint kept after trusting a proxy")
	}
	var count int
	if err := p.app.DB.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action='settings.trusted_proxies.updated'").Scan(&count); err != nil || count != 1 {
		t.Fatal("audit", count, err)
	}
	if code := save(""); code != http.StatusOK {
		t.Fatal(code)
	}
	if got := throughProxy(); got != "172.18.0.2" {
		t.Fatal("proxy still trusted", got)
	}
}

// A value set in the environment wins over a saved one, and the panel shows
// it instead of a field.
func TestAdvancedSettingsEnvironmentWins(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	form := url.Values{"_csrf": {CSRFToken(p.admin.Token)}, "limit": {"10 MB"}}
	if code := p.do("POST", "/settings/upload-limit", p.admin, strings.NewReader(form.Encode()), map[string]string{"HX-Request": "true"}).Code; code != http.StatusOK {
		t.Fatal(code)
	}
	ctx := context.Background()
	if limit, fixed := p.app.Advanced.UploadLimit(ctx); limit != 10_000_000 || fixed {
		t.Fatal(limit, fixed)
	}
	// The field shows a value that saves back unchanged.
	if page := p.do("GET", "/settings", p.admin, nil, nil).Body.String(); !strings.Contains(page, `value="10 MB"`) {
		t.Fatal("limit not shown exactly")
	}
	p.app.Advanced.Env.MaxUploadBytes = 1 << 30
	p.app.Advanced.Env.LogLevel = "warn"
	p.app.Advanced.Env.TrustedProxies, p.app.Advanced.Env.TrustedProxiesSet = "10.0.0.1", true
	if limit, fixed := p.app.Advanced.UploadLimit(ctx); limit != 1<<30 || !fixed {
		t.Fatal(limit, fixed)
	}
	page := p.do("GET", "/settings", p.admin, nil, nil).Body.String()
	for _, want := range []string{"<code>LUDOCK_MAX_UPLOAD_SIZE</code>", "<code>LUDOCK_LOG_LEVEL</code>", "<code>LUDOCK_TRUSTED_PROXIES</code>", "1 GiB", "<code>10.0.0.1</code>"} {
		if !strings.Contains(page, want) {
			t.Error("missing", want)
		}
	}
	if strings.Contains(page, `name="limit"`) || strings.Contains(page, `name="trusted"`) || strings.Contains(page, `name="verbose"`) {
		t.Error("fixed settings are editable")
	}
	form.Set("limit", "")
	if code := p.do("POST", "/settings/upload-limit", p.admin, strings.NewReader(form.Encode()), map[string]string{"HX-Request": "true"}).Code; code != http.StatusConflict {
		t.Fatal(code)
	}
}
