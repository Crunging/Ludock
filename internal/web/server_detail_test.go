package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/ops"
)

func TestServerDetailFollowsGrants(t *testing.T) {
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
		strings.Contains(body, "/servers/"+p.server+"/start") || strings.Contains(body, "Save monitoring") {
		t.Fatal(page.Code, body)
	}
	admin := p.do("GET", "/servers/"+p.server, p.admin, nil, nil).Body.String()
	for _, want := range []string{"tab=backups", "tab=schedules", "tab=update", "/servers/" + p.server + "/start", "Save monitoring"} {
		if !strings.Contains(admin, want) {
			t.Error("admin page lacks", want)
		}
	}
}

func TestActivityShowsOnlyThisServersOperations(t *testing.T) {
	p := newPages(t)
	job, err := p.app.Runner.Enqueue(context.Background(), nil, ops.EnqueueOptions{ServerID: p.server, ActorID: p.admin.User.ID,
		Kind: "restart", BindingRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	page := p.do("GET", "/servers/"+p.server+"?tab=activity&operation="+job.ID, p.admin, nil, nil).Body.String()
	if !strings.Contains(page, "Operation details") || !strings.Contains(page, "/operations/"+job.ID) || strings.Contains(page, "Operation not found") {
		t.Fatal(page)
	}
	// An operation of another server, or none, is not shown.
	page = p.do("GET", "/servers/"+p.server+"?tab=activity&operation=00000000-0000-4000-8000-000000000000", p.admin, nil, nil).Body.String()
	if !strings.Contains(page, "Operation not found") {
		t.Fatal(page)
	}
	list := p.do("GET", "/fragments/servers/"+p.server+"/activity?status=failed", p.admin, nil, map[string]string{"HX-Request": "true"})
	if !strings.Contains(list.Body.String(), "Showing 0 of 1") || !strings.Contains(list.Body.String(), "An active operation is hidden") ||
		list.Header().Get("HX-Replace-Url") != "/servers/"+p.server+"?status=failed&tab=activity" {
		t.Fatal(list.Header(), list.Body.String())
	}
}

func TestMonitoringSettingsNeedAnAdministrator(t *testing.T) {
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
	p := newPages(t)
	form := url.Values{"_csrf": {CSRFToken(p.admin.Token)}, "confirmation": {"valheim"}, "bindingRevision": {"1"}}
	response := p.do("POST", "/servers/"+p.server+"/binding-review", p.admin, strings.NewReader(form.Encode()),
		map[string]string{"HX-Request": "true"})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "no pending binding review") {
		t.Fatal(response.Code, response.Body.String())
	}
}

func TestStopFromDetailFollowsTheOperation(t *testing.T) {
	p := newPages(t)
	p.engine.Update(p.docker, func(c *fakedocker.Container) { c.State.Running = true; c.State.Status = "running" })
	p.app.Servers.Refresh(context.Background())
	response := p.do("POST", "/servers/"+p.server+"/stop", p.admin, strings.NewReader(url.Values{"_csrf": {CSRFToken(p.admin.Token)}}.Encode()),
		map[string]string{"HX-Request": "true", "HX-Target": "detail-status"})
	if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("HX-Redirect"), "/servers/"+p.server+"?tab=activity&operation=") {
		t.Fatal(response.Code, response.Header(), response.Body.String())
	}
}

func TestSelectedOperationFragmentStaysOnItsServer(t *testing.T) {
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
