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
)

func scheduleForm(values map[string][]string) url.Values {
	form := url.Values{"kind": {"recurring"}, "action": {"start"}, "time": {"08:00"}, "timezone": {"Europe/Berlin"},
		"days": {"1", "2"}}
	for key, value := range values {
		form[key] = value
	}
	return form
}

func TestSchedulesFollowActionGrants(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleOperator)
	p.grant(operator, authz.ServerView, authz.SchedulesManage, authz.ServerStart)
	page := p.do("GET", "/servers/"+p.server+"?tab=schedules", operator, nil, nil).Body.String()
	if !strings.Contains(page, `<option value="start"`) || strings.Contains(page, `<option value="stop"`) {
		t.Fatal(page)
	}
	// A schedule cannot use an action the owner was not granted.
	response := p.action("/servers/"+p.server+"/schedules", operator, scheduleForm(map[string][]string{"action": {"stop"}}))
	if response.StatusCode < 400 || !strings.Contains(readBody(t, response), "schedule-form") {
		t.Fatal(response.StatusCode)
	}
	response = p.action("/servers/"+p.server+"/schedules", operator, scheduleForm(nil))
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("HX-Redirect"), "saved=created") {
		t.Fatal(response.StatusCode, readBody(t, response))
	}
	list, _ := p.app.Schedules.List(context.Background(), &operator.User, p.server)
	if len(list) != 1 || list[0].Timezone != "Europe/Berlin" || len(list[0].Days) != 2 {
		t.Fatal(list)
	}
	// Viewers without the schedule grant cannot reach the tab or save.
	viewer := p.user("u-viewer", "viewer", db.RoleViewer)
	p.grant(viewer, authz.ServerView)
	if response := p.action("/servers/"+p.server+"/schedules", viewer, scheduleForm(nil)); response.StatusCode != http.StatusForbidden {
		t.Fatal(response.StatusCode)
	}
	if strings.Contains(p.do("GET", "/servers/"+p.server+"?tab=schedules", viewer, nil, nil).Body.String(), "schedule-form") {
		t.Fatal("viewer sees the schedule form")
	}
}

// A server without a game console offers no command schedules, and a forged
// request for one saves nothing.
func TestCommandSchedulesNeedAGameConsole(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleOperator)
	p.grant(operator, authz.ServerView, authz.SchedulesManage, authz.ConsoleExecute, authz.ServerStart)
	if page := p.do("GET", "/servers/"+p.server+"?tab=schedules", operator, nil, nil).Body.String(); strings.Contains(page, `<option value="command"`) {
		t.Fatal("offered a command schedule without a game console")
	}
	response := p.action("/servers/"+p.server+"/schedules", operator,
		scheduleForm(map[string][]string{"action": {"command"}, "command": {"save"}}))
	if body := readBody(t, response); response.StatusCode != http.StatusBadRequest || !strings.Contains(body, "accept commands") {
		t.Fatal(response.StatusCode, body)
	}
	if list, _ := p.app.Schedules.List(context.Background(), &operator.User, p.server); len(list) != 0 {
		t.Fatal(list)
	}
}

// A server whose players Ludock can't read, here with its status query
// turned off, offers no waiting for an empty server, and a forged request
// for one saves nothing.
func TestWaitingNeedsReadablePlayers(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	if page := p.do("GET", "/servers/"+p.server+"?tab=schedules", p.admin, nil, nil).Body.String(); !strings.Contains(page, `name="waitForEmpty"`) {
		t.Fatal("a server with a status query was not offered waiting")
	}
	p.engine.Update(p.docker, func(c *fakedocker.Container) { c.Config.Labels = map[string]string{"ludock.query": "disabled"} })
	if _, err := p.app.Servers.RefreshNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	page := p.do("GET", "/servers/"+p.server+"?tab=schedules", p.admin, nil, nil).Body.String()
	if strings.Contains(page, `name="waitForEmpty"`) || !strings.Contains(page, "needs a game whose players Ludock can read") {
		t.Fatal("offered waiting without readable players")
	}
	response := p.action("/servers/"+p.server+"/schedules", p.admin,
		scheduleForm(map[string][]string{"action": {"stop"}, "waitForEmpty": {"true"}, "waitMinutes": {"30"}, "waitThen": {"skip"}}))
	if body := readBody(t, response); response.StatusCode != http.StatusBadRequest {
		t.Fatal(response.StatusCode, body)
	}
	if list, _ := p.app.Schedules.List(context.Background(), &p.admin.User, p.server); len(list) != 0 {
		t.Fatal(list)
	}
}
