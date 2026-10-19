// Package schedules runs owner-bound recurring and one-time server actions.
// A schedule never grants more than its owner currently has: every run and
// every edit rechecks the owner's role, grants, and the server binding.
package schedules

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/schedtime"
)

// Announcements are restart countdown warnings.
type Announcements struct {
	LeadMinutes []int `json:"leadMinutes"`
}

// Input is a schedule's settings, as stored in JSON.
type Input struct {
	Kind          string         `json:"kind"`
	Action        string         `json:"action"`
	Announcements *Announcements `json:"announcements,omitempty"`
	CreateBackup  bool           `json:"createBackup"`
	Enabled       bool           `json:"enabled"`
	Time          string         `json:"time"`
	Timezone      string         `json:"timezone"`
	Days          []int          `json:"days,omitempty"`
	Date          string         `json:"date,omitempty"`
}

// Clock returns the time-math view.
func (i Input) Clock() schedtime.Input {
	return schedtime.Input{Kind: i.Kind, Enabled: i.Enabled, Time: i.Time, Timezone: i.Timezone, Days: i.Days, Date: i.Date}
}

var (
	timePattern = regexp.MustCompile(`^(?:[01]\d|2[0-3]):[0-5]\d$`)
	datePattern = regexp.MustCompile(`^\d{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01])$`)
)

// Actions a schedule can run.
var Actions = []string{"start", "stop", "restart", "backup"}

// DefaultLeadMinutes are the countdown warnings offered by default.
var DefaultLeadMinutes = []int{10, 5, 1}

func invalid(message string) error { return apperr.New("INVALID_REQUEST", 400, message) }

// Validate checks settings.
func (i *Input) Validate() error {
	if !slices.Contains(Actions, i.Action) {
		return invalid("Choose a schedule action")
	}
	if !timePattern.MatchString(i.Time) {
		return invalid("Enter a time as HH:MM")
	}
	if len(i.Timezone) == 0 || len(i.Timezone) > 100 {
		return invalid("Unknown timezone")
	}
	if _, ok := schedtime.Location(i.Timezone); !ok {
		return invalid("Unknown timezone")
	}
	switch i.Kind {
	case "recurring":
		if i.Date != "" || len(i.Days) == 0 || len(i.Days) > 7 {
			return invalid("Choose at least one day")
		}
		for _, d := range i.Days {
			if d < 0 || d > 6 {
				return invalid("Choose at least one day")
			}
		}
	case "once":
		if i.Days != nil {
			return invalid("One-time schedules have a date, not days")
		}
		parsed, err := time.Parse("2006-01-02", i.Date)
		if !datePattern.MatchString(i.Date) || err != nil || parsed.Format("2006-01-02") != i.Date {
			return invalid("Unknown date")
		}
	default:
		return invalid("Choose a schedule kind")
	}
	if i.CreateBackup && i.Action != "restart" {
		return invalid("Backup before restart is only available for restart schedules")
	}
	if i.Announcements != nil {
		if i.Action != "restart" {
			return invalid("Announcements are only available for restarts")
		}
		if i.Announcements.LeadMinutes == nil {
			i.Announcements.LeadMinutes = slices.Clone(DefaultLeadMinutes)
		}
		leads := i.Announcements.LeadMinutes
		if len(leads) < 1 || len(leads) > 10 {
			return invalid("Choose between 1 and 10 warning times")
		}
		seen := map[int]bool{}
		for _, lead := range leads {
			if lead < 1 || lead > 60 || seen[lead] {
				return invalid("Lead times must be unique minutes between 1 and 60")
			}
			seen[lead] = true
		}
	}
	return nil
}

// ParseSaved decodes stored settings strictly, applying defaults.
func ParseSaved(raw string) (Input, error) {
	input := Input{Enabled: true}
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return Input{}, errors.New("invalid saved schedule")
	}
	if err := input.Validate(); err != nil {
		return Input{}, err
	}
	return input, nil
}

// Capabilities are what the owner needs for a run.
func (i Input) Capabilities() []authz.Capability {
	capability := authz.Capability("server." + i.Action)
	if i.Action == "backup" {
		capability = authz.BackupsCreate
	}
	result := []authz.Capability{capability}
	if i.CreateBackup {
		result = append(result, authz.BackupsCreate)
	}
	return result
}

// EditCapabilities also require console access for warnings, which are
// skipped rather than suspending the schedule if the owner loses it.
func (i Input) EditCapabilities() []authz.Capability {
	result := i.Capabilities()
	if i.Announcements != nil {
		result = append(result, authz.ConsoleExecute)
	}
	return result
}

// key compares settings, treating days as a set.
func (i Input) key() string {
	days := slices.Clone(i.Days)
	slices.Sort(days)
	days = slices.Compact(days)
	encoded, _ := json.Marshal([]any{i.Kind, i.Action, i.CreateBackup, i.Announcements, i.Enabled, i.Time, i.Timezone, days, i.Date})
	return string(encoded)
}
