package web

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/db"
)

func (p *pages) postForm(target string, principal *auth.Principal, values url.Values) *http.Response {
	values.Set("_csrf", CSRFToken(principal.Token))
	return p.do("POST", target, principal, strings.NewReader(values.Encode()), nil).Result()
}

func TestChangePasswordReplacesTheSession(t *testing.T) {
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleOperator)
	other, err := p.app.Auth.Login(context.Background(), "operator", "password-u-operator", auth.Client{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		values url.Values
		want   string
	}{
		{url.Values{"currentPassword": {"password-u-operator"}, "newPassword": {"a-new-long-password"}, "confirmPassword": {"a-new-long-passwore"}}, "New passwords do not match."},
		{url.Values{"currentPassword": {"wrong-password"}, "newPassword": {"a-new-long-password"}, "confirmPassword": {"a-new-long-password"}}, "Current password is incorrect"},
	} {
		response := p.postForm("/account/password", operator, c.values)
		body := readBody(t, response)
		if response.StatusCode != http.StatusBadRequest || !strings.Contains(body, c.want) || strings.Contains(body, "a-new-long-password") {
			t.Fatal(response.StatusCode, body)
		}
	}
	response := p.postForm("/account/password", operator, url.Values{"currentPassword": {"password-u-operator"},
		"newPassword": {"a-new-long-password"}, "confirmPassword": {"a-new-long-password"}})
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/account?changed=1" {
		t.Fatal(response.StatusCode, response.Header)
	}
	var replacement string
	for _, cookie := range response.Cookies() {
		if cookie.Name == auth.SessionCookie {
			replacement = cookie.Value
		}
	}
	for _, token := range []string{operator.Token, other.Token} {
		if principal, _ := p.app.Auth.Session(context.Background(), token); principal != nil {
			t.Fatal("old session still valid")
		}
	}
	current, _ := p.app.Auth.Session(context.Background(), replacement)
	if current == nil {
		t.Fatal("replacement session missing")
	}
	page := p.do("GET", "/account?changed=1", current, nil, nil)
	if !strings.Contains(page.Body.String(), "Password changed. Other sessions have been signed out.") ||
		!strings.Contains(page.Body.String(), CSRFToken(replacement)) {
		t.Fatal(page.Body.String())
	}
}

func TestRevokeSessions(t *testing.T) {
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleOperator)
	phone, _ := p.app.Auth.Login(context.Background(), "operator", "password-u-operator", auth.Client{IP: "192.0.2.7", UserAgent: "Phone"})
	sessions, _ := p.app.Auth.Sessions(context.Background(), phone)
	var phoneID string
	for _, session := range sessions {
		if session.Current {
			phoneID = session.ID
		}
	}
	// Another account's session is not found and stays signed in.
	response := p.post("/account/sessions/"+phoneID+"/revoke", p.admin, CSRFToken(p.admin.Token))
	if !strings.Contains(response.Body.String(), "Session not found") {
		t.Fatal(response.Code, response.Body.String())
	}
	if principal, _ := p.app.Auth.Session(context.Background(), phone.Token); principal == nil {
		t.Fatal("revoked another account's session")
	}
	response = p.post("/account/sessions/"+phoneID+"/revoke", operator, CSRFToken(operator.Token))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Session revoked.") ||
		strings.Contains(response.Body.String(), "192.0.2.7") {
		t.Fatal(response.Code, response.Body.String())
	}
	if principal, _ := p.app.Auth.Session(context.Background(), phone.Token); principal != nil {
		t.Fatal("session not revoked")
	}
	// Revoking this session signs the browser out.
	sessions, _ = p.app.Auth.Sessions(context.Background(), operator)
	response = p.post("/account/sessions/"+sessions[0].ID+"/revoke", operator, CSRFToken(operator.Token))
	if response.Code != http.StatusUnauthorized || response.Header().Get("HX-Redirect") != "/login" ||
		!strings.Contains(response.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatal(response.Code, response.Header())
	}
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
