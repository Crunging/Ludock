package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/ops"
)

func TestOperationHistoryFollowsGrants(t *testing.T) {
	p := newPages(t)
	job, err := p.app.Runner.Enqueue(context.Background(), nil, ops.EnqueueOptions{ServerID: p.server, ActorID: p.admin.User.ID,
		Kind: "restart", BindingRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	viewer := p.user("u-viewer", "viewer", db.RoleViewer)
	list := p.do("GET", "/operations", viewer, nil, nil).Body.String()
	if strings.Contains(list, job.ID) || !strings.Contains(list, "No operations yet.") {
		t.Fatal(list)
	}
	hidden := p.do("GET", "/operations/"+job.ID, viewer, nil, nil)
	missing := p.do("GET", "/operations/00000000-0000-4000-8000-000000000000", viewer, nil, nil)
	if hidden.Code != http.StatusNotFound || missing.Code != http.StatusNotFound || strings.Contains(hidden.Body.String(), "valheim") {
		t.Fatal(hidden.Code, missing.Code)
	}
	p.grant(viewer, authz.ServerView)
	detail := p.do("GET", "/operations/"+job.ID, viewer, nil, nil).Body.String()
	if !strings.Contains(detail, "valheim") || !strings.Contains(detail, "admin") || strings.Contains(detail, "Related audit events") ||
		!strings.Contains(detail, `hx-trigger="every 2s"`) {
		t.Fatal(detail)
	}
	list = p.do("GET", "/operations?serverId="+p.server+"&status=queued", viewer, nil, nil).Body.String()
	if !strings.Contains(list, "/operations/"+job.ID) {
		t.Fatal(list)
	}
	if bad := p.do("GET", "/operations?from=9&to=1", viewer, nil, nil).Body.String(); !strings.Contains(bad, "From must be before or equal to To.") {
		t.Fatal(bad)
	}
}

func TestAuditNeedsAnAdministrator(t *testing.T) {
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleOperator)
	if response := p.do("GET", "/audit", operator, nil, nil); response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/" {
		t.Fatal(response.Code, response.Header())
	}
	page := p.do("GET", "/audit?action=auth.login", p.admin, nil, nil).Body.String()
	if !strings.Contains(page, "auth.login") || !strings.Contains(page, "Audit log") {
		t.Fatal(page)
	}
}
