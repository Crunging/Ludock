package web

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/monitor"
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/schedtime"
	"github.com/Crunging/Ludock/internal/schedules"
	"github.com/Crunging/Ludock/internal/web/views"
)

type postingAs struct {
	session *auth.Principal
	csrf    string
}

// Players can't reach loopback, private, or link-local addresses, nor names
// only a local network resolves, from elsewhere.
func TestLocalHost(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"127.0.0.1", "::1", "localhost", "panel.localhost", "10.0.0.5", "172.16.3.4", "192.168.1.20",
		"fd00::1", "fe80::1", "169.254.1.1", "::ffff:192.168.1.2", "0.0.0.0", "nas", "nas.local", "box.lan", "server.home.arpa",
		"NAS.LOCAL.", "[::1]", "[fd00::1]"} {
		if !localHost(host) {
			t.Error("not local:", host)
		}
	}
	for _, host := range []string{"203.0.113.10", "2001:db8::1", "play.example.com", "100.100.1.1", "[2001:db8::1]", ""} {
		if localHost(host) {
			t.Error("local:", host)
		}
	}
	if !loopbackHost("127.0.0.1") || !loopbackHost("[::1]") || !loopbackHost("localhost") || loopbackHost("192.168.1.20") {
		t.Fatal("loopback")
	}
}

// Nightly backups start at 04:00 a quarter hour apart, skip times other
// backups use, and keep running every night and stay apart across
// daylight-saving changes.
func TestBackupTimesStaggerAcrossDaylightSaving(t *testing.T) {
	t.Parallel()
	if times := backupTimes(nil, 3); !slices.Equal(times, []string{"04:00", "04:15", "04:30"}) {
		t.Fatal(times)
	}
	if times := backupTimes([]string{"04:00", "04:30"}, 3); !slices.Equal(times, []string{"04:15", "04:45", "05:00"}) {
		t.Fatal(times)
	}
	if times := backupTimes(nil, 9); times[8] != "04:05" {
		t.Fatal(times)
	}
	if times := backupTimes(nil, 30); len(times) != 30 || times[24] != "04:00" {
		t.Fatal(times)
	}
	times := backupTimes(nil, 8)
	for _, zone := range []string{"America/New_York", "Europe/Berlin", "Australia/Sydney", "Australia/Lord_Howe", "America/Santiago",
		"America/Havana", "Asia/Beirut", "Africa/Casablanca", "Pacific/Auckland", "America/St_Johns"} {
		location, _ := schedtime.Location(zone)
		for day := 0; day < 366; day++ {
			// 03:00 exists on every day in these zones; midnight doesn't in Santiago.
			before := time.Date(2026, 1, 1+day, 3, 0, 0, 0, location)
			date := before.Format("2006-01-02 ")
			var previous int64
			for index, at := range times {
				input := schedtime.Input{Kind: "recurring", Enabled: true, Time: at, Timezone: zone, Days: []int{0, 1, 2, 3, 4, 5, 6}}
				next, ok := schedtime.NextRun(input, before.UnixMilli(), "")
				if run := time.UnixMilli(next).In(location); !ok || run.Format("2006-01-02 15:04") != date+at {
					t.Fatalf("%s %s%s: %v %v", zone, date, at, run, ok)
				}
				if index > 0 && next-previous != 15*60_000 {
					t.Fatalf("%s %s%s is %d ms after the one before", zone, date, at, next-previous)
				}
				previous = next
			}
		}
	}
}

