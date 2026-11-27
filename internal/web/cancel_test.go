package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/ops"
)

// A forged cancel from someone who can't see the server, or can see it but
// not restart it, leaves a queued restart queued; someone who may restart
// it cancels it once, and is recorded as having done so.
func TestCancelNeedsTheStartCapability(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	ctx := context.Background()
	server, _ := p.app.Identity.Get(ctx, p.server)
	job, err := p.app.Runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: p.server, ActorID: p.admin.User.ID, Kind: "restart",
		BindingRevision: server.BindingRevision})
	if err != nil {
		t.Fatal(err)
	}
	share := func(principal *auth.Principal, capabilities ...authz.Capability) {
		if err := p.app.Authz.Share(ctx, &p.admin.User, p.admin.TokenHash, principal.User.ID, p.server, capabilities); err != nil {
			t.Fatal(err)
		}
	}
	cancel := func(principal *auth.Principal) int {
		body := url.Values{"_csrf": {CSRFToken(principal.Token)}}.Encode()
		return p.do("POST", "/servers/"+p.server+"/operations/"+job.ID+"/cancel", principal, strings.NewReader(body),
			map[string]string{"HX-Request": "true"}).Code
	}
	stranger := p.user("u-stranger", "stranger", db.RoleMember)
	viewer := p.user("u-viewer", "viewer", db.RoleMember)
	share(viewer, authz.ServerView, authz.ServerStart, authz.ServerStop)
	runner := p.user("u-runner", "runner", db.RoleMember)
	share(runner, authz.ServerView, authz.ServerRestart)
	if code := cancel(stranger); code != http.StatusNotFound {
		t.Fatal("stranger", code)
	}
	if code := cancel(viewer); code != http.StatusForbidden {
		t.Fatal("viewer", code)
	}
	if current, _ := p.app.Runner.Get(ctx, job.ID); current.Status != ops.Queued {
		t.Fatal(current.Status)
	}
	// While Ludock updates itself, the cancel waits too.
	p.app.DB.SetSetting(ctx, nil, "ludock.update", map[string]string{"id": "update", "state": "updating"})
	if code := cancel(runner); code != http.StatusConflict {
		t.Fatal("during an update", code)
	}
	if current, _ := p.app.Runner.Get(ctx, job.ID); current.Status != ops.Queued {
		t.Fatal(current.Status)
	}
	p.app.DB.Exec("DELETE FROM settings WHERE key = 'ludock.update'")
	if code := cancel(runner); code != http.StatusOK {
		t.Fatal("runner", code)
	}
	if current, _ := p.app.Runner.Get(ctx, job.ID); current.Status != ops.Cancelled || current.Result.CancelledBy != "u-runner" {
		t.Fatalf("%+v", current)
	}
	if code := cancel(runner); code != http.StatusConflict {
		t.Fatal("again", code)
	}
}
