package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/schedules"
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

func TestScheduleEditConflictKeepsTheDraft(t *testing.T) {
	p := newPages(t)
	created, err := p.app.Schedules.Create(context.Background(), &p.admin.User, p.server, schedules.Input{Kind: "recurring",
		Action: "restart", Enabled: true, Time: "06:00", Timezone: "UTC", Days: []int{0}})
	if err != nil {
		t.Fatal(err)
	}
	edit := p.do("GET", "/servers/"+p.server+"?tab=schedules&edit="+created.ID, p.admin, nil, nil).Body.String()
	if !strings.Contains(edit, "Edit schedule") || !strings.Contains(edit, `value="06:00"`) {
		t.Fatal(edit)
	}
	// Someone pauses it meanwhile.
	if _, err := p.app.Schedules.SetEnabled(context.Background(), &p.admin.User, p.server, created.ID, created.Revision, false); err != nil {
		t.Fatal(err)
	}
	draft := scheduleForm(map[string][]string{"action": {"restart"}, "time": {"07:30"}, "revision": {"1"}, "enabled": {"true"}})
	response := p.action("/servers/"+p.server+"/schedules/"+created.ID, p.admin, draft)
	body := readBody(t, response)
	if response.StatusCode != http.StatusConflict || !strings.Contains(body, "changed elsewhere") || !strings.Contains(body, `value="07:30"`) {
		t.Fatal(response.StatusCode, body)
	}
	draft.Set("keepDraft", "2")
	kept := readBody(t, p.action("/servers/"+p.server+"/schedules/"+created.ID, p.admin, draft))
	if !strings.Contains(kept, `name="revision" value="2"`) || !strings.Contains(kept, "stays paused") || strings.Contains(kept, "changed elsewhere") {
		t.Fatal(kept)
	}
	draft.Del("keepDraft")
	draft.Set("revision", "2")
	draft.Set("enabled", "false")
	if response := p.action("/servers/"+p.server+"/schedules/"+created.ID, p.admin, draft); response.StatusCode != http.StatusOK {
		t.Fatal(response.StatusCode, readBody(t, response))
	}
	list, _ := p.app.Schedules.List(context.Background(), &p.admin.User, p.server)
	if list[0].Time != "07:30" || list[0].Enabled {
		t.Fatal(list[0].Input)
	}
}
