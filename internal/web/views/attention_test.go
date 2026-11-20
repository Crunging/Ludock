package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/attention"
)

// A server that is down offers Start in Needs attention, but not while the
// list is only the last known state because Docker can't be reached.
func TestNeedsAttentionStartNeedsDocker(t *testing.T) {
	items := []attention.Item{{ID: "availability:s1", Kind: "availability", ServerID: "s1", ServerName: "valheim", State: "exited", OutageStartedAt: 1, CanStart: true}}
	for unavailable, want := range map[bool]bool{false: true, true: false} {
		var out bytes.Buffer
		if err := NeedsAttention(AttentionView{Items: items, DiscoveryUnavailable: unavailable}).Render(context.Background(), &out); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(out.String(), `hx-post="/servers/s1/start"`); got != want {
			t.Errorf("Docker unreachable %v: Start offered %v\n%s", unavailable, got, out.String())
		}
	}
}
