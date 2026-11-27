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
