package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/schedules"
)

func TestServerDetailFollowsGrants(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	viewer := p.user("u-viewer", "viewer", db.RoleMember)
	for _, target := range []string{"/servers/" + p.server, "/fragments/servers/" + p.server + "/live",
		"/fragments/servers/" + p.server + "/overview", "/fragments/servers/" + p.server + "/activity"} {
		if response := p.do("GET", target, viewer, nil, map[string]string{"HX-Request": "true"}); response.Code != http.StatusNotFound ||
			strings.Contains(response.Body.String(), "valheim") {
			t.Fatal(target, response.Code)
		}
	}
	p.grant(viewer, authz.ServerView)
	page := p.do("GET", "/servers/"+p.server+"?tab=backups", viewer, nil, nil)
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, "valheim") || strings.Contains(body, "tab=backups") ||
		strings.Contains(body, "/servers/"+p.server+"/start") || strings.Contains(body, "/availability") {
		t.Fatal(page.Code, body)
	}
	admin := p.do("GET", "/servers/"+p.server, p.admin, nil, nil).Body.String()
	for _, want := range []string{"tab=backups", "tab=schedules", "tab=update", "/servers/" + p.server + "/start", "/servers/" + p.server + "/availability"} {
		if !strings.Contains(admin, want) {
			t.Error("admin page lacks", want)
		}
	}
	// Outages Ludock logged are under Everything, which only administrators
	// open; their server's Activity links there.
	outages := `href="/activity?action=outage&amp;serverId=` + p.server + `&amp;show=everything"`
	for user, want := range map[*auth.Principal]bool{viewer: false, p.admin: true} {
		if body := p.do("GET", "/servers/"+p.server+"?tab=activity", user, nil, nil).Body.String(); strings.Contains(body, outages) != want {
			t.Error(user.User.Username, want, body)
		}
	}
}

// Monitoring settings need an administrator.
func TestMonitoringSettingsNeedAdmin(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleMember)
	p.grant(operator, authz.ServerView)
	form := url.Values{"enabled": {"true"}, "graceSeconds": {"300"}, "diskPercent": {"90"}, "memoryPercent": {"90"}, "memoryMinutes": {"5"}}
	response := p.do("POST", "/servers/"+p.server+"/availability", operator,
		strings.NewReader(url.Values{"_csrf": {CSRFToken(operator.Token)}, "enabled": {"true"}, "graceSeconds": {"300"}}.Encode()),
		map[string]string{"HX-Request": "true"})
	if response.Code != http.StatusForbidden {
		t.Fatal(response.Code)
	}
	for grace, status := range map[string]int{"5": 400, "ten": 400, "300": 200} {
		form.Set("graceSeconds", grace)
		form.Set("_csrf", CSRFToken(p.admin.Token))
		response = p.do("POST", "/servers/"+p.server+"/availability", p.admin, strings.NewReader(form.Encode()),
			map[string]string{"HX-Request": "true"})
		if response.Code != status {
			t.Fatal(grace, response.Code, response.Body.String())
		}
	}
	saved, _ := p.app.Monitor.Get(context.Background(), p.server)
	if !saved.Policy.Enabled || saved.Policy.GraceSeconds != 300 {
		t.Fatal(saved.Policy)
	}
}

