package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/db"
)

func TestSettingsNeedAnAdministrator(t *testing.T) {
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleOperator)
	for _, target := range []string{"/settings", "/diagnostics"} {
		if response := p.do("GET", target, operator, nil, nil); response.Code != http.StatusSeeOther {
			t.Fatal(target, response.Code)
		}
	}
	form := url.Values{"_csrf": {CSRFToken(operator.Token)}, "host": {"evil.example"}}
	if response := p.do("POST", "/settings/connection", operator, strings.NewReader(form.Encode()), map[string]string{"HX-Request": "true"}); response.Code != http.StatusForbidden {
		t.Fatal(response.Code)
	}
}

// The webhook URL is a credential: it never comes back to the browser.
func TestWebhookStaysPrivate(t *testing.T) {
	p := newPages(t)
	secret := "https://discord.com/api/webhooks/123/secret-token"
	saved := p.adminPost("/settings/notifications", url.Values{"webhookUrl": {secret}})
	if !strings.Contains(saved.Body, "Notification settings saved.") || strings.Contains(saved.Body, "secret-token") {
		t.Fatal(saved.Body)
	}
	if page := p.do("GET", "/settings", p.admin, nil, nil).Body.String(); strings.Contains(page, "secret-token") || !strings.Contains(page, "Replace webhook URL") {
		t.Fatal("webhook leaked or not saved")
	}
}
