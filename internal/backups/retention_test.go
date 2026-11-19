package backups

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	zone, err := time.LoadLocation(name)
	if err != nil {
		t.Skip("time zone data unavailable:", err)
	}
	return zone
}

// series makes complete backups at the given times, returned newest first.
func series(times ...time.Time) []Backup {
	var list []Backup
	for index, at := range times {
		list = append(list, Backup{ID: fmt.Sprintf("b%04d", index), CreatedAt: at.UnixMilli(), Size: 10, State: "complete"})
	}
	slices.SortFunc(list, func(a, b Backup) int { return int(b.CreatedAt - a.CreatedAt) })
	return list
}

func nothingProtected(string) bool { return false }

// planRemovals is what Prune removes when each removal frees its size of
// room: everything expired, then spare backups while room is short of need.
func planRemovals(list []Backup, zone *time.Location, protected func(string) bool, room, need int64) []Backup {
	expired, spare := planRetention(list, zone, protected)
	for _, backup := range expired {
		room += backup.Size
	}
	for _, backup := range spare {
		if room >= need {
			break
		}
		expired = append(expired, backup)
		room += backup.Size
	}
	return expired
}

func remaining(list, removed []Backup) []Backup {
	return slices.DeleteFunc(slices.Clone(list), func(backup Backup) bool {
		return slices.ContainsFunc(removed, func(r Backup) bool { return r.ID == backup.ID })
	})
}

// A year of nightly backups leaves a few weeks of dailies, then weeklies,
// then monthlies reaching back about six months.
func TestRetentionNightlyYear(t *testing.T) {
	zone := mustZone(t, "America/New_York")
	start := time.Date(2025, 11, 1, 3, 0, 0, 0, zone)
	var times []time.Time
	for day := 0; day < 365; day++ {
		times = append(times, start.AddDate(0, 0, day))
	}
	list := series(times...)
	left := remaining(list, planRemovals(list, zone, nothingProtected, 0, 0))
	if len(left) > keepNewest+keepDays+keepWeeks+keepMonths || len(left) < 12 {
		t.Fatalf("kept %d backups", len(left))
	}
	newest := time.UnixMilli(list[0].CreatedAt).In(zone)
	for day := 0; day < keepDays; day++ {
		if left[day].ID != list[day].ID {
			t.Fatalf("the backup from %d days ago is gone", day)
		}
	}
	oldest := time.UnixMilli(left[len(left)-1].CreatedAt).In(zone)
	if months := newest.Sub(oldest).Hours() / 24 / 30; months < 4.5 || months > 6.5 {
		t.Fatalf("history reaches back %.1f months", months)
	}
}

// Backups taken rarely are never removed for their age: days, weeks, and
// months without backups don't count.
func TestRetentionRareBackups(t *testing.T) {
	zone := time.UTC
	three := series(time.Date(2019, 1, 5, 12, 0, 0, 0, zone), time.Date(2021, 6, 1, 12, 0, 0, 0, zone), time.Date(2024, 2, 9, 12, 0, 0, 0, zone))
	if removed := planRemovals(three, zone, nothingProtected, 0, 0); len(removed) != 0 {
		t.Fatal("removed old backups", removed)
	}
	var monthly []time.Time
	for month := 0; month < 24; month++ {
		monthly = append(monthly, time.Date(2023, time.Month(1+month), 15, 9, 0, 0, 0, zone))
	}
	list := series(monthly...)
	left := remaining(list, planRemovals(list, zone, nothingProtected, 0, 0))
	// The seven most recent days with backups are seven months.
	if len(left) != keepDays || left[keepDays-1].ID != list[keepDays-1].ID {
		t.Fatalf("kept %d monthly backups", len(left))
	}
}