// A server whose game changed is paused for the operator it was shared with:
// a forged start, which says it is paused, or confirmation from them fails, a confirmation for an
// older state of the server fails, and the administrator's confirmation
// restores the operator's access.
func TestConfirmChangeRestoresSharing(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	ctx := context.Background()
	operator := p.user("u-operator", "operator", db.RoleMember)
	p.grant(operator, authz.ServerView, authz.ServerStart)
	before, _ := p.app.Identity.Get(ctx, p.server)
	p.engine.Update(p.docker, func(c *fakedocker.Container) { c.Config.Labels["ludock.game"] = "minecraft" })
	if _, err := p.app.Servers.RefreshNow(ctx); err != nil {
		t.Fatal(err)
	}
	paused, _ := p.app.Identity.Get(ctx, p.server)
	if !paused.SharingPaused {
		t.Fatalf("%+v", paused)
	}
	if response := p.post("/servers/"+p.server+"/start", operator, CSRFToken(operator.Token)); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "is paused until an administrator confirms a change to it") {
		t.Fatal("paused operator started the server:", response.Code, response.Body.String())
	}
	confirm := func(principal *auth.Principal, revision int64) *httptest.ResponseRecorder {
		form := url.Values{"_csrf": {CSRFToken(principal.Token)}, "bindingRevision": {strconv.FormatInt(revision, 10)}}
		return p.do("POST", "/servers/"+p.server+"/confirm-change", principal, strings.NewReader(form.Encode()),
			map[string]string{"HX-Request": "true"})
	}
	if response := confirm(operator, paused.BindingRevision); response.Code != http.StatusForbidden {
		t.Fatal("operator confirmed the change:", response.Code)
	}
	if response := confirm(p.admin, before.BindingRevision); response.Code != http.StatusConflict {
		t.Fatal("confirmed an older state of the server:", response.Code)
	}
	if current, _ := p.app.Identity.Get(ctx, p.server); !current.SharingPaused {
		t.Fatal("sharing resumed without a confirmation")
	}
	if response := confirm(p.admin, paused.BindingRevision); response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	if response := p.do("GET", "/servers/"+p.server, operator, nil, nil); response.Code != http.StatusOK {
		t.Fatal("confirmation did not restore access:", response.Code)
	}
}

// Selected operation fragment stays on its server.
func TestOperationFragmentScope(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	job, _ := p.app.Runner.Enqueue(context.Background(), nil, ops.EnqueueOptions{ServerID: p.server, ActorID: p.admin.User.ID,
		Kind: "restart", BindingRevision: 1})
	viewer := p.user("u-viewer", "viewer", db.RoleMember)
	if response := p.do("GET", "/fragments/servers/"+p.server+"/operations/"+job.ID, viewer, nil, map[string]string{"HX-Request": "true"}); response.Code != http.StatusNotFound {
		t.Fatal(response.Code)
	}
	fragment := p.do("GET", "/fragments/servers/"+p.server+"/operations/"+job.ID, p.admin, nil, map[string]string{"HX-Request": "true"}).Body.String()
	if !strings.Contains(fragment, `hx-trigger="every 2s"`) || !strings.Contains(fragment, "Restart") {
		t.Fatal(fragment)
	}
}

// Someone who can't open the Backups tab follows a backup linked there,
// such as from the server list, in Activity instead.
func TestOperationLinkFallsBackToActivity(t *testing.T) {
	t.Parallel()
	p := newDataPages(t)
	viewer := p.user("u-viewer", "viewer", db.RoleMember)
	p.grant(viewer, authz.ServerView)
	if response := p.action("/servers/"+p.server+"/backups", p.admin, nil); response.StatusCode != http.StatusOK {
		t.Fatal(response.StatusCode)
	}
	var operation string
	p.app.DB.QueryRow("SELECT id FROM operations ORDER BY created_at DESC LIMIT 1").Scan(&operation)
	body := p.do("GET", "/servers/"+p.server+"?tab=backups&operation="+operation, viewer, nil, nil).Body.String()
	if !strings.Contains(body, `id="selected-operation"`) || !strings.Contains(body, `id="tab-activity" aria-current="page"`) {
		t.Fatal(body)
	}
}

