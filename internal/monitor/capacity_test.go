package monitor

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Crunging/Ludock/internal/identity"
)

func space(percent int64) []DiskSpace {
	return []DiskSpace{{Path: "/data", Total: 100, Used: percent, Available: 100 - percent}}
}

// A disk alert starts at its threshold and lasts until the disk is
// Hysteresis points below it, so a disk hovering at the line alerts once.
func TestDiskAlertHysteresis(t *testing.T) {
	var state DiskState
	steps := []struct {
		percent  int64
		alerting bool
		ended    int
	}{{89, false, 0}, {90, true, 0}, {89, true, 0}, {86, true, 0}, {90, true, 0}, {85, false, 1}, {89, false, 0}}
	for index, step := range steps {
		var ended []Folder
		state, ended = evaluateDisk(state, space(step.percent), 90, int64(index+1))
		if state.alerting() != step.alerting || len(ended) != step.ended {
			t.Fatal(index, step, state, ended)
		}
	}
}

// A folder that can't be measured keeps its last figures and its alert,
// and is marked unknown until a measurement succeeds.
func TestDiskUnknownMeasurementKeepsTheAlert(t *testing.T) {
	state, _ := evaluateDisk(DiskState{}, space(95), 90, 1)
	state, ended := evaluateDisk(state, []DiskSpace{{Path: "/data", Failed: true}}, 90, 2)
	folder := state.Folders[0]
	if len(ended) != 0 || folder.AlertSince != 1 || !folder.Unknown || folder.UsedBytes != 95 || folder.MeasuredAt != 1 {
		t.Fatal(folder, ended)
	}
	state, _ = evaluateDisk(state, space(95), 90, 3)
	if folder := state.Folders[0]; folder.Unknown || folder.AlertSince != 1 || folder.MeasuredAt != 3 {
		t.Fatal(folder)
	}
}

func samples(minute int64, kib ...int64) []memorySample {
	var result []memorySample
	for index, value := range kib {
		result = append(result, memorySample{at: minute - int64(len(kib)-1-index)*60_000, kib: value})
	}
	return result
}

// Memory alerts only when every minute of the window is near the limit,
// ends once the current minute is Hysteresis points below it, and changes
// nothing without a current sample.
func TestMemorySustainedTime(t *testing.T) {
	const minute, limit = 10 * 60_000, 1000
	if got := evaluateMemory(MemoryState{}, samples(minute, 950, 950, 899, 950, 950), minute, limit, 90, 5); got.AlertSince != 0 {
		t.Fatal("a dip inside the window alerted", got)
	}
	if got := evaluateMemory(MemoryState{}, samples(minute, 950, 950, 950, 950), minute, limit, 90, 5); got.AlertSince != 0 {
		t.Fatal("a window missing a minute alerted", got)
	}
	got := evaluateMemory(MemoryState{}, samples(minute, 950, 900, 950, 950, 990), minute, limit, 90, 5)
	if got.AlertSince != minute-4*60_000 {
		t.Fatal("a full window didn't alert", got)
	}
	alerting := MemoryState{AlertSince: 1, Notified: true}
	if got := evaluateMemory(alerting, samples(minute, 860), minute, limit, 90, 5); got != alerting {
		t.Fatal("ended above the hysteresis line", got)
	}
	if got := evaluateMemory(alerting, samples(minute, 850), minute, limit, 90, 5); got != (MemoryState{}) {
		t.Fatal("didn't end below the hysteresis line", got)
	}
	if got := evaluateMemory(alerting, samples(minute-60_000, 100), minute, limit, 90, 5); got != alerting {
		t.Fatal("a missing sample changed the alert", got)
	}
}

// A disk alert is sent once per episode, measured again every minute while
// it lasts, and a recovery follows. An unknown measurement neither sends nor
// clears anything. Stopped servers are measured too.
func TestDiskAlertOncePerEpisode(t *testing.T) {
	s, events, server, _ := setupWith(t, nil)
	percent, fail, measured := int64(95), false, 0
	s.MeasureDisk = func(context.Context, identity.Server) ([]DiskSpace, error) {
		measured++
		if fail {
			return []DiskSpace{{Path: "/data", Failed: true}}, nil
		}
		return space(percent), nil
	}
	check := func(now int64) {
		if err := s.CheckDisk(ctx, state("exited", ""), now); err != nil {
			t.Fatal(err)
		}
	}
	check(1000)
	check(30_000)
	if measured != 1 || len(events.events) != 1 || events.events[0] != "Minecraft: the disk under /data is 95% full (5 B free)." {
		t.Fatal(measured, events.events)
	}
	problems, _ := s.ResourceProblems(ctx)
	if found := problems[server.ID]; len(found) != 1 || found[0].Kind != "disk" || found[0].Path != "/data" || found[0].Since != 1000 {
		t.Fatal(found)
	}
	fail = true
	check(61_000)
	percent, fail = 80, false
	check(90_000)
	if measured != 2 || len(events.events) != 1 {
		t.Fatal("measured again too soon", measured, events.events)
	}
	check(121_000)
	if measured != 3 || len(events.events) != 2 || events.events[1] != "Minecraft: the disk under /data has space again (80% full)." {
		t.Fatal(measured, events.events)
	}
	check(200_000)
	if measured != 3 {
		t.Fatal("measured more often than DiskInterval without an alert")
	}
	if problems, _ := s.ResourceProblems(ctx); len(problems) != 0 {
		t.Fatal(problems)
	}
}

