package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/schedules"
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
	p := newDataPages(t)
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
	page := open()
	if !strings.Contains(page, "Restore game data") || !strings.Contains(page, "/backups/11111111-1111-4111-8111-111111111111/download") {
		t.Fatal(page)
	}
	// Delete carries one action and nothing a template branch left behind.
	if strings.Contains(page, `" else`) || strings.Count(page, "/backups/11111111-1111-4111-8111-111111111111/delete") != 1 {
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

// Only administrators read or delete a server's archives, also once its
// container is gone; then a member's grant gives nothing, and nothing that
// needs the server is offered or accepted. Removing it from Ludock is
// refused while it has backups, which nothing could reach once it left the
// list; one an install removed before that stays on Settings until its
// backups are gone.
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
	folder := t.TempDir()
	p.app.Backups.Storage.Roots = []string{folder}
	const id = "22222222-2222-4222-8222-222222222222"
	if err := os.WriteFile(filepath.Join(folder, id+".tar"), []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.backup(t, id, p.server, "")
	p.app.DB.Exec("UPDATE backups SET destination=?, size=7 WHERE id=?", folder, id)
	p.engine.RemoveContainer(p.docker)
	if _, err := p.app.Servers.RefreshNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	backup = "/servers/" + p.server + "/backups/" + id
	page := p.do("GET", "/servers/"+p.server+"?tab=backups&keep="+id, p.admin, nil, nil).Body.String()
	for _, want := range []string{backup + "/download", "browse=" + id, backup + "/delete"} {
		if !strings.Contains(page, want) {
			t.Fatal(want, page)
		}
	}
	for _, offered := range []string{"can’t be shown", "restore=" + id, "keep=" + id, "keep-backup", "backup-readiness"} {
		if strings.Contains(page, offered) {
			t.Fatal(offered, page)
		}
	}
	if response := p.do("GET", backup+"/download", p.admin, nil, nil); response.Code != http.StatusOK || response.Body.String() != "archive" {
		t.Fatal(response.Code)
	}
	if response := p.do("GET", "/servers/"+p.server+"?tab=backups", operator, nil, nil); response.Code != http.StatusNotFound {
		t.Fatal(response.Code)
	}
	if response := p.do("GET", backup+"/download", operator, nil, nil); response.Code == http.StatusOK {
		t.Fatal(response.Code)
	}
	if response := p.action(backup+"/delete", operator, nil); response.StatusCode == http.StatusOK {
		t.Fatal(response.StatusCode)
	}
	restore := url.Values{"backupId": {id}, "confirmation": {"valheim"}}
	if response := p.action("/servers/"+p.server+"/restores", p.admin, restore); response.StatusCode != http.StatusForbidden {
		t.Fatal("restore", response.StatusCode)
	}
	if response := p.action("/servers/"+p.server+"/backups", p.admin, nil); response.StatusCode != http.StatusForbidden {
		t.Fatal("backup", response.StatusCode)
	}
	if panel := p.do("GET", "/fragments/servers/"+p.server+"/update", p.admin, nil, map[string]string{"HX-Request": "true"}).Body.String(); !strings.Contains(panel, "can’t find this server in Docker") ||
		strings.Contains(panel, "Check again") {
		t.Fatal(panel)
	}
	if kind, _ := p.queued(t); kind != "" {
		t.Fatal("queued", kind)
	}
	if response := p.post("/servers/"+p.server+"/remove", p.admin, CSRFToken(p.admin.Token)); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "Delete this server’s backups on its Backups tab") {
		t.Fatal(response.Code, response.Body.String())
	}
	if page := p.do("GET", "/servers/"+p.server, p.admin, nil, nil).Body.String(); !strings.Contains(page, "move them to its new server") ||
		strings.Contains(page, "/servers/"+p.server+"/remove") {
		t.Fatal(page)
	}
	storage := func() string {
		return p.do("GET", "/fragments/settings/backup-storage", p.admin, nil, map[string]string{"HX-Request": "true"}).Body.String()
	}
	link := `href="/servers/` + p.server + `?tab=backups">valheim</a> (1 backup)`
	if strings.Contains(storage(), link) {
		t.Fatal(storage())
	}
	if _, err := p.app.DB.Exec(`INSERT INTO settings (key, value_json) SELECT 'server.choice:' || external_identity,
    '{"managed":false}' FROM logical_servers WHERE id=?`, p.server); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(storage(), link) {
		t.Fatal(storage())
	}
	if response := p.action(backup+"/delete", p.admin, nil); response.StatusCode != http.StatusOK {
		t.Fatal(response.StatusCode, readBody(t, response))
	}
	if _, err := os.Stat(filepath.Join(folder, id+".tar")); !os.IsNotExist(err) {
		t.Fatal("archive left", err)
	}
	if strings.Contains(storage(), link) {
		t.Fatal(storage())
	}
	if response := p.post("/servers/"+p.server+"/remove", p.admin, CSRFToken(p.admin.Token)); response.Header().Get("HX-Redirect") != "/" {
		t.Fatal(response.Code, response.Body.String())
	}
}

