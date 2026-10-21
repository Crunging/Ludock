package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
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
