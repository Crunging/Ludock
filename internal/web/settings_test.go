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

func TestConnectionAndNotificationSettings(t *testing.T) {
	p := newPages(t)
	if bad := p.adminPost("/settings/connection", url.Values{"host": {"not a host!"}}); bad.Code != http.StatusBadRequest {
		t.Fatal(bad.Code, bad.Body)
	}
	saved := p.adminPost("/settings/connection", url.Values{"host": {"Play.Example.com"}})
	if !strings.Contains(saved.Body, "Server address saved.") || !strings.Contains(saved.Body, `value="play.example.com"`) {
		t.Fatal(saved.Body)
	}
	if cleared := p.adminPost("/settings/connection", url.Values{"host": {""}}); !strings.Contains(cleared.Body, "Server address cleared.") {
		t.Fatal(cleared.Body)
	}
	if missing := p.adminPost("/settings/notifications", url.Values{"enabled": {"true"}}); missing.Code != http.StatusBadRequest {
		t.Fatal(missing.Code)
	}
	secret := "https://discord.com/api/webhooks/123/secret-token"
	saved = p.adminPost("/settings/notifications", url.Values{"enabled": {"true"}, "webhookUrl": {secret}})
	if !strings.Contains(saved.Body, "Notification settings saved.") || strings.Contains(saved.Body, "secret-token") {
		t.Fatal(saved.Body)
	}
	page := p.do("GET", "/settings", p.admin, nil, nil).Body.String()
	if strings.Contains(page, "secret-token") || !strings.Contains(page, "Replace webhook URL") {
		t.Fatal("webhook leaked or not configured")
	}
	queued := p.adminPost("/settings/notifications/test", url.Values{})
	if !strings.Contains(queued.Body, "Test notification queued.") || strings.Contains(queued.Body, "secret-token") {
		t.Fatal(queued.Body)
	}
}

func TestBackupSettingsValidation(t *testing.T) {
	p := newPages(t)
	bad := p.adminPost("/settings/backups", url.Values{"destination": {"/backups"}, "retentionCount": {"0"}, "maxGiB": {"1"}, "reserveGiB": {"0"}})
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body, "Check the backup settings") || !strings.Contains(bad.Body, `value="0"`) {
		t.Fatal(bad.Code, bad.Body)
	}
}
