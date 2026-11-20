package views

import (
	"context"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/monitor"
)

// A recovery restart whose outcome Ludock stopped waiting for, because the
// server was stopped or started again by someone else, no longer says it is
// waiting for the game to answer.
func TestRecoveryStatusWaitsOnlyWhilePending(t *testing.T) {
	render := func(r monitor.Recovery) string {
		var out strings.Builder
		if err := RecoveryStatus(r).Render(context.Background(), &out); err != nil {
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
