package views

import (
	"context"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/notify"
)

// A destination never saved offers alerts on, so its first save turns it on;
// one saved off, even before it has all it needs, stays off.
func TestNotificationDestinationStartsOn(t *testing.T) {
	render := func(c notify.Configuration) string {
		var out strings.Builder
		if err := NotificationDestination(NotificationDestinationView{Configuration: c}).Render(context.Background(), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	checked := `name="enabled" value="true" checked`
	if text := render(notify.Configuration{Destination: notify.Webhook}); !strings.Contains(text, checked) {
		t.Fatal("a new destination should start on")
	}
	savedOff := notify.Configuration{Destination: notify.Webhook, Stored: true}
	if text := render(savedOff); strings.Contains(text, checked) {
		t.Fatal("a destination saved off should stay off")
	}
}

// A destination alerts aren't reaching always says so in one sentence, and
// says why only once: after a failed test, the test's result gives the reason.
func TestNotificationDestinationFailingSaysWhyOnce(t *testing.T) {
	failing := NotificationDestinationView{Configuration: notify.Configuration{Destination: notify.Webhook, Enabled: true, Configured: true, Stored: true},
		Health: notify.Health{Failed: true, Failing: true, Reason: "Unable to reach the webhook.", LastTried: 1}}
	render := func(n NotificationDestinationView) string {
		var out strings.Builder
		if err := NotificationDestination(n).Render(context.Background(), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if text := render(failing); strings.Count(text, "Unable to reach the webhook.") != 1 ||
		!strings.Contains(text, "<p>Alerts aren’t reaching Webhook. Unable to reach the webhook. Last tried") {
		t.Fatal(text)
	}
	failing.Test = &notify.Delivery{State: "failed", LastFailure: "Unable to reach the webhook."}
	if text := render(failing); strings.Count(text, "Unable to reach the webhook.") != 1 ||
		!strings.Contains(text, "<p>Alerts aren’t reaching Webhook. Last tried") {
		t.Fatal(text)
	}
}
