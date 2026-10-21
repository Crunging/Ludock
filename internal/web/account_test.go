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

// Revoking a session from the account page is covered by the account journey.
func TestRevokeSessionsStaysWithinTheAccountAndIsAudited(t *testing.T) {
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
	// A revocation that cannot be audited does not happen.
	if _, err := p.app.DB.Exec(`CREATE TRIGGER no_revoke_audit BEFORE INSERT ON audit_log
		WHEN NEW.action = 'auth.session.revoked' BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	if response = p.post("/account/sessions/"+phoneID+"/revoke", operator, CSRFToken(operator.Token)); response.Code == http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	if principal, _ := p.app.Auth.Session(context.Background(), phone.Token); principal == nil {
		t.Fatal("revoked a session without auditing it")
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
