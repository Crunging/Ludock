package web

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/Crunging/Ludock/internal/auth"
)

// A crafted sign-in link can't send the browser to another site. Browsers
// drop tabs and newlines from a Location, so "/\t/evil.example" would become
// "//evil.example".
func TestLoginNextStaysOnSite(t *testing.T) {
	p := newPages(t)
	for next, want := range map[string]string{
		"/servers?view=list":    "/servers?view=list",
		"//evil.example":        "/",
		"/\\evil.example":       "/",
		"/\t/evil.example":      "/",
		"/\n/evil.example":      "/",
		"/\x00/evil.example":    "/",
		"https://evil.example/": "/",
		"/login?next=/x":        "/",
	} {
		response := p.do("GET", "/login?next="+url.QueryEscape(next), p.admin, nil, nil)
		if response.Code != http.StatusSeeOther || response.Header().Get("Location") != want {
			t.Errorf("next %q: %d %q, want %q", next, response.Code, response.Header().Get("Location"), want)
		}
	}
}

// Signing out over HTTPS clears the session cookie with the same Secure
// attribute it was set with.
func TestLogoutClearsSecureCookie(t *testing.T) {
	p := newPages(t)
	response := p.do("POST", "/logout", p.admin, nil, map[string]string{
		"X-Forwarded-Proto": "https", "Origin": "https://panel.example", "X-CSRF-Token": CSRFToken(p.admin.Token)})
	if response.Code != http.StatusSeeOther {
		t.Fatal(response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != auth.SessionCookie || cookies[0].MaxAge >= 0 || !cookies[0].Secure {
		t.Fatalf("%+v", cookies)
	}
}
