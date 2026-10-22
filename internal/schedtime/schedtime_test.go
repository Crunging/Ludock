package schedtime

import (
	"testing"
	"time"
)

func at(value string) int64 {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return t.UnixMilli()
}

var recurring = Input{Kind: "recurring", Enabled: true, Time: "09:30", Days: []int{1, 3}, Timezone: "UTC"}

func with(change func(*Input)) Input {
	copy := recurring
	copy.Days = append([]int(nil), recurring.Days...)
	change(&copy)
	return copy
}

func next(t *testing.T, input Input, now int64, last string, want int64) {
	t.Helper()
	got, ok := NextRun(input, now, last)
	if want == 0 && ok || want != 0 && (!ok || got != want) {
		t.Errorf("NextRun(%v, %d, %q) = %d %v, want %d", input, now, last, got, ok, want)
	}
}

func TestFallBackOccurrencesShareASlot(t *testing.T) {
	fall := with(func(i *Input) { i.Timezone, i.Days, i.Time = "America/Los_Angeles", []int{0}, "01:30" })
	first, second := at("2026-11-01T08:30:00Z"), at("2026-11-01T09:30:00Z")
	slot := Slot(fall, first)
	if slot == "" || slot != Slot(fall, second) {
		t.Fatal(slot, Slot(fall, second))
	}
	next(t, fall, first-60_000, "", first)
	next(t, fall, first+15_000, "", first)
	next(t, fall, first+60_000, "", second)
	next(t, fall, second+15_000, "", second)
	next(t, fall, first, slot, at("2026-11-08T09:30:00Z"))
	next(t, fall, second, slot, at("2026-11-08T09:30:00Z"))
}

func TestSpringGapLooksBeyondAWeek(t *testing.T) {
	spring := with(func(i *Input) { i.Timezone, i.Days, i.Time = "America/Los_Angeles", []int{0}, "02:30" })
	next(t, spring, at("2026-03-01T10:31:00Z"), "", at("2026-03-15T09:30:00Z"))
	next(t, spring, at("2026-03-08T09:59:00Z"), "", at("2026-03-15T09:30:00Z"))
	if Slot(spring, at("2026-03-08T10:30:00Z")) != "" {
		t.Fatal("slot inside a spring gap")
	}
}

func TestHalfHourTransitions(t *testing.T) {
	fall := with(func(i *Input) { i.Timezone, i.Days, i.Time = "Australia/Lord_Howe", []int{0}, "01:45" })
	first, second := at("2026-04-04T14:45:00Z"), at("2026-04-04T15:15:00Z")
	if Slot(fall, first) != Slot(fall, second) {
		t.Fatal("half-hour fall-back slots differ")
	}
	next(t, fall, first+60_000, "", second)
	next(t, fall, first, Slot(fall, first), at("2026-04-11T15:15:00Z"))
	spring := fall
	spring.Time = "02:15"
	next(t, spring, at("2026-10-03T14:00:00Z"), "", at("2026-10-10T15:15:00Z"))
}

func TestCrossMidnightRollback(t *testing.T) {
	casey := with(func(i *Input) { i.Timezone, i.Days, i.Time = "Antarctica/Casey", []int{4}, "23:30" })
	first, second, now := at("2010-03-04T12:30:00Z"), at("2010-03-04T15:30:00Z"), at("2010-03-04T13:30:00Z")
	next(t, casey, now, "", second)
	if Slot(casey, first) != Slot(casey, second) {
		t.Fatal("rollback slots differ")
	}
	next(t, casey, now, Slot(casey, first), at("2010-03-11T15:30:00Z"))
}

func TestOneTimeRuns(t *testing.T) {
	once := Input{Kind: "once", Enabled: true, Date: "2026-11-01", Time: "09:30", Timezone: "America/Los_Angeles"}
	check := func(input Input, want int64) {
		t.Helper()
		got, ok := OneTimeRun(input)
		if want == 0 && ok || want != 0 && got != want {
			t.Errorf("OneTimeRun(%+v) = %d %v", input, got, ok)
		}
	}
	check(once, at("2026-11-01T17:30:00Z"))
	utc := once
	utc.Timezone = "UTC"
	check(utc, at("2026-11-01T09:30:00Z"))
	repeated := once
	repeated.Time = "01:30"
	check(repeated, at("2026-11-01T08:30:00Z"))
	gap := once
	gap.Date, gap.Time = "2026-03-08", "02:30"
	check(gap, 0)
	invalid := once
	invalid.Date = "2026-02-30"
	check(invalid, 0)

	runAt := at("2026-11-01T17:30:00Z")
	slot := OneTimeSlot(once)
	next(t, once, runAt-86400_000, "", runAt)
	next(t, once, runAt+60_000, "", runAt)
	next(t, once, runAt+OneTimeGraceMs, "", 0)
	next(t, once, runAt, slot, 0)
	disabled := once
	disabled.Enabled = false
	next(t, disabled, runAt, "", 0)
	for _, c := range []struct {
		now  int64
		want string
	}{{runAt - 1, ""}, {runAt, slot}, {runAt + OneTimeGraceMs - 1, slot}, {runAt + OneTimeGraceMs, ""}} {
		if got := Slot(once, c.now); got != c.want {
			t.Errorf("Slot at %d = %q", c.now, got)
		}
	}
}

func TestLocalDate(t *testing.T) {
	now := at("2026-11-01T03:00:00Z")
	if date, _ := LocalDate("UTC", now); date != "2026-11-01" {
		t.Fatal(date)
	}
	if date, _ := LocalDate("America/Los_Angeles", now); date != "2026-10-31" {
		t.Fatal(date)
	}
	if _, ok := LocalDate("Not/AZone", now); ok {
		t.Fatal("unknown zone accepted")
	}
}