// Requests the list never sends: a member's, one without the session's
// CSRF token, an unknown item, and a missing time zone change nothing.
// Automatic restart stays off.
func TestChecklistRefusesForgedRequests(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	ctx := context.Background()
	member := p.user("u-member", "member", db.RoleMember)
	post := func(target string, principal *postingAs, values url.Values) int {
		values.Set("_csrf", principal.csrf)
		return p.do("POST", target, principal.session, strings.NewReader(values.Encode()), map[string]string{"HX-Request": "true"}).Code
	}
	asMember := &postingAs{member, CSRFToken(member.Token)}
	forged := &postingAs{p.admin, "not-a-token"}
	asAdmin := &postingAs{p.admin, CSRFToken(p.admin.Token)}
	for _, attempt := range []struct {
		as     *postingAs
		target string
		values url.Values
		status int
	}{
		{asMember, "/checklist/backups", url.Values{"timezone": {"UTC"}}, http.StatusForbidden},
		{forged, "/checklist/backups", url.Values{"timezone": {"UTC"}}, http.StatusForbidden},
		{asAdmin, "/checklist/backups", url.Values{"timezone": {"Mars/Olympus"}}, http.StatusBadRequest},
		{asMember, "/checklist/recovery", url.Values{"server": {p.server}}, http.StatusForbidden},
		{forged, "/checklist/recovery", url.Values{"server": {p.server}}, http.StatusForbidden},
		{asMember, "/checklist/address/dismiss", url.Values{}, http.StatusForbidden},
		{forged, "/checklist/address/dismiss", url.Values{}, http.StatusForbidden},
		{asAdmin, "/checklist/everything/dismiss", url.Values{}, http.StatusNotFound},
	} {
		if status := post(attempt.target, attempt.as, attempt.values); status != attempt.status {
			t.Fatal(attempt.target, attempt.values, status)
		}
	}
	if dismissed := p.web.checklistDismissed(ctx); len(dismissed) != 0 {
		t.Fatal(dismissed)
	}
	if list, err := p.app.Schedules.List(ctx, &p.admin.User, p.server); err != nil || len(list) != 0 {
		t.Fatal(list, err)
	}
	if availability, err := p.app.Monitor.Get(ctx, p.server); err != nil || availability.Policy.Recover {
		t.Fatal(availability, err)
	}
	if response := p.do("GET", "/fragments/checklist", member, nil, map[string]string{"HX-Request": "true"}); response.Code != http.StatusForbidden {
		t.Fatal(response.Code)
	}
	// The administrator's own dismissal is remembered once and audited.
	for range 2 {
		if status := post("/checklist/address/dismiss", asAdmin, url.Values{}); status != http.StatusOK {
			t.Fatal(status)
		}
	}
	if dismissed := p.web.checklistDismissed(ctx); !slices.Equal(dismissed, []string{views.ChecklistAddress}) {
		t.Fatal(dismissed)
	}
}

