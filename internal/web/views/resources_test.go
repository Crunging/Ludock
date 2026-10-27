package views

import (
	"testing"

	"github.com/Crunging/Ludock/internal/resources"
)

// An hour with a lone sample, a missed minute that is bridged, and a
// longer gap draws a dot and two runs of line, each with its own area.
func TestResourcePlotBreaksAtGaps(t *testing.T) {
	minute := int64(resources.Interval)
	h := resources.History{Range: resources.RangeFor("hour"), Start: 0, End: 60 * minute}
	for _, at := range []int64{0, 10, 11, 13, 30, 31} {
		h.Points = append(h.Points, resources.Point{At: at * minute})
	}
	values := []float64{5, 10, 0, 10, 20, 5}
	line, area, dots := plot(h, values, 20)
	if dots != "M0.0 75.00h0" {
		t.Fatalf("dots = %q", dots)
	}
	if line != "M166.7 50.00L183.3 100.00L216.7 50.00M500.0 0.00L516.7 75.00" {
		t.Fatalf("line = %q", line)
	}
	if area != "M166.7 100L166.7 50.00L183.3 100.00L216.7 50.00L216.7 100ZM500.0 100L500.0 0.00L516.7 75.00L516.7 100Z" {
		t.Fatalf("area = %q", area)
	}
	if !resourceGaps(h) {
		t.Fatal("no gaps")
	}
}

// Axis tops round up to a short number with room above the highest value.
func TestNiceCeil(t *testing.T) {
	for value, want := range map[float64]float64{0: 1, 10: 20, 9: 10, 23: 25, 140: 200, 1.4: 2, 0.3: 0.5} {
		if got := niceCeil(value); got != want {
			t.Errorf("niceCeil(%v) = %v, want %v", value, got, want)
		}
	}
}
