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