// A nightly backup whose owner was disabled is suspended: its row says so,
// and the checklist points at it instead of adding a second beside it. A
// server started with --rm counts among those Ludock can't back up.
func TestChecklistBackupsSkipSuspendedSchedules(t *testing.T) {
	t.Parallel()
	p := newDataPages(t)
	ctx := context.Background()
	if err := p.app.DB.SetSetting(ctx, p.app.DB, "backups", map[string]any{"destination": t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	removed := p.engine.AddContainer(fakedocker.GameOptions{Name: "minecraft", Image: "itzg/minecraft-server",
		Mounts: []docker.MountPoint{{Type: "volume", Name: "minecraft-data", Destination: "/data", RW: true}}})
	p.engine.Update(removed, func(c *fakedocker.Container) { c.HostConfig = map[string]any{"AutoRemove": true} })
	if _, err := p.app.Servers.RefreshNow(ctx); err != nil {
		t.Fatal(err)
	}
	other := p.user("u-other", "other", db.RoleAdmin)
	nightly := schedules.Input{Kind: "recurring", Action: "backup", Enabled: true, Time: "04:00", Timezone: "UTC", Days: []int{0, 1, 2, 3, 4, 5, 6}}
	if _, err := p.app.Schedules.Create(ctx, &other.User, other.TokenHash, p.server, nightly); err != nil {
		t.Fatal(err)
	}
	checklist := func() string {
		return p.do("GET", "/fragments/checklist", p.admin, nil, map[string]string{"HX-Request": "true"}).Body.String()
	}
	if body := checklist(); strings.Contains(body, "Back up your servers") {
		t.Fatal(body)
	}
	if err := dbtest.UpdateUserAccess(ctx, p.app.DB, other.User.ID, db.RoleAdmin, true); err != nil {
		t.Fatal(err)
	}
	if page := p.do("GET", "/servers/"+p.server+"?tab=schedules", p.admin, nil, nil).Body.String(); !strings.Contains(page, "Suspended") {
		t.Fatal(page)
	}
	body := checklist()
	if !strings.Contains(body, "Backups are suspended for 1 server") || !strings.Contains(body, "Ludock can’t back up 1 other server") ||
		strings.Contains(body, `hx-post="/checklist/backups"`) {
		t.Fatal(body)
	}
	// With nothing else to schedule, the item's button is its one link.
	if strings.Count(body, "?tab=schedules") != 1 || !strings.Contains(body, "Review schedule") {
		t.Fatal(body)
	}
	values := url.Values{"_csrf": {CSRFToken(p.admin.Token)}, "timezone": {"UTC"}, "server": {p.server}}
	p.do("POST", "/checklist/backups", p.admin, strings.NewReader(values.Encode()), map[string]string{"HX-Request": "true"})
	if list, err := p.app.Schedules.List(ctx, &p.admin.User, p.server); err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
}

// Automatic restart is offered for a monitored server whose game answers,
// but not while its alerts are paused for maintenance, when restarts can't run.
func TestChecklistRecoverySkipsMaintenance(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	ctx := context.Background()
	offered := func() bool {
		body := p.do("GET", "/fragments/checklist", p.admin, nil, map[string]string{"HX-Request": "true"}).Body.String()
		return strings.Contains(body, "/checklist/recovery")
	}
	if !offered() {
		t.Fatal("not offered for a monitored server")
	}
	policy := monitor.DefaultPolicy()
	policy.Maintenance = true
	if _, err := p.app.Monitor.Configure(ctx, p.admin.User.ID, p.admin.TokenHash, p.server, policy); err != nil {
		t.Fatal(err)
	}
	if offered() {
		t.Fatal("offered during maintenance")
	}
}

// "Get alerts" is done once a destination is on, unless everything sent to
// it has failed; then the item names it and why. A destination that worked
// before and fails now is left to Needs attention.
func TestChecklistAlertsWorking(t *testing.T) {
	t.Parallel()
	on := func(d notify.Destination) notify.Configuration {
		return notify.Configuration{Destination: d, Enabled: true, Configured: true}
	}
	refused := notify.Health{Failed: true, Failing: true, Reason: "Unable to reach ntfy."}
	for _, c := range []struct {
		name           string
		configurations []notify.Configuration
		healths        map[notify.Destination]notify.Health
		on             bool
		unproven       notify.Destination
	}{
		{"none on", []notify.Configuration{{Destination: notify.Ntfy, Configured: true}}, nil, false, ""},
		{"on, nothing sent yet", []notify.Configuration{on(notify.Ntfy)}, nil, true, ""},
		{"only failures", []notify.Configuration{on(notify.Ntfy)}, map[notify.Destination]notify.Health{notify.Ntfy: refused}, false, notify.Ntfy},
		{"one failed retry", []notify.Configuration{on(notify.Ntfy)},
			map[notify.Destination]notify.Health{notify.Ntfy: {Failed: true, Reason: "Unable to reach ntfy."}}, false, notify.Ntfy},
		{"worked before", []notify.Configuration{on(notify.Ntfy)},
			map[notify.Destination]notify.Health{notify.Ntfy: {Failed: true, Failing: true, Delivered: true}}, true, ""},
		{"another works", []notify.Configuration{on(notify.Ntfy), on(notify.Email)},
			map[notify.Destination]notify.Health{notify.Ntfy: refused, notify.Email: {Delivered: true}}, true, ""},
	} {
		working, unproven, reason := alertsWorking(c.configurations, c.healths)
		if working != c.on || unproven != c.unproven || (unproven != "" && reason != "Unable to reach ntfy.") {
			t.Errorf("%s: %v %q %q", c.name, working, unproven, reason)
		}
	}
	items := checklistItems(checklistState{TwoFactor: true, Unproven: notify.Ntfy, UnprovenReason: "Unable to reach ntfy."})
	if len(items) != 1 || items[0].Key != views.ChecklistAlerts || items[0].Destination != notify.Ntfy {
		t.Fatal(items)
	}
}
