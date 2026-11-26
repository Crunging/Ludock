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
		"update": "Server restart for an update", "restore": "Server restart to restore a backup",
		"undo_restore": "Server restart to undo a restore"}[action]
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
		"update": "Server restart for an update", "restore": "Server restart to restore a backup",
		"undo_restore": "Server restart to undo a restore"}[action]
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
		return invalid("Unknown time zone")
	}
	if _, ok := schedtime.Location(i.Timezone); !ok {
		return invalid("Unknown time zone")
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
		return invalid("Choose whether this repeats or runs once")
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
				return invalid("Warning times must be unique whole numbers between 1 and 60 minutes")
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

// Key compares settings, treating days as a set: schedules with equal keys
// do exactly the same thing.
func (i Input) Key() string {
	days := slices.Clone(i.Days)
	slices.Sort(days)
	days = slices.Compact(days)
	encoded, _ := json.Marshal([]any{i.Kind, i.Action, i.Command, i.CreateBackup, i.Announcements, i.WaitForEmpty, i.Enabled, i.Time, i.Timezone, days, i.Date})
	return string(encoded)
}

// SameTime reports whether two enabled schedules run at the same instant,
// whatever time zones they name: a one-time schedule's run against the
// other's, or two recurring ones on any of their runs in the coming year.
func SameTime(a, b Input, now int64) bool { return (&SameTimes{Now: now}).Same(a, b) }

// SameTimes is SameTime for many schedules, working out each recurring
// schedule's runs in the coming year only once.
type SameTimes struct {
	Now  int64
	runs map[string][]int64
}

func (s *SameTimes) Same(a, b Input) bool {
	if !a.Enabled || !b.Enabled {
		return false
	}
	runsAt := func(input Input, at int64) bool {
		next, ok := schedtime.NextRun(input.Clock(), at, "")
		return ok && next == at
	}
	for _, pair := range [][2]Input{{a, b}, {b, a}} {
		if pair[0].Kind == "once" {
			at, ok := schedtime.NextRun(pair[0].Clock(), s.Now, "")
			return ok && runsAt(pair[1], at)
		}
	}
	// In one time zone, runs only happen at their own wall-clock minute.
	if a.Timezone == b.Timezone {
		return a.Time == b.Time && slices.ContainsFunc(a.Days, func(day int) bool { return slices.Contains(b.Days, day) })
	}
	first, second := s.year(a), s.year(b)
	for i, j := 0, 0; i < len(first) && j < len(second); {
		switch {
		case first[i] == second[j]:
			return true
		case first[i] < second[j]:
			i++
		default:
			j++
		}
	}
	return false
}

// year lists a recurring schedule's runs in the coming year, which covers
// every weekday two schedules share and every stretch in which their time
// zones line up. A repeated fall-back minute runs once, as the scheduler
// runs it.
func (s *SameTimes) year(input Input) []int64 {
	key := input.Key()
	if runs, ok := s.runs[key]; ok {
		return runs
	}
	var runs []int64
	clock, last := input.Clock(), ""
	for at := s.Now; ; {
		next, ok := schedtime.NextRun(clock, at, last)
		if !ok || next > s.Now+366*24*time.Hour.Milliseconds() {
			break
		}
		runs = append(runs, next)
		last, at = schedtime.Slot(clock, next), next+60_000
	}
	if s.runs == nil {
		s.runs = map[string][]int64{}
	}
	s.runs[key] = runs
	return runs
}