// Changing a disk threshold ends the alert without a recovery and measures
// again on the next tick.
func TestDiskThresholdChange(t *testing.T) {
	s, events, server, configure := setupWith(t, nil)
	s.MeasureDisk = func(context.Context, identity.Server) ([]DiskSpace, error) { return space(93), nil }
	s.CheckDisk(ctx, state("running", ""), 1000)
	policy := DefaultPolicy()
	policy.Enabled, policy.DiskPercent = true, 95
	configure(policy)
	if problems, _ := s.ResourceProblems(ctx); len(problems) != 0 {
		t.Fatal(problems)
	}
	s.CheckDisk(ctx, state("running", ""), 2000)
	disk, _ := s.Disk(ctx, server.ID)
	if len(events.events) != 1 || disk.CheckedAt != 2000 || disk.Folders[0].AlertSince != 0 {
		t.Fatal(events.events, disk)
	}
}

// A measurement skipped for other work changes nothing and is tried on the
// next tick.
func TestDiskMeasurementSkipped(t *testing.T) {
	s, events, server, _ := setupWith(t, nil)
	s.MeasureDisk = func(context.Context, identity.Server) ([]DiskSpace, error) { return nil, errors.New("busy") }
	s.CheckDisk(ctx, state("running", ""), 1000)
	if disk, _ := s.Disk(ctx, server.ID); disk.CheckedAt != 0 || len(events.events) != 0 {
		t.Fatal(disk, events.events)
	}
}

func addSamples(t *testing.T, s *Service, serverID string, minute int64, kib ...int64) {
	t.Helper()
	for _, sample := range samples(minute, kib...) {
		if _, err := s.DB.ExecContext(ctx, "INSERT INTO resource_samples(server_id,at,cpu,memory) VALUES(?,?,0,?)", serverID, sample.at, sample.kib); err != nil {
			t.Fatal(err)
		}
	}
}

// Memory alerts once a running server with a limit stays near it, and
// recovers when it stops. A server without a limit never alerts.
func TestMemoryAlert(t *testing.T) {
	s, events, server, _ := setupWith(t, func(policy *Policy) { policy.MemoryMinutes = 2 })
	const minute = 10 * 60_000
	addSamples(t, s, server.ID, minute, 950, 960)
	limits := map[string]int64{server.ID: 0}
	s.CheckMemory(ctx, limits, state("running", ""), minute)
	if len(events.events) != 0 {
		t.Fatal("a server without a limit alerted", events.events)
	}
	limits[server.ID] = 1000
	s.CheckMemory(ctx, limits, state("running", ""), minute)
	s.CheckMemory(ctx, limits, state("running", ""), minute)
	if !slices.Equal(events.events, []string{"Minecraft has used over 90% of its memory limit for 2 minutes."}) {
		t.Fatal(events.events)
	}
	s.CheckMemory(ctx, map[string]int64{}, nil, minute+60_000)
	if problems, _ := s.ResourceProblems(ctx); len(problems[server.ID]) != 1 {
		t.Fatal("a Docker outage cleared the alert", problems)
	}
	s.CheckMemory(ctx, map[string]int64{}, state("exited", ""), minute+60_000)
	if len(events.events) != 2 || events.events[1] != "Minecraft’s memory use is no longer near its limit." {
		t.Fatal(events.events)
	}
}

// Monitoring turned off while a disk is measured keeps the measurement from
// saving or sending an alert under the old policy.
func TestPolicySavedDuringADiskMeasurement(t *testing.T) {
	s, events, server, configure := setupWith(t, nil)
	s.MeasureDisk = func(context.Context, identity.Server) ([]DiskSpace, error) {
		configure(DefaultPolicy())
		return space(95), nil
	}
	s.CheckDisk(ctx, state("running", ""), 1000)
	if disk, _ := s.Disk(ctx, server.ID); len(events.events) != 0 || disk.CheckedAt != 0 {
		t.Fatal(events.events, disk)
	}
}

// A disk exactly at the recovery line ends the alert, though 55 of 100
// computes as 55.00000000000001.
func TestAlertEndsExactlyAtTheLine(t *testing.T) {
	state, _ := evaluateDisk(DiskState{}, space(60), 60, 1)
	if state, _ = evaluateDisk(state, space(55), 60, 2); state.alerting() {
		t.Fatal("55% kept a 60% alert")
	}
}
