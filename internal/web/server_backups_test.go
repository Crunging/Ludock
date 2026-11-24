package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
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
		map[string]string{"HX-Request": "true", "HX-Target": "div#detail-status"}).Result()
}

func (p *pages) queued(t *testing.T) (kind string, input string) {
	t.Helper()
	p.app.DB.QueryRow("SELECT kind,input_json FROM operations ORDER BY created_at DESC LIMIT 1").Scan(&kind, &input)
	return kind, input
}

func TestCreateBackupNeedsTheGrant(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleMember)
	p.grant(operator, authz.ServerView)
	if response := p.action("/servers/"+p.server+"/backups", operator, nil); response.StatusCode != http.StatusForbidden {
		t.Fatal(response.StatusCode)
	}
	p.grant(operator, authz.ServerView, authz.BackupsCreate)
	response := p.action("/servers/"+p.server+"/backups", operator, nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("HX-Location"), "tab=backups&operation=") {
		t.Fatal(response.StatusCode, response.Header)
	}
	if kind, _ := p.queued(t); kind != "backup" {
		t.Fatal(kind)
	}
	// Operators see the summary, never the archive list.
	page := p.do("GET", "/servers/"+p.server+"?tab=backups", operator, nil, nil).Body.String()
	if !strings.Contains(page, "Last backup: never") || strings.Contains(page, "backup-list") || strings.Contains(page, "administrator") {
		t.Fatal(page)
	}
}

func TestRestoreChecksConfirmationAndBackup(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	p.backup(t, "11111111-1111-4111-8111-111111111111", p.server, "")
	operator := p.user("u-operator", "operator", db.RoleMember)
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
	// The form isn't offered while the restore is queued, and is once it ends.
	open := func() string {
		return p.do("GET", "/servers/"+p.server+"?tab=backups&restore=11111111-1111-4111-8111-111111111111", p.admin, nil, nil).Body.String()
	}
	if page := open(); strings.Contains(page, "Restore game data") {
		t.Fatal(page)
	}
	p.app.DB.Exec("UPDATE operations SET status='succeeded'")
	if page := open(); !strings.Contains(page, "Restore game data") || !strings.Contains(page, "/backups/11111111-1111-4111-8111-111111111111/download") {
		t.Fatal(page)
	}
}

// backup records a complete backup of server taken from the binding it has
// now, or from fingerprint when one is given; the game is valheim.
func (p *pages) backup(t *testing.T, id, server, fingerprint string) {
	t.Helper()
	if _, err := p.app.DB.Exec(`INSERT INTO backups (id,server_id,binding_fingerprint,destination,roots_json,size,checksum,created_at,state,game_type)
    SELECT ?,id,COALESCE(NULLIF(?,''),binding_fingerprint),'/backups','[]',10,'c',1,'complete','valheim' FROM logical_servers WHERE id=?`,
		id, fingerprint, server); err != nil {
		t.Fatal(err)
	}
}

// A backup taken before the server's data moved to other host storage
// restores only with the confirmation the page sends, for the binding the
// page showed; a forged or stale request is refused before anything is queued.
func TestRestoreAfterDataMovedNeedsConfirmation(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	const moved = "11111111-1111-4111-8111-111111111111"
	p.backup(t, moved, p.server, "an-earlier-binding")
	values := url.Values{"backupId": {moved}, "confirmation": {"valheim"}}
	if response := p.action("/servers/"+p.server+"/restores", p.admin, values); response.StatusCode != http.StatusBadRequest ||
		!strings.Contains(readBody(t, response), "lived somewhere else") {
		t.Fatal(response.StatusCode)
	}
	if kind, _ := p.queued(t); kind != "" {
		t.Fatal("queued", kind)
	}
	page := p.do("GET", "/servers/"+p.server+"?tab=backups&restore="+moved, p.admin, nil, nil).Body.String()
	if !strings.Contains(page, "Taken when the server’s data lived somewhere else") || !strings.Contains(page, `name="dataMoved" value="true"`) {
		t.Fatal(page)
	}
	var revision int64
	p.app.DB.QueryRow("SELECT binding_revision FROM logical_servers WHERE id=?", p.server).Scan(&revision)
	values.Set("dataMoved", "true")
	values.Set("bindingRevision", strconv.FormatInt(revision-1, 10))
	if response := p.action("/servers/"+p.server+"/restores", p.admin, values); response.StatusCode != http.StatusBadRequest {
		t.Fatal("stale", response.StatusCode)
	}
	values.Set("bindingRevision", strconv.FormatInt(revision, 10))
	if response := p.action("/servers/"+p.server+"/restores", p.admin, values); response.StatusCode != http.StatusOK {
		t.Fatal(response.StatusCode, readBody(t, response))
	}
	if kind, input := p.queued(t); kind != "restore" || !strings.Contains(input, `"dataMoved":true`) {
		t.Fatal(kind, input)
	}
}

