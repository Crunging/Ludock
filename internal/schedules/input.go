// Package schedules runs owner-bound recurring and one-time server actions.
// A schedule never grants more than its owner currently has: every run and
// every edit rechecks the owner's role, grants, and the server binding.
package schedules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/schedtime"
)

type Announcements struct {
	LeadMinutes []int `json:"leadMinutes"`
}

// WaitForEmpty holds a restart, stop, or backup until no one is online, for up to
// Minutes after its scheduled time. Then is "skip" or "run": what happens
// when players are still online, or their count is unknown, at the end.
type WaitForEmpty struct {
	Minutes int    `json:"minutes"`
	Then    string `json:"then"`
}

const (
	WaitSkip = "skip"
	WaitRun  = "run"
	// MaxWaitMinutes keeps a wait, with its grace, well inside the shortest
	// gap between two recurring runs, a 23-hour day.
	MaxWaitMinutes     = 360
	DefaultWaitMinutes = 30
)

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
	Command       string         `json:"command,omitempty"`
	WaitForEmpty  *WaitForEmpty  `json:"waitForEmpty,omitempty"`
}

// Clock returns the time-math view.
func (i Input) Clock() schedtime.Input {
	return schedtime.Input{Kind: i.Kind, Enabled: i.Enabled, Time: i.Time, Timezone: i.Timezone, Days: i.Days, Date: i.Date}
}

var (
	timePattern = regexp.MustCompile(`^(?:[01]\d|2[0-3]):[0-5]\d$`)
	datePattern = regexp.MustCompile(`^\d{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01])$`)
)

var Actions = []string{"start", "stop", "restart", "backup", "command"}

// MaxCommandLength is the longest scheduled console command, in characters.
const MaxCommandLength = 500

// DefaultLeadMinutes are the countdown warnings offered by default.
var DefaultLeadMinutes = []int{5, 1}

// Disconnects reports whether an action stops a running server, which is
// what warnings and waiting for an empty server are for.
func Disconnects(action string) bool {
	return action == "restart" || action == "stop" || action == "backup"
}

// Considerate gives a new schedule the polite choices its game allows:
// waiting up to DefaultWaitMinutes for the server to empty, then going
// ahead, and warning anyone still online before it does.
func (i *Input) Considerate(canWarn, canWait bool) {
	i.WaitForEmpty, i.Announcements = nil, nil
	if !Disconnects(i.Action) {
		return
	}
	if canWait {
		i.WaitForEmpty = &WaitForEmpty{Minutes: DefaultWaitMinutes, Then: WaitRun}
	}
	if canWarn {
		i.Announcements = &Announcements{LeadMinutes: slices.Clone(DefaultLeadMinutes)}
	}
}

// WarningMessage is what players see minutes before an action, in the
// words of the restart warning.
func WarningMessage(action string, minutes int) string {
	what := map[string]string{"stop": "Server shutting down", "backup": "Server restart for a backup",
		"update": "Server restart for an update", "restore": "Server restart to restore a backup"}[action]
	if what == "" {
		what = "Server restart"
	}
	unit := "minutes"
	if minutes == 1 {
		unit = "minute"
	}
	return fmt.Sprintf("%s in %d %s.", what, minutes, unit)
}

// CalledOffMessage tells players a warned action won't happen.
func CalledOffMessage(action string) string {
	what := map[string]string{"stop": "Server shutdown", "backup": "Server restart for a backup",
		"update": "Server restart for an update", "restore": "Server restart to restore a backup"}[action]
	if what == "" {
		what = "Server restart"
	}
	return what + " cancelled."
}

func invalid(message string) error { return apperr.New("INVALID_REQUEST", 400, message) }

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
	if err := i.validateCommand(); err != nil {
		return err
	}
	if i.CreateBackup && i.Action != "restart" {
		return invalid("Backup before restart is only available for restart schedules")
	}
	if wait := i.WaitForEmpty; wait != nil {
		if !Disconnects(i.Action) {
			return invalid("Waiting for an empty server is only available for restarts, stops, and backups")
		}
		if wait.Minutes < 1 || wait.Minutes > MaxWaitMinutes {
			return apperr.Newf("INVALID_REQUEST", 400, "Wait between 1 and %d minutes for players to leave", MaxWaitMinutes)
		}
		if wait.Then != WaitSkip && wait.Then != WaitRun {
			return invalid("Choose what happens if players are still online")
		}
		// A warning announces a restart, so it needs one that happens.
		if i.Announcements != nil && wait.Then != WaitRun {
			return invalid("Warnings need the run to go ahead when the wait ends, so they don’t announce something that doesn’t happen")
		}
	}
	if i.Announcements != nil {
		if !Disconnects(i.Action) {
			return invalid("Warnings are only available for restarts, stops, and backups")
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

// validateCommand trims the command and keeps it to one line without control
// characters, so a schedule sends exactly one command.
func (i *Input) validateCommand() error {
	if i.Action != "command" {
		if i.Command != "" {
			return invalid("Only command schedules send a console command")
		}
		return nil
	}
	i.Command = strings.TrimSpace(i.Command)
	if i.Command == "" {
		return invalid("Enter a console command")
	}
	if !utf8.ValidString(i.Command) || utf8.RuneCountInString(i.Command) > MaxCommandLength {
		return apperr.Newf("INVALID_REQUEST", 400, "Console commands can be up to %d characters", MaxCommandLength)
	}
	if strings.ContainsFunc(i.Command, func(r rune) bool {
		return unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp)
	}) {
		return invalid("Console commands must be one line without control characters")
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
	switch i.Action {
	case "backup":
		capability = authz.BackupsCreate
	case "command":
		capability = authz.ConsoleExecute
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
	encoded, _ := json.Marshal([]any{i.Kind, i.Action, i.Command, i.CreateBackup, i.Announcements, i.WaitForEmpty, i.Enabled, i.Time, i.Timezone, days, i.Date})
	return string(encoded)
}