// The live header sends back the operation it showed and whether an undo
// was blocked; once that work has ended or the undo's state changed, its
// refresh tells paused tabs such as Backups to catch up, whatever operation
// the page links to. While work runs, the header says why its controls are
// greyed out.
func TestLiveHeaderAnnouncesFinishedWork(t *testing.T) {
	t.Parallel()
	p := newDataPages(t)
	live := func(query string) string {
		response := p.do("GET", "/fragments/servers/"+p.server+"/live?"+query, p.admin, nil, map[string]string{"HX-Request": "true"})
		if response.Code != http.StatusOK {
			t.Fatal(query, response.Code)
		}
		return response.Header().Get("HX-Trigger")
	}
	if trigger := live("tab=backups"); trigger != "" {
		t.Fatal("no work seen:", trigger)
	}
	if trigger := live("tab=backups&active=11111111-1111-4111-8111-111111111111"); trigger != "operation-finished" {
		t.Fatal("ended work:", trigger)
	}
	if trigger := live("tab=backups&blocked=false"); trigger != "" {
		t.Fatal("undo unchanged:", trigger)
	}
	if trigger := live("tab=backups&blocked=true"); trigger != "operation-finished" {
		t.Fatal("undo no longer blocked:", trigger)
	}
	if response := p.action("/servers/"+p.server+"/backups", p.admin, nil); response.StatusCode != http.StatusOK {
		t.Fatal(response.StatusCode)
	}
	var running string
	p.app.DB.QueryRow("SELECT id FROM operations ORDER BY created_at DESC LIMIT 1").Scan(&running)
	if trigger := live("tab=backups&active=" + running); trigger != "" {
		t.Fatal("work still running:", trigger)
	}
	// Even on the page that shows the work's progress.
	page := p.do("GET", "/servers/"+p.server+"?tab=backups&operation="+running, p.admin, nil, nil).Body.String()
	if !strings.Contains(page, "Paused until the backup finishes.") {
		t.Fatal(page)
	}
}

