package backups

import (
	"time"
)

// The retention rule, per server: the newest keepNewest backups, plus the
// newest backup of each of the most recent days, weeks, and months that have
// one. It depends only on when backups were taken, never on the current
// time, so nothing is removed for its age alone and a clock change can't
// expire a run of backups.
const (
	keepNewest  = 3
	keepDays    = 7
	keepWeeks   = 4
	keepMonths  = 6
	noteLimit   = 100
	spaceMargin = 10 // percent added to the newest backup's size for the next one
)

type period struct {
	count int
	key   func(time.Time) [2]int
}

var periods = []period{
	{keepDays, func(t time.Time) [2]int { return [2]int{t.Year(), t.YearDay()} }},
	{keepWeeks, func(t time.Time) [2]int { year, week := t.ISOWeek(); return [2]int{year, week} }},
	{keepMonths, func(t time.Time) [2]int { return [2]int{t.Year(), int(t.Month())} }},
}

// ruleKeeps returns the backups the rule keeps among list, which is newest
// first. Days, weeks, and months are calendar periods in zone.
func ruleKeeps(list []Backup, zone *time.Location) map[string]bool {
	keep := map[string]bool{}
	for _, backup := range list[:min(len(list), keepNewest)] {
		keep[backup.ID] = true
	}
	for _, p := range periods {
		seen := map[[2]int]bool{}
		for _, backup := range list {
			key := p.key(time.UnixMilli(backup.CreatedAt).In(zone))
			if seen[key] {
				continue
			}
			if len(seen) == p.count {
				break
			}
			seen[key] = true
			keep[backup.ID] = true
		}
	}
	return keep
}

// planRetention sorts one server's backups (newest first) for removal:
// expired are those the rule doesn't keep, and spare, oldest first, are the
// ones it keeps beyond the newest keepNewest, which go only while the next
// backup wouldn't fit. Kept backups, ones still being deleted, and protected
// ones are in neither; unreadable ones are in neither and take no place in
// the rule.
func planRetention(list []Backup, zone *time.Location, protected, readable func(id string) bool) (expired, spare []Backup) {
	var candidates []Backup
	for _, backup := range list {
		if backup.State == "complete" && backup.KeptAt == 0 && readable(backup.ID) {
			candidates = append(candidates, backup)
		}
	}
	keep := ruleKeeps(candidates, zone)
	for _, backup := range candidates {
		if !keep[backup.ID] && !protected(backup.ID) {
			expired = append(expired, backup)
		}
	}
	for index := len(candidates) - 1; index >= keepNewest; index-- {
		if backup := candidates[index]; keep[backup.ID] && !protected(backup.ID) {
			spare = append(spare, backup)
		}
	}
	return expired, spare
}
