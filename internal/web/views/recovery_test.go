package views

import (
	"context"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/monitor"
	"github.com/Crunging/Ludock/internal/servers"
)

// A recovery restart whose outcome Ludock stopped waiting for, because the
// server was stopped or started again by someone else, no longer says it is
// waiting for the game to answer.
func TestRecoveryStatusWaitsOnlyWhilePending(t *testing.T) {
	render := func(r monitor.Recovery) string {
		var out strings.Builder
		if err := RecoveryStatus(r, true).Render(context.Background(), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	restarted := monitor.Recovery{Last: monitor.RecoveryRestarted, RestartedAt: 1_000}
	if text := render(restarted); !strings.Contains(text, "because the game stopped answering") || strings.Contains(text, "Waiting") {
		t.Fatal(text)
	}
	restarted.Pending = true
	if text := render(restarted); !strings.Contains(text, "Waiting for it to answer.") {
		t.Fatal(text)
	}
}

// A server that exited isn't told automatic restart wasn't needed: it says
// it acts only on a running game. The guide covers restart policies.
func TestRecoveryStatusExitedServer(t *testing.T) {
	for _, r := range []monitor.Recovery{{}, {Last: monitor.RecoveryRestarted, RestartedAt: 1_000, Pending: true}} {
		var out strings.Builder
		if err := RecoveryStatus(r, false).Render(context.Background(), &out); err != nil {
			t.Fatal(err)
		}
		if text := out.String(); strings.Contains(text, "Not needed") || strings.Contains(text, "Waiting") || !strings.Contains(text, "On while the server runs") ||
			strings.Contains(text, "restart policy") {
			t.Fatal(text)
		}
	}
}

// A server that stopped outside Ludock is offered what the viewer can do
// there: Start, and reading its logs, each only with access to it. A share
// with the game console but not the logs isn't sent to them.
func TestOutageAdviceFollowsAccess(t *testing.T) {
	for _, c := range []struct {
		permissions []authz.Capability
		canStart    bool
		want        string
	}{
		{[]authz.Capability{authz.ServerStart, authz.LogsRead}, true, "Ludock didn’t stop it. Start it here, or <a class=\"text-link\" href=\"/console/s1?mode=logs\">read its logs</a> to see why."},
		{[]authz.Capability{authz.ConsoleExecute}, false, "Ludock didn’t stop it.</p>"},
		{[]authz.Capability{authz.LogsRead}, false, "Ludock didn’t stop it. <a class=\"text-link\" href=\"/console/s1?mode=logs\">Read its logs</a> to see why."},
	} {
		d := ServerDetail{Row: ServerRow{Server: servers.View{ID: "s1", Permissions: c.permissions, BindingStatus: "active",
			Container: servers.Container{State: "exited"}}, CanStart: c.canStart, ConsoleLabel: "Console"},
			Availability: &monitor.Availability{Policy: monitor.Policy{Enabled: true},
				State: monitor.State{OutageStartedAt: 1, Since: 1, Notified: true, LastState: "exited"}}, Now: 1_000_000}
		var out strings.Builder
		if err := AvailabilityStatus(d).Render(context.Background(), &out); err != nil {
			t.Fatal(err)
		}
		if text := out.String(); !strings.Contains(text, c.want) {
			t.Errorf("%v: %s", c.permissions, text)
		}
	}
}

// Monitoring never shows an empty note for an outage, whatever state and
// health the server is in and whatever the viewer may do. It blames a health
// check only when Docker reports one that hasn't passed.
func TestOutageNoteForEveryState(t *testing.T) {
	for _, state := range []string{"running", "exited", "created", "restarting", "paused", "removing", "dead", "missing", "ambiguous", "unknown", ""} {
		for _, health := range []string{"", "starting", "healthy", "unhealthy"} {
			for _, permissions := range [][]authz.Capability{nil, {authz.ServerStart, authz.ServerStop, authz.LogsRead}} {
				d := ServerDetail{Row: ServerRow{Server: servers.View{Permissions: permissions, BindingStatus: "active",
					Container: servers.Container{State: state, Health: health}}, CanStart: permissions != nil, CanStop: permissions != nil},
					Availability: &monitor.Availability{Policy: monitor.Policy{Enabled: true},
						State: monitor.State{OutageStartedAt: 1, Since: 1, Notified: true, LastState: state}}, Now: 1_000_000}
				var out strings.Builder
				if err := AvailabilityStatus(d).Render(context.Background(), &out); err != nil {
					t.Fatal(err)
				}
				_, note, _ := strings.Cut(out.String(), `class="section-note">`)
				note, _, _ = strings.Cut(note, "</p>")
				failing := state == "running" && (health == "starting" || health == "unhealthy")
				if strings.TrimSpace(note) == "" || strings.Contains(note, " as .") || strings.Contains(note, "health check") != failing {
					t.Errorf("state %q, health %q, permissions %v: note %q", state, health, permissions, note)
				}
			}
		}
	}
}