// The recovery switch isn't on the form while Ludock can't tell whether the
// game supports it, such as when Docker is unreachable: saving another
// monitoring option then leaves recovery as it was. The switch being on the
// form and unticked turns it off.
func TestSavingMonitoringKeepsRecoveryNotOffered(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	save := func(extra url.Values) {
		t.Helper()
		form := url.Values{"_csrf": {CSRFToken(p.admin.Token)}, "enabled": {"true"}, "graceSeconds": {"300"}, "diskPercent": {"90"},
			"memoryPercent": {"90"}, "memoryMinutes": {"5"}}
		for key, value := range extra {
			form[key] = value
		}
		response := p.do("POST", "/servers/"+p.server+"/availability", p.admin, strings.NewReader(form.Encode()),
			map[string]string{"HX-Request": "true"})
		if response.Code != http.StatusOK {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	recover := func() bool {
		saved, _ := p.app.Monitor.Get(context.Background(), p.server)
		return saved.Policy.Recover
	}
	save(url.Values{"recoverOffered": {"true"}, "recover": {"true"}})
	if !recover() {
		t.Fatal("recovery was not switched on")
	}
	save(url.Values{"maintenance": {"true"}})
	if !recover() {
		t.Fatal("a form without the switch turned recovery off")
	}
	save(url.Values{"recoverOffered": {"true"}})
	if recover() {
		t.Fatal("unticking the switch left recovery on")
	}
}

// A running server started with --rm, which Docker deletes when it stops,
// is offered nothing that stops it: the page says why once, even on the
// Update tab, with no "never backed up" warning, the checklist skips it,
// schedules offer only what can run, forged requests, a stopping schedule
// or the backup check are refused, and a stopping schedule already saved
// says why it was skipped.
func TestDeletedOnStopServerOffersNoStop(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	ctx := context.Background()
	restart := schedules.Input{Kind: "recurring", Action: "restart", Enabled: true, Time: "08:00", Timezone: "UTC", Days: []int{0, 1, 2, 3, 4, 5, 6}}
	if _, err := p.app.Schedules.Create(ctx, &p.admin.User, p.admin.TokenHash, p.server, restart); err != nil {
		t.Fatal(err)
	}
	p.engine.Update(p.docker, func(c *fakedocker.Container) {
		c.HostConfig = map[string]any{"AutoRemove": true}
		c.State.Running, c.State.Status = true, "running"
	})
	if _, err := p.app.Servers.RefreshNow(ctx); err != nil {
		t.Fatal(err)
	}
	detail := p.do("GET", "/servers/"+p.server, p.admin, nil, nil).Body.String()
	if strings.Count(detail, "started with --rm") != 1 || strings.Contains(detail, `/stop"`) || strings.Contains(detail, `/restart"`) ||
		strings.Contains(detail, "Last backup") {
		t.Fatal(detail)
	}
	if body := p.do("GET", "/fragments/servers", p.admin, nil, map[string]string{"HX-Request": "true"}).Body.String(); strings.Contains(body, "server-row__never") {
		t.Fatal(body)
	}
	panel := p.do("GET", "/fragments/servers/"+p.server+"/update", p.admin, nil, map[string]string{"HX-Request": "true"}).Body.String()
	if !strings.Contains(panel, "can’t update this server") || strings.Contains(panel, "started with --rm") || strings.Contains(panel, "/updates") {
		t.Fatal(panel)
	}
	if response := p.do("GET", "/fragments/servers/"+p.server+"/backups/preflight", p.admin, nil, map[string]string{"HX-Request": "true"}); response.Code != http.StatusConflict ||
		strings.Contains(response.Body.String(), "create-backup") {
		t.Fatal(response.Code, response.Body.String())
	}
	// A stale Create backup form hears the reason that matters, not that
	// the server has no data folders.
	if response := p.action("/servers/"+p.server+"/backups", p.admin, nil); response.StatusCode != http.StatusConflict ||
		!strings.Contains(readBody(t, response), "--rm") {
		t.Fatal(response.StatusCode)
	}
	for _, action := range []string{"stop", "restart", "backup"} {
		form := url.Values{"kind": {"recurring"}, "action": {action}, "time": {"09:00"}, "timezone": {"UTC"}, "days": {"1"}}
		if response := p.action("/servers/"+p.server+"/schedules", p.admin, form); response.StatusCode != http.StatusConflict {
			t.Fatal(action, response.StatusCode)
		}
	}
	if body := p.do("GET", "/servers/"+p.server+"?tab=backups", p.admin, nil, nil).Body.String(); strings.Contains(body, `id="create-backup"`) {
		t.Fatal(body)
	}
	if body := p.do("GET", "/servers/"+p.server+"?tab=schedules", p.admin, nil, nil).Body.String(); strings.Contains(body, `value="restart"`) ||
		strings.Contains(body, `value="backup"`) || !strings.Contains(body, `value="start"`) {
		t.Fatal(body)
	}
	if body := p.do("GET", "/fragments/checklist", p.admin, nil, map[string]string{"HX-Request": "true"}).Body.String(); strings.Contains(body, "/checklist/recovery") {
		t.Fatal(body)
	}
	for _, path := range []string{"/stop", "/restart", "/backups"} {
		if response := p.action("/servers/"+p.server+path, p.admin, nil); response.StatusCode != http.StatusConflict {
			t.Fatal(path, response.StatusCode)
		}
	}
	next := time.Now().UTC().Truncate(24 * time.Hour).Add(8 * time.Hour)
	if !next.After(time.Now()) {
		next = next.Add(24 * time.Hour)
	}
	p.app.Schedules.Run(ctx, next.UnixMilli())
	list, err := p.app.Schedules.List(ctx, &p.admin.User, p.server)
	if err != nil || len(list) != 1 || !strings.Contains(list[0].LastResult, "started with --rm") {
		t.Fatal(list, err)
	}
	// Pinned by digest as well, the Update tab still says only the header's
	// reason, with nothing to check again.
	p.engine.Update(p.docker, func(c *fakedocker.Container) {
		c.Config.Image = "lloesche/valheim-server@sha256:" + strings.Repeat("5", 64)
	})
	if _, err := p.app.Servers.RefreshNow(ctx); err != nil {
		t.Fatal(err)
	}
	panel = p.do("GET", "/fragments/servers/"+p.server+"/update", p.admin, nil, map[string]string{"HX-Request": "true"}).Body.String()
	if !strings.Contains(panel, "can’t update this server") || strings.Contains(panel, "Check again") {
		t.Fatal(panel)
	}
}
