package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/ops"
)

func TestServerDetailFollowsGrants(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	viewer := p.user("u-viewer", "viewer", db.RoleViewer)
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
}

// Monitoring settings need an administrator.
func TestMonitoringSettingsNeedAdmin(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleOperator)
	p.grant(operator, authz.ServerView)
	form := url.Values{"enabled": {"true"}, "graceSeconds": {"300"}}
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

func TestBindingReviewNeedsAPendingReview(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	form := url.Values{"_csrf": {CSRFToken(p.admin.Token)}, "confirmation": {"valheim"}, "bindingRevision": {"1"}}
	response := p.do("POST", "/servers/"+p.server+"/binding-review", p.admin, strings.NewReader(form.Encode()),
		map[string]string{"HX-Request": "true"})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "no pending binding review") {
		t.Fatal(response.Code, response.Body.String())
	}
}

// Selected operation fragment stays on its server.
func TestOperationFragmentScope(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	job, _ := p.app.Runner.Enqueue(context.Background(), nil, ops.EnqueueOptions{ServerID: p.server, ActorID: p.admin.User.ID,
		Kind: "restart", BindingRevision: 1})
	viewer := p.user("u-viewer", "viewer", db.RoleViewer)
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
	p := newPages(t)
	viewer := p.user("u-viewer", "viewer", db.RoleViewer)
	p.grant(viewer, authz.ServerView)
	if response := p.action("/servers/"+p.server+"/backups", p.admin, nil); response.StatusCode != http.StatusOK {
		t.Fatal(response.StatusCode)
	}
	var operation string
	p.app.DB.QueryRow("SELECT id FROM operations ORDER BY created_at DESC LIMIT 1").Scan(&operation)
	body := p.do("GET", "/servers/"+p.server+"?tab=backups&operation="+operation, viewer, nil, nil).Body.String()
	if !strings.Contains(body, `id="selected-operation"`) || !strings.Contains(body, `id="tab-activity" role="tab" aria-selected="true"`) {
		t.Fatal(body)
	}
}

// The live header sends back the operation it showed; once that work has
// ended, its refresh tells paused tabs such as Backups to catch up, whatever
// operation the page links to.
func TestLiveHeaderAnnouncesFinishedWork(t *testing.T) {
	t.Parallel()
	p := newPages(t)
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
	if response := p.action("/servers/"+p.server+"/backups", p.admin, nil); response.StatusCode != http.StatusOK {
		t.Fatal(response.StatusCode)
	}
	var running string
	p.app.DB.QueryRow("SELECT id FROM operations ORDER BY created_at DESC LIMIT 1").Scan(&running)
	if trigger := live("tab=backups&active=" + running); trigger != "" {
		t.Fatal("work still running:", trigger)
	}
}