// One of two backups lost its archive: Settings and the server's Backups tab
// count only the one still there. While the folder can't be read, both pages
// count both backups as recorded and say so.
func TestBackupSpaceCountsArchivesFound(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	folder := t.TempDir()
	p.app.Backups.Storage.Roots = []string{folder}
	const there, lost = "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	if err := os.WriteFile(filepath.Join(folder, there+".tar"), []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{there, lost} {
		p.backup(t, id, p.server, "")
		p.app.DB.Exec("UPDATE backups SET destination=?, size=7 WHERE id=?", folder, id)
	}
	if page := p.do("GET", "/servers/"+p.server+"?tab=backups", p.admin, nil, nil).Body.String(); !strings.Contains(page, "1 backup uses 7 B.") {
		t.Fatal(page)
	}
	storage := func() string {
		return p.do("GET", "/fragments/settings/backup-storage", p.admin, nil, nil).Body.String()
	}
	if page := storage(); !strings.Contains(page, "<dd>7 B") || strings.Contains(page, "As recorded") {
		t.Fatal(page)
	}
	if err := os.Rename(folder, folder+"-away"); err != nil {
		t.Fatal(err)
	}
	if page := storage(); !strings.Contains(page, "<dd>14 B") || !strings.Contains(page, "As recorded; a folder can’t be read") {
		t.Fatal(page)
	}
	if page := p.do("GET", "/servers/"+p.server+"?tab=backups", p.admin, nil, nil).Body.String(); !strings.Contains(page, "2 backups use 14 B. As recorded; a folder can’t be read.") {
		t.Fatal(page)
	}
}

// Updating needs an administrator, skipping its backup needs the server's
// name typed, and a container Docker removes on stop is never replaced.
func TestUpdatesNeedAdminAndConfirmation(t *testing.T) {
	t.Parallel()
	p := newDataPages(t)
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
	// pulled, and so is a backup, a restart, a schedule that would back it
	// up, and resuming one saved while it had folders.
	paused, err := p.app.Schedules.Create(context.Background(), &p.admin.User, p.admin.TokenHash, p.server, schedules.Input{Kind: "recurring",
		Action: "backup", Time: "08:00", Timezone: "UTC", Days: []int{1}})
	if err != nil {
		t.Fatal(err)
	}
	p.engine.Update(p.docker, func(c *fakedocker.Container) {
		c.HostConfig, c.Mounts = nil, nil
		c.State.Running, c.State.Status = true, "running"
	})
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
	resume := url.Values{"enabled": {"true"}, "revision": {strconv.FormatInt(paused.Revision, 10)}}
	if response := p.action("/servers/"+p.server+"/schedules/"+paused.ID+"/toggle", p.admin, resume); response.StatusCode != http.StatusConflict {
		t.Fatal("resuming:", response.StatusCode)
	}
	if response := p.action("/servers/"+p.server+"/restart", p.admin, url.Values{"createBackup": {"true"}}); response.StatusCode != http.StatusConflict {
		t.Fatal("restart with a backup first:", response.StatusCode)
	}
	if response := p.action("/servers/"+p.server+"/backups", p.admin, nil); response.StatusCode != http.StatusConflict {
		t.Fatal("create backup:", response.StatusCode)
	}
}

// The Backups tab follows work that is still running even without a link
// to it, such as after Dismiss, so the tab hears when the work ends and
// brings back the controls it paused.
func TestBackupsTabFollowsRunningWork(t *testing.T) {
	t.Parallel()
	p := newDataPages(t)
	if response := p.action("/servers/"+p.server+"/backups", p.admin, nil); response.StatusCode != http.StatusOK {
		t.Fatal(response.StatusCode)
	}
	body := p.do("GET", "/servers/"+p.server+"?tab=backups", p.admin, nil, nil).Body.String()
	if !strings.Contains(body, `id="selected-operation"`) || !strings.Contains(body, `hx-trigger="every 2s"`) ||
		strings.Contains(body, ">Dismiss</a>") {
		t.Fatal(body)
	}
}

