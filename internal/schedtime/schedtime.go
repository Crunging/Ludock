// Package schedtime computes schedule slots and next runs in a schedule's
// own time zone. Missed minutes and spring-forward gaps are skipped, never
// caught up, and repeated fall-back clock times share one slot so
// destructive work runs once.
package schedtime

import (
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"

	// Ludock's image has no system zoneinfo; embed it.
	_ "time/tzdata"
)

const (
	minute = int64(60_000)
	hour   = 60 * minute
	day    = 24 * hour
	// OneTimeGraceMs is how long after its time a one-time schedule may still
	// start, covering a delayed tick or brief restart. Later runs are
	// recorded as missed and never replayed.
	OneTimeGraceMs = 5 * minute
)

var (
	timePattern = regexp.MustCompile(`^(?:[01]\d|2[0-3]):[0-5]\d$`)
	datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// Input is the clock part of a schedule.
type Input struct {
	Kind     string // "recurring" or "once"
	Enabled  bool
	Time     string // HH:MM
	Timezone string
	Days     []int  // recurring: 0 = Sunday
	Date     string // once: YYYY-MM-DD
}

var (
	zonesMu sync.Mutex
	zones   = map[string]*time.Location{}
)

// Location loads a time zone, caching results.
func Location(name string) (*time.Location, bool) {
	zonesMu.Lock()
	defer zonesMu.Unlock()
	if location, ok := zones[name]; ok {
		return location, location != nil
	}
	location, err := time.LoadLocation(name)
	if err != nil || name == "" || name == "Local" {
		location = nil
	}
	if len(zones) > 256 {
		zones = map[string]*time.Location{}
	}
	zones[name] = location
	return location, location != nil
}

type clock struct {
	date        string
	dateEpoch   int64
	time        string
	minuteStart int64
	offset      int64
}

func floorDiv(value, divisor int64) int64 {
	quotient := value / divisor
	if value%divisor != 0 && (value < 0) != (divisor < 0) {
		quotient--
	}
	return quotient
}

func localClock(location *time.Location, now int64) clock {
	t := time.UnixMilli(now).In(location)
	_, offsetSeconds := t.Zone()
	seconds := floorDiv(now, 1000) * 1000
	return clock{
		date:        t.Format("2006-01-02"),
		dateEpoch:   time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).UnixMilli(),
		time:        t.Format("15:04"),
		minuteStart: seconds - int64(t.Second())*1000,
		offset:      int64(offsetSeconds) * 1000,
	}
}

func validClock(input Input, now int64) (*time.Location, bool) {
	if !input.Enabled || now < 0 || !timePattern.MatchString(input.Time) || len(input.Timezone) == 0 || len(input.Timezone) > 100 {
		return nil, false
	}
	switch input.Kind {
	case "once":
		if !datePattern.MatchString(input.Date) {
			return nil, false
		}
	case "recurring":
		if len(input.Days) == 0 || len(input.Days) > 7 {
			return nil, false
		}
		for _, d := range input.Days {
			if d < 0 || d > 6 {
				return nil, false
			}
		}
	default:
		return nil, false
	}
	return Location(input.Timezone)
}

func weekday(dateEpoch int64) int { return int(time.UnixMilli(dateEpoch).UTC().Weekday()) }

func clockSlot(input Input, c clock) string {
	if !slices.Contains(input.Days, weekday(c.dateEpoch)) || c.time != input.Time {
		return ""
	}
	return c.date + "T" + input.Time + ":" + input.Timezone
}

// OneTimeSlot is the single execution slot of a one-time schedule.
func OneTimeSlot(input Input) string { return input.Date + "T" + input.Time + ":" + input.Timezone }

