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

// Activity lists work, a start pressed in the panel included, only on
// servers the viewer can see.
func TestActivityFollowsSharing(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	job, err := p.app.Runner.Enqueue(context.Background(), nil, ops.EnqueueOptions{ServerID: p.server, ActorID: p.admin.User.ID,
		Kind: "restart", BindingRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	viewer := p.user("u-viewer", "viewer", db.RoleMember)
	list := p.do("GET", "/activity", viewer, nil, nil).Body.String()
	if strings.Contains(list, job.ID) || !strings.Contains(list, "Nothing has happened yet.") {
		t.Fatal(list)
	}
	hidden := p.do("GET", "/activity/"+job.ID, viewer, nil, nil)
	missing := p.do("GET", "/activity/00000000-0000-4000-8000-000000000000", viewer, nil, nil)
	if hidden.Code != http.StatusNotFound || missing.Code != http.StatusNotFound || strings.Contains(hidden.Body.String(), "valheim") {
		t.Fatal(hidden.Code, missing.Code)
	}
	p.grant(viewer, authz.ServerView)
	detail := p.do("GET", "/activity/"+job.ID, viewer, nil, nil).Body.String()
	if !strings.Contains(detail, "valheim") || !strings.Contains(detail, "admin") || strings.Contains(detail, "Everything recorded about it") ||
		!strings.Contains(detail, `hx-trigger="every 2s"`) {
		t.Fatal(detail)
	}
	list = p.do("GET", "/activity?serverId="+p.server+"&status=queued", viewer, nil, nil).Body.String()
	if !strings.Contains(list, "/activity/"+job.ID) {
		t.Fatal(list)
	}
	if bad := p.do("GET", "/activity?from=9&to=1", viewer, nil, nil).Body.String(); !strings.Contains(bad, "The start date must be before the end date.") {
		t.Fatal(bad)
	}
	// A start pressed in the panel is listed like a restart.
	p.app.DB.Exec("UPDATE operations SET status=? WHERE id=?", ops.Succeeded, job.ID)
	if response := p.post("/servers/"+p.server+"/start", p.admin, CSRFToken(p.admin.Token)); response.Code != http.StatusOK {
		t.Fatal(response.Code)
	}
	var started string
	p.app.DB.QueryRow("SELECT id FROM operations WHERE kind='start'").Scan(&started)
	if list = p.do("GET", "/activity", viewer, nil, nil).Body.String(); started == "" || !strings.Contains(list, "/activity/"+started) {
		t.Fatal("start not in Activity", started)
	}
}

// Everything in Activity is the audit log, for administrators: a member who
// asks for it by address sees only work on servers they can use.
func TestActivityEverythingNeedsAnAdministrator(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleMember)
	member := p.do("GET", "/activity?show=everything", operator, nil, nil).Body.String()
	if !strings.Contains(member, "Nothing has happened yet.") || strings.Contains(member, "Signed in") || strings.Contains(member, `name="show"`) {
		t.Fatal(member)
	}
	admin := p.do("GET", "/activity?show=everything", p.admin, nil, nil).Body.String()
	if !strings.Contains(admin, `name="show"`) || !strings.Contains(admin, "audit-list") {
		t.Fatal(admin)
	}
}

// Searching Everything finds an entry by the name the page shows, not only
// by how it is recorded.
func TestActivitySearchMatchesShownNames(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	if err := p.app.DB.WriteAudit(context.Background(), nil, db.AuditEntry{UserID: p.admin.User.ID, Action: "server.recovery.enabled",
		TargetType: "server", TargetID: p.server}); err != nil {
		t.Fatal(err)
	}
	for _, search := range []string{"automatic restart", "recovery"} {
		body := p.do("GET", "/activity?show=everything&action="+url.QueryEscape(search), p.admin, nil, nil).Body.String()
		if !strings.Contains(body, "Turned on automatic restart") {
			t.Fatal(search, body)
		}
	}
	if body := p.do("GET", "/activity?show=everything&action=signed+out", p.admin, nil, nil).Body.String(); strings.Contains(body, "Turned on automatic restart") {
		t.Fatal(body)
	}
	// "Signed in" is not also "Sign-in failed", though one is recorded as
	// the other with a suffix.
	for _, action := range []string{"auth.login", "auth.login.failed"} {
		if err := p.app.DB.WriteAudit(context.Background(), nil, db.AuditEntry{UserID: p.admin.User.ID, Action: action}); err != nil {
			t.Fatal(err)
		}
	}
	if body := p.do("GET", "/activity?show=everything&action=signed+in", p.admin, nil, nil).Body.String(); !strings.Contains(body, "Signed in") ||
		strings.Contains(body, "Sign-in failed") {
		t.Fatal(body)
	}
	// Outages Ludock logged are found by what their rows say, a game that
	// stopped answering apart from a server that was down.
	for _, entry := range []db.AuditEntry{{Action: "server.outage.alerted", Details: map[string]int64{"since": 1}},
		{Action: "server.outage.ended", Details: map[string]int64{"downFor": 240_000}},
		{Action: "server.silence.alerted", Details: map[string]int64{"since": 1}},
		{Action: "server.silence.ended", Details: map[string]int64{"downFor": 240_000}}} {
		entry.TargetType, entry.TargetID = "server", p.server
		if err := p.app.DB.WriteAudit(context.Background(), nil, entry); err != nil {
			t.Fatal(err)
		}
	}
	for search, rows := range map[string][2]string{"Server down": {"Server down", "Game not answering"},
		"Server running again": {"Server running again after 4 minutes down", "Game answering again"},
		"Game not answering":   {"Game not answering", "Server down"},
		"Game answering again": {"Game answering again after 4 minutes", "Server running again"}} {
		body := p.do("GET", "/activity?show=everything&action="+url.QueryEscape(search), p.admin, nil, nil).Body.String()
		if !strings.Contains(body, "<strong>"+rows[0]+"</strong>") || strings.Contains(body, "<strong>"+rows[1]) {
			t.Error(search, body)
		}
	}
}