// A backup of another game is refused with the reason, and the page offers
// no restore for it; a backup of another server can't be restored here even
// when its folders match.
func TestRestoreRefusesOtherGamesAndServers(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	const otherGame, otherServer = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	p.backup(t, otherGame, p.server, "an-earlier-binding")
	p.app.DB.Exec("UPDATE backups SET game_type='minecraft' WHERE id=?", otherGame)
	values := url.Values{"backupId": {otherGame}, "confirmation": {"valheim"}, "dataMoved": {"true"}}
	if response := p.action("/servers/"+p.server+"/restores", p.admin, values); response.StatusCode != http.StatusConflict ||
		!strings.Contains(readBody(t, response), "holds Minecraft data, but the server now runs Valheim") {
		t.Fatal(response.StatusCode)
	}
	page := p.do("GET", "/servers/"+p.server+"?tab=backups&restore="+otherGame, p.admin, nil, nil).Body.String()
	if !strings.Contains(page, "Can’t be restored here.") || strings.Contains(page, "Restore game data") || strings.Contains(page, "restore="+otherGame) {
		t.Fatal(page)
	}
	p.engine.AddContainer(fakedocker.GameOptions{Name: "valheim-two", Image: "lloesche/valheim-server"})
	if _, err := p.app.Servers.RefreshNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	var other string
	p.app.DB.QueryRow("SELECT id FROM logical_servers WHERE id<>?", p.server).Scan(&other)
	p.backup(t, otherServer, other, "")
	values.Set("backupId", otherServer)
	if response := p.action("/servers/"+p.server+"/restores", p.admin, values); response.StatusCode != http.StatusConflict ||
		!strings.Contains(readBody(t, response), "Backup not found") {
		t.Fatal(response.StatusCode)
	}
	if kind, _ := p.queued(t); kind != "" {
		t.Fatal("queued", kind)
	}
}

func TestArchiveAccessNeedsAnAdministrator(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleMember)
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

// Updating needs an administrator, skipping its backup needs the server's
// name typed, and a container Docker removes on stop is never replaced.
func TestUpdatesNeedAdminAndConfirmation(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleMember)
	p.grant(operator, authz.ServerView, authz.ServerRestart)
	values := url.Values{"createBackup": {"true"}}
	if response := p.action("/servers/"+p.server+"/updates", operator, values); response.StatusCode != http.StatusForbidden {
		t.Fatal(response.StatusCode)
	}
	// Skipping the backup needs the server's name typed.
	if response := p.action("/servers/"+p.server+"/updates", p.admin, url.Values{"createBackup": {"false"}}); response.StatusCode != http.StatusBadRequest {
		t.Fatal(response.StatusCode)
	}
	// A member who can restart isn't offered the Update tab at all.
	if body := p.do("GET", "/servers/"+p.server, operator, nil, nil).Body.String(); strings.Contains(body, "tab=update") {
		t.Fatal(body)
	}
	if response := p.do("GET", "/fragments/servers/"+p.server+"/update", operator, nil, map[string]string{"HX-Request": "true"}); response.Code == http.StatusOK {
		t.Fatal(response.Code)
	}
	// Docker deletes a --rm container when it stops, so it can't be replaced:
	// a forged request is refused.
	p.engine.Update(p.docker, func(c *fakedocker.Container) { c.HostConfig = map[string]any{"AutoRemove": true} })
	values.Set("skipBackupConfirmation", "valheim")
	values.Set("createBackup", "false")
	if response := p.action("/servers/"+p.server+"/updates", p.admin, values); response.StatusCode != http.StatusConflict {
		t.Fatal(response.StatusCode)
	}
	// A server with no data folders has nothing to back up first: a form
	// opened earlier, or a forged request, is refused before any image is
	// pulled, and so is a schedule that would back it up.
	p.engine.Update(p.docker, func(c *fakedocker.Container) { c.HostConfig, c.Mounts = nil, nil })
	if _, err := p.app.Servers.RefreshNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if response := p.action("/servers/"+p.server+"/updates", p.admin, url.Values{"createBackup": {"true"}}); response.StatusCode != http.StatusConflict {
		t.Fatal(response.StatusCode)
	}
	p.engine.Update(p.docker, func(*fakedocker.Container) {
		if requests := strings.Join(p.engine.Requests, "\n"); strings.Contains(requests, "/images/create") {
			t.Error("pulled an image for an update that couldn't back up first")
		}
	})
	for _, form := range []url.Values{{"action": {"backup"}}, {"action": {"restart"}, "createBackup": {"true"}}} {
		form.Set("kind", "recurring")
		form.Set("time", "09:00")
		form.Set("timezone", "UTC")
		form.Set("days", "1")
		if response := p.action("/servers/"+p.server+"/schedules", p.admin, form); response.StatusCode != http.StatusConflict {
			t.Fatal(form, response.StatusCode)
		}
	}
}

// The Backups tab follows work that is still running even without a link
// to it, such as after Dismiss, so the tab hears when the work ends and
// brings back the controls it paused.
func TestBackupsTabFollowsRunningWork(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	if response := p.action("/servers/"+p.server+"/backups", p.admin, nil); response.StatusCode != http.StatusOK {
		t.Fatal(response.StatusCode)
	}
	body := p.do("GET", "/servers/"+p.server+"?tab=backups", p.admin, nil, nil).Body.String()
	if !strings.Contains(body, `id="selected-operation"`) || !strings.Contains(body, `hx-trigger="every 2s"`) ||
		strings.Contains(body, ">Dismiss</a>") {
		t.Fatal(body)
	}
}
