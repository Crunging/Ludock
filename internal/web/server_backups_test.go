package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
)

func (p *pages) action(target string, principal *auth.Principal, values url.Values) *http.Response {
	if values == nil {
		values = url.Values{}
	}
	values.Set("_csrf", CSRFToken(principal.Token))
	return p.do("POST", target, principal, strings.NewReader(values.Encode()),
		map[string]string{"HX-Request": "true", "HX-Target": "detail-status"}).Result()
}

func (p *pages) queued(t *testing.T) (kind string, input string) {
	t.Helper()
	p.app.DB.QueryRow("SELECT kind,input_json FROM operations ORDER BY created_at DESC LIMIT 1").Scan(&kind, &input)
	return kind, input
}

func TestCreateBackupNeedsTheGrant(t *testing.T) {
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleOperator)
	p.grant(operator, authz.ServerView)
	if response := p.action("/servers/"+p.server+"/backups", operator, nil); response.StatusCode != http.StatusForbidden {
		t.Fatal(response.StatusCode)
	}
	p.grant(operator, authz.ServerView, authz.BackupsCreate)
	response := p.action("/servers/"+p.server+"/backups", operator, nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("HX-Redirect"), "tab=activity&operation=") {
		t.Fatal(response.StatusCode, response.Header)
	}
	if kind, _ := p.queued(t); kind != "backup" {
		t.Fatal(kind)
	}
	// Operators see the summary, never the archive list.
	page := p.do("GET", "/servers/"+p.server+"?tab=backups", operator, nil, nil).Body.String()
	if !strings.Contains(page, "Archive access and restoration require an administrator") || strings.Contains(page, "backup-list") {
		t.Fatal(page)
	}
}

func TestRestoreChecksConfirmationAndBackup(t *testing.T) {
	p := newPages(t)
	p.app.DB.Exec(`INSERT INTO backups (id,server_id,binding_fingerprint,destination,roots_json,size,checksum,created_at,state)
    VALUES ('11111111-1111-4111-8111-111111111111',?,'f','/backups','[]',10,'c',1,'complete')`, p.server)
	operator := p.user("u-operator", "operator", db.RoleOperator)
	p.grant(operator, authz.ServerView, authz.BackupsCreate)
	restore := func(principal *auth.Principal, backup, confirmation string) *http.Response {
		return p.action("/servers/"+p.server+"/restores", principal, url.Values{"backupId": {backup}, "confirmation": {confirmation}})
	}
	if response := restore(operator, "11111111-1111-4111-8111-111111111111", "valheim"); response.StatusCode != http.StatusForbidden {
		t.Fatal("operator", response.StatusCode)
	}
	if response := restore(p.admin, "11111111-1111-4111-8111-111111111111", "Valheim"); response.StatusCode != http.StatusBadRequest {
		t.Fatal("confirmation", response.StatusCode)
	}
	if response := restore(p.admin, "22222222-2222-4222-8222-222222222222", "valheim"); response.StatusCode != http.StatusConflict {
		t.Fatal("missing", response.StatusCode)
	}
	if response := restore(p.admin, "11111111-1111-4111-8111-111111111111", "valheim"); response.StatusCode != http.StatusOK {
		t.Fatal("restore", response.StatusCode)
	}
	if kind, input := p.queued(t); kind != "restore" || !strings.Contains(input, `"backupId":"11111111-1111-4111-8111-111111111111"`) {
		t.Fatal(kind, input)
	}
	page := p.do("GET", "/servers/"+p.server+"?tab=backups&restore=11111111-1111-4111-8111-111111111111", p.admin, nil, nil).Body.String()
	if !strings.Contains(page, "Restore game data") || !strings.Contains(page, "/backups/11111111-1111-4111-8111-111111111111/download") {
		t.Fatal(page)
	}
}

func TestArchiveAccessNeedsAnAdministrator(t *testing.T) {
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleOperator)
	p.grant(operator, authz.ServerView, authz.BackupsCreate)
	backup := "/servers/" + p.server + "/backups/11111111-1111-4111-8111-111111111111"
	if response := p.do("GET", backup+"/download", operator, nil, nil); response.Code == http.StatusOK {
		t.Fatal(response.Code)
	}
	if response := p.action(backup+"/delete", operator, nil); response.StatusCode != http.StatusForbidden {
		t.Fatal(response.StatusCode)
	}
	// A missing backup is a conflict, as for every backup failure.
	if response := p.action(backup+"/delete", p.admin, nil); response.StatusCode != http.StatusConflict {
		t.Fatal(response.StatusCode, readBody(t, response))
	}
	if response := p.do("GET", backup+"/download", p.admin, nil, nil); response.Code != http.StatusConflict {
		t.Fatal(response.Code)
	}
}

func TestUpdatesNeedAdminAndConfirmation(t *testing.T) {
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleOperator)
	p.grant(operator, authz.ServerView, authz.ServerRestart)
	values := url.Values{"createBackup": {"true"}, "confirmed": {"true"}}
	if response := p.action("/servers/"+p.server+"/updates", operator, values); response.StatusCode != http.StatusForbidden {
		t.Fatal(response.StatusCode)
	}
	if response := p.action("/servers/"+p.server+"/updates", p.admin, url.Values{"createBackup": {"true"}}); response.StatusCode != http.StatusBadRequest {
		t.Fatal(response.StatusCode)
	}
	panel := p.do("GET", "/fragments/servers/"+p.server+"/update", operator, nil, map[string]string{"HX-Request": "true"}).Body.String()
	if !strings.Contains(panel, "Pulling a newer image needs an administrator") || strings.Contains(panel, "/updates") {
		t.Fatal(panel)
	}
	// Docker removes a --rm container when it stops, so it can't be replaced:
	// the form is not offered and a forged request is refused.
	p.engine.Update(p.docker, func(c *fakedocker.Container) { c.HostConfig = map[string]any{"AutoRemove": true} })
	panel = p.do("GET", "/fragments/servers/"+p.server+"/update", p.admin, nil, map[string]string{"HX-Request": "true"}).Body.String()
	if !strings.Contains(panel, "removes this container when it stops") || !strings.Contains(panel, "Check again") || strings.Contains(panel, "/updates") {
		t.Fatal(panel)
	}
	values.Set("skipBackupConfirmation", "valheim")
	values.Set("createBackup", "false")
	if response := p.action("/servers/"+p.server+"/updates", p.admin, values); response.StatusCode != http.StatusConflict {
		t.Fatal(response.StatusCode)
	}
}