// Access taken away after a request's first check and before its write
// stops the write: nothing is queued, cancelled, deleted, or asked of the
// registry, and the request gets the refusal it would have had earlier. A
// start pauses no outage tracking. A settings form refused for its input
// shows nothing saved, and an account change writes nothing, for a session
// that ended after it was checked.
func TestWritesRecheckAccessInside(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const backupID = "33333333-3333-4333-8333-333333333333"
	member := func(p *pages, capabilities ...authz.Capability) *auth.Principal {
		who := p.user("u-member", "member", db.RoleMember)
		p.grant(who, capabilities...)
		return who
	}
	queued := func(t *testing.T, p *pages) bool { kind, _ := p.queued(t); return kind != "" }
	for _, test := range []struct {
		name string
		// setup returns who sends the request and where.
		setup func(t *testing.T, p *pages) (*auth.Principal, string)
		// revoke takes access away, given who's ID and session.
		revoke  string
		status  int
		written func(t *testing.T, p *pages) bool
	}{
		{name: "backup", revoke: "DELETE FROM server_grants WHERE user_id=?1", status: http.StatusNotFound, written: queued,
			setup: func(t *testing.T, p *pages) (*auth.Principal, string) {
				return member(p, authz.ServerView, authz.BackupsCreate), "/servers/" + p.server + "/backups"
			}},
		{name: "restart", revoke: "UPDATE users SET disabled=1 WHERE id=?1", status: http.StatusUnauthorized, written: queued,
			setup: func(t *testing.T, p *pages) (*auth.Principal, string) {
				if err := p.app.Docker.Start(ctx, p.docker); err != nil {
					t.Fatal(err)
				}
				return member(p, authz.ServerView, authz.ServerRestart), "/servers/" + p.server + "/restart"
			}},
		{name: "cancel", revoke: `UPDATE server_grants SET capabilities_json='["server.view"]' WHERE user_id=?1`, status: http.StatusForbidden,
			setup: func(t *testing.T, p *pages) (*auth.Principal, string) {
				server, _ := p.app.Identity.Get(ctx, p.server)
				job, err := p.app.Runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: p.server, ActorID: p.admin.User.ID, Kind: ops.KindRestart,
					BindingRevision: server.BindingRevision})
				if err != nil {
					t.Fatal(err)
				}
				return member(p, authz.ServerView, authz.ServerRestart), "/servers/" + p.server + "/operations/" + job.ID + "/cancel"
			},
			written: func(t *testing.T, p *pages) bool {
				var status string
				p.app.DB.QueryRow("SELECT status FROM operations").Scan(&status)
				return status != ops.Queued
			}},
		{name: "delete backup", revoke: "DELETE FROM sessions WHERE token_hash=?2", status: http.StatusUnauthorized,
			setup: func(t *testing.T, p *pages) (*auth.Principal, string) {
				folder := t.TempDir()
				p.app.Backups.Storage.Roots = []string{folder}
				if err := os.WriteFile(filepath.Join(folder, backupID+".tar"), []byte("archive"), 0o600); err != nil {
					t.Fatal(err)
				}
				p.backup(t, backupID, p.server, "")
				p.app.DB.Exec("UPDATE backups SET destination=?, size=7 WHERE id=?", folder, backupID)
				return p.admin, "/servers/" + p.server + "/backups/" + backupID + "/delete"
			},
			written: func(t *testing.T, p *pages) bool {
				_, err := os.Stat(filepath.Join(p.app.Backups.Storage.Roots[0], backupID+".tar"))
				found, _ := p.app.Backups.Exists(ctx, backupID, true)
				return err != nil || !found
			}},
		{name: "image check", revoke: "DELETE FROM sessions WHERE token_hash=?2", status: http.StatusUnauthorized,
			setup: func(t *testing.T, p *pages) (*auth.Principal, string) {
				return p.admin, "/servers/" + p.server + "/update-check"
			},
			written: func(t *testing.T, p *pages) bool { return len(p.engine.RegistryAuth) != 0 }},
		{name: "start", revoke: "DELETE FROM server_grants WHERE user_id=?1", status: http.StatusNotFound,
			setup: func(t *testing.T, p *pages) (*auth.Principal, string) {
				return member(p, authz.ServerView, authz.ServerStart), "/servers/" + p.server + "/start"
			},
			written: func(t *testing.T, p *pages) bool {
				var paused int
				p.app.DB.QueryRow("SELECT COUNT(*) FROM availability WHERE suppressed_until > 0").Scan(&paused)
				return paused != 0 || p.started()
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p := newDataPages(t)
			who, target := test.setup(t, p)
			p.app.Authz.Rechecking = func(tx *sql.Tx) {
				if _, err := tx.Exec(test.revoke, who.User.ID, who.TokenHash); err != nil {
					t.Error(err)
				}
			}
			response := p.action(target, who, nil)
			if response.StatusCode != test.status || test.written(t, p) {
				t.Fatal(response.StatusCode, test.written(t, p), readBody(t, response))
			}
		})
	}
	// These requests passed the sign-in check; then their session ended, or
	// the account was disabled, before the handler wrote.
	for _, test := range []struct {
		name, revoke, target, form string
		handler                    func(p *pages) func(http.ResponseWriter, *http.Request, *Request)
		// setup returns the path's session ID, if it has one.
		setup func(t *testing.T, p *pages, who *auth.Principal) string
		// written reports whether the change went through, or for a form,
		// whether it showed what is saved.
		written func(t *testing.T, p *pages, who *auth.Principal, body string) bool
	}{
		{name: "settings form", revoke: "DELETE FROM sessions WHERE token_hash=?2", target: "/settings/upload-limit", form: "limit=lots",
			handler: func(p *pages) func(http.ResponseWriter, *http.Request, *Request) { return p.web.saveUploadLimit },
			written: func(t *testing.T, p *pages, who *auth.Principal, body string) bool {
				return strings.Contains(body, "limit")
			}},
		{name: "sign out a session", revoke: "UPDATE users SET disabled=1 WHERE id=?1", target: "/account/sessions/{id}/revoke",
			handler: func(p *pages) func(http.ResponseWriter, *http.Request, *Request) { return p.web.revokeSession },
			setup: func(t *testing.T, p *pages, who *auth.Principal) string {
				other, err := p.app.Auth.Login(ctx, who.User.Username, "password-"+who.User.ID, auth.Client{})
				if err != nil {
					t.Fatal(err)
				}
				var id string
				p.app.DB.QueryRow("SELECT session_id FROM sessions WHERE token_hash=?", other.TokenHash).Scan(&id)
				return id
			},
			written: func(t *testing.T, p *pages, who *auth.Principal, body string) bool {
				var sessions int
				p.app.DB.QueryRow("SELECT COUNT(*) FROM sessions WHERE user_id=?", who.User.ID).Scan(&sessions)
				return sessions != 2
			}},
		{name: "set up two-factor", revoke: "DELETE FROM sessions WHERE token_hash=?2", target: "/account/two-factor/setup",
			handler: func(p *pages) func(http.ResponseWriter, *http.Request, *Request) { return p.web.beginTwoFactor },
			written: func(t *testing.T, p *pages, who *auth.Principal, body string) bool {
				var pending int
				p.app.DB.QueryRow("SELECT COUNT(*) FROM users WHERE id=? AND totp_pending IS NOT NULL", who.User.ID).Scan(&pending)
				return pending != 0
			}},
		{name: "cancel two-factor setup", revoke: "DELETE FROM sessions WHERE token_hash=?2", target: "/account/two-factor/cancel",
			handler: func(p *pages) func(http.ResponseWriter, *http.Request, *Request) { return p.web.cancelTwoFactor },
			setup: func(t *testing.T, p *pages, who *auth.Principal) string {
				if _, err := p.app.Auth.BeginTwoFactor(ctx, who); err != nil {
					t.Fatal(err)
				}
				return ""
			},
			written: func(t *testing.T, p *pages, who *auth.Principal, body string) bool {
				var pending int
				p.app.DB.QueryRow("SELECT COUNT(*) FROM users WHERE id=? AND totp_pending IS NOT NULL", who.User.ID).Scan(&pending)
				return pending == 0
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p := newPages(t)
			who := p.admin
			sessionID := ""
			if test.setup != nil {
				sessionID = test.setup(t, p, who)
			}
			request := &Request{Principal: who, CSRF: CSRFToken(who.Token)}
			if _, err := p.app.DB.Exec(test.revoke, who.User.ID, who.TokenHash); err != nil {
				t.Fatal(err)
			}
			form := httptest.NewRequest("POST", "http://panel.example"+strings.Replace(test.target, "{id}", sessionID, 1),
				strings.NewReader(test.form))
			form.SetPathValue("id", sessionID)
			form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			form.Header.Set("HX-Request", "true")
			response := httptest.NewRecorder()
			test.handler(p)(response, form, request)
			if body := response.Body.String(); response.Code != http.StatusUnauthorized || test.written(t, p, who, body) {
				t.Fatal(response.Code, test.written(t, p, who, body), body)
			}
		})
	}
}