// Slot is the matching local minute without an offset, so fall-back
// repetitions share it. A one-time schedule matches from its time until the
// grace period ends. "" means no slot.
func Slot(input Input, now int64) string {
	location, ok := validClock(input, now)
	if !ok {
		return ""
	}
	if input.Kind == "once" {
		runAt, ok := pendingOneTime(input, now)
		if ok && now >= runAt {
			return OneTimeSlot(input)
		}
		return ""
	}
	return clockSlot(input, localClock(location, now))
}

// dayOffsets samples the surrounding days for the offsets before and after a
// clock change.
func dayOffsets(location *time.Location, dateEpoch int64) []int64 {
	var offsets []int64
	for h := int64(-24); h <= 48; h += 12 {
		offset := localClock(location, dateEpoch+h*hour).offset
		if !slices.Contains(offsets, offset) {
			offsets = append(offsets, offset)
		}
	}
	return offsets
}

func parseClock(value string) (int64, int64) {
	return int64(value[0]-'0')*10 + int64(value[1]-'0'), int64(value[3]-'0')*10 + int64(value[4]-'0')
}

// OneTimeRun is the first instant the schedule's local date and time
// occurs; ok is false when that wall time does not exist (a spring gap) or
// the date is invalid.
func OneTimeRun(input Input) (int64, bool) {
	location, ok := Location(input.Timezone)
	if !ok || !timePattern.MatchString(input.Time) {
		return 0, false
	}
	date, err := time.Parse("2006-01-02", input.Date)
	if err != nil || date.Format("2006-01-02") != input.Date {
		return 0, false
	}
	dateEpoch := date.UnixMilli()
	h, m := parseClock(input.Time)
	wall := dateEpoch + h*hour + m*minute
	var candidates []int64
	for _, offset := range dayOffsets(location, dateEpoch) {
		if candidate := wall - offset; candidate >= 0 {
			candidates = append(candidates, candidate)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i] < candidates[j] })
	for _, candidate := range candidates {
		c := localClock(location, candidate)
		if c.dateEpoch == dateEpoch && c.time == input.Time {
			return candidate, true
		}
	}
	return 0, false
}

func pendingOneTime(input Input, now int64) (int64, bool) {
	runAt, ok := OneTimeRun(input)
	if ok && now < runAt+OneTimeGraceMs {
		return runAt, true
	}
	return 0, false
}

// NextRun is the next eligible local minute, including the current minute
// when it is unconsumed. A one-time schedule stays due through its grace
// period until its slot is consumed.
func NextRun(input Input, now int64, lastSlot string) (int64, bool) {
	location, ok := validClock(input, now)
	if !ok {
		return 0, false
	}
	if input.Kind == "once" {
		if OneTimeSlot(input) == lastSlot {
			return 0, false
		}
		return pendingOneTime(input, now)
	}
	current := localClock(location, now)
	h, m := parseClock(input.Time)
	// A rollback can return to yesterday, and a weekly slot can disappear in
	// a spring gap; cover yesterday through the week after a skipped one.
	for offsetDay := int64(-1); offsetDay <= 14; offsetDay++ {
		dateEpoch := current.dateEpoch + offsetDay*day
		if !slices.Contains(input.Days, weekday(dateEpoch)) {
			continue
		}
		wall := dateEpoch + h*hour + m*minute
		var candidates []int64
		for _, offset := range dayOffsets(location, dateEpoch) {
			if candidate := wall - offset; candidate >= current.minuteStart && candidate >= 0 {
				candidates = append(candidates, candidate)
			}
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i] < candidates[j] })
		for _, candidate := range candidates {
			c := localClock(location, candidate)
			if slot := clockSlot(input, c); c.dateEpoch == dateEpoch && slot != "" && slot != lastSlot {
				return candidate, true
			}
		}
	}
	return 0, false
}

// LocalDate is today's date in a time zone, as YYYY-MM-DD.
func LocalDate(timezone string, now int64) (string, bool) {
	location, ok := Location(timezone)
	if !ok {
		return "", false
	}
	return localClock(location, now).date, true
}