// Many backups in a day keep the newest three of that day, and earlier days
// keep their last backup.
func TestRetentionManyInADay(t *testing.T) {
	zone := time.UTC
	var times []time.Time
	for hour := 0; hour < 24; hour++ {
		times = append(times, time.Date(2026, 3, 10, hour, 0, 0, 0, zone), time.Date(2026, 3, 9, hour, 30, 0, 0, zone))
	}
	list := series(times...)
	left := remaining(list, planRemovals(list, zone, nothingProtected, 0, 0))
	want := []string{"2026-03-10T23:00", "2026-03-10T22:00", "2026-03-10T21:00", "2026-03-09T23:30"}
	var got []string
	for _, backup := range left {
		got = append(got, time.UnixMilli(backup.CreatedAt).In(zone).Format("2006-01-02T15:04"))
	}
	if !slices.Equal(got, want) {
		t.Fatal(got)
	}
}

// Days follow the calendar in Ludock's time zone, including the days clocks
// change: a day 23 or 25 hours long is still one day.
func TestRetentionDaylightSavingDays(t *testing.T) {
	zone := mustZone(t, "Europe/Berlin")
	// 2026-03-29 is 23 hours long and 2026-10-25 is 25 hours long in Berlin.
	for _, day := range []time.Time{time.Date(2026, 3, 29, 0, 0, 0, 0, zone), time.Date(2026, 10, 25, 0, 0, 0, 0, zone)} {
		late := time.Date(day.Year(), day.Month(), day.Day(), 23, 50, 0, 0, zone)
		var times []time.Time
		for at := day.Add(10 * time.Minute); at.Before(late) || at.Equal(late); at = at.Add(time.Hour) {
			times = append(times, at)
		}
		previous := day.Add(-5 * time.Minute)
		list := series(append(times, previous)...)
		left := remaining(list, planRemovals(list, zone, nothingProtected, 0, 0))
		if len(left) != keepNewest+1 || left[keepNewest].CreatedAt != previous.UnixMilli() {
			t.Fatalf("%s: kept %d backups", day.Format(time.DateOnly), len(left))
		}
		// The long day holds two backups at 02:10, an hour apart.
		if day.Month() == 10 && len(times) != 25 {
			t.Fatalf("the long day had %d hourly backups", len(times))
		}
	}
}

// Kept backups are never removed and don't take a place in the rule; a
// protected backup stays even when its clock reads older than the rest.
func TestRetentionKeptAndProtected(t *testing.T) {
	zone := time.UTC
	var times []time.Time
	for minute := 0; minute < 10; minute++ {
		times = append(times, time.Date(2026, 5, 1, 12, minute, 0, 0, zone))
	}
	list := series(times...)
	list[0].KeptAt, list[9].KeptAt = 1, 1
	list[5].State = "deleting"
	protected := func(id string) bool { return id == list[8].ID }
	left := remaining(list, planRemovals(list, zone, protected, 0, 0))
	var ids []string
	for _, backup := range left {
		ids = append(ids, backup.ID)
	}
	want := []string{list[0].ID, list[1].ID, list[2].ID, list[3].ID, list[5].ID, list[8].ID, list[9].ID}
	if !slices.Equal(ids, want) {
		t.Fatal(ids, want)
	}
}

// When the next backup wouldn't fit, the oldest backups the rule keeps go
// first, until it fits; the newest three and kept ones always stay.
func TestRetentionSpacePressure(t *testing.T) {
	zone := time.UTC
	var times []time.Time
	for day := 0; day < 10; day++ {
		times = append(times, time.Date(2026, 1, 1+day, 3, 0, 0, 0, zone))
	}
	list := series(times...)
	list[9].KeptAt = 1
	// The rule keeps January 4 to 10 (dailies and the week before), so ten
	// bytes short of room also removes January 4.
	byRule := planRemovals(list, zone, nothingProtected, 100, 0)
	pressed := planRemovals(list, zone, nothingProtected, 100-10*int64(len(byRule))-10, 100)
	if len(byRule) != 2 || len(pressed) != 3 || pressed[2].ID != list[6].ID {
		t.Fatal(byRule, pressed)
	}
	// With no room at all, everything but the newest three and the kept one goes.
	left := remaining(list, planRemovals(list, zone, nothingProtected, -1000, 100))
	if len(left) != keepNewest+1 || left[keepNewest].ID != list[9].ID {
		t.Fatal(left)
	}
}
