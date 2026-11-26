package views

import (
	"encoding/json"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/datasize"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/history"
	"github.com/Crunging/Ludock/internal/jobs"
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/selfupdate"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/update"
)

type Status struct{ Label, Tone string }

var cleanExits = map[int]bool{0: true, 130: true, 143: true}

// ServerStatus names Docker's state the way players would.
func ServerStatus(state, health string, exit *servers.Exit) Status {
	switch state {
	case "running":
		switch health {
		case "starting":
			return Status{"Starting", "active"}
		case "unhealthy":
			return Status{"Unhealthy", "attention"}
		}
		return Status{"Running", "ok"}
	case "exited":
		if exit != nil && exit.OOMKilled {
			return Status{"Out of memory", "failed"}
		}
		// Docker kills a server that doesn't stop within its grace period.
		if exit != nil && exit.Code == 137 {
			return Status{"Force-stopped", "attention"}
		}
		if exit != nil && !cleanExits[exit.Code] {
			return Status{"Crashed", "failed"}
		}
		return Status{"Stopped", "idle"}
	case "created":
		return Status{"Not started", "idle"}
	// Docker restarts a game that keeps exiting under its restart policy.
	case "restarting":
		return Status{"Keeps restarting", "failed"}
	case "paused":
		return Status{"Paused", "active"}
	case "removing":
		return Status{"Removing", "active"}
	case "dead":
		return Status{"Dead", "failed"}
	}
	return Status{Capitalize(state), "idle"}
}

// ExitDetail explains an unclean exit.
func ExitDetail(exit *servers.Exit) string {
	if exit == nil {
		return ""
	}
	if exit.OOMKilled {
		return "Ran out of memory"
	}
	if cleanExits[exit.Code] {
		return ""
	}
	return "Exit code " + strconv.Itoa(exit.Code)
}

// LifecycleAction is the main control for a state: start, stop, or none.
// Docker treats a restarting container as started, and a stop ends its
// restarts.
func LifecycleAction(state string) string {
	switch state {
	case "running", "restarting":
		return "stop"
	case "created", "exited":
		return "start"
	}
	return ""
}

// StateGuidance explains a broken server, or a state without a lifecycle
// control; one that keeps restarting gets what to do only for a viewer who
// can stop it.
func StateGuidance(state string, canStop bool) string {
	if state == "restarting" && canStop {
		return "The game keeps stopping and Docker keeps starting it again. Stop it, check its logs, then fix the cause or restore a backup."
	}
	// Someone who can't stop it is told what is happening, not what to do.
	if state == "restarting" {
		return "The game keeps stopping and Docker keeps starting it again."
	}
	if LifecycleAction(state) != "" {
		return ""
	}
	switch state {
	case "paused":
		return "Paused in Docker. Resume it with the tool that runs it, such as Compose or Portainer."
	case "removing":
		return "Removal in progress."
	}
	return "Docker reports it as " + state + ". Check it with the tool that runs it, such as Compose or Portainer, before using the controls."
}

// SafetyNote says what a safety backup was taken before, or is empty.
func SafetyNote(reason string) string {
	switch reason {
	case "restore":
		return "Taken before a restore"
	case "update":
		return "Taken before an update"
	}
	return ""
}

// Capitalize upper-cases the first letter and spaces underscores.
func Capitalize(value string) string {
	if value == "" {
		return value
	}
	value = strings.ReplaceAll(value, "_", " ")
	return strings.ToUpper(value[:1]) + value[1:]
}

var phaseLabels = map[string]string{"saving_world": "Saving world", "waiting_for_save": "Waiting for save", "save_skipped": "Save skipped",
	"checking_players": "Checking who is online", "warning_players": "Warning players", "warning": "Giving players 1 minute", "warned": "Going ahead", "stopping": "Waiting for server to stop", "verifying_stop": "Checking shutdown", "starting": "Starting server",
	"sending_command": "Sending command", "pulling": "Downloading the new image", "creating": "Preparing the updated server", "replacing": "Swapping in the updated server",
	"restore_staging": "Unpacking the backup", "restore_moving_old": "Moving the current data aside", "restore_old_moved": "Putting the backup in place",
	"restore_replacing": "Putting the backup in place", "restore_replaced": "Putting the backup in place", "restore_committed": "Backup in place",
	"restore_cleaned": "Cleaning up", "restore_rolling_back": "Putting the previous data back",
	"restore_rolled_back": "Previous data back in place"}

// testDelivered says where a delivered test can be seen.
func testDelivered(destination notify.Destination) string {
	switch destination {
	case notify.Email:
		return "Delivered. The test message should be in your inbox now."
	case notify.Webhook:
		return "Delivered. The webhook accepted the test message."
	}
	return "Delivered. The test message should be in " + destination.Label() + " now."
}

// OperationLabel describes an operation's progress, or its result once it
// has finished.
func OperationLabel(status, phase string) string {
	if label, ok := OperationStatusLabels[status]; ok && status != "queued" && status != "running" {
		return label
	}
	if label, ok := phaseLabels[phase]; ok {
		return label
	}
	return Capitalize(phase)
}

// OperationPath is where a server's page follows an operation: the Backups
// tab for backups and restores, the Activity tab for anything else.
func OperationPath(serverID, kind, operationID string) string {
	tab := "activity"
	if kind == "backup" || kind == "restore" {
		tab = "backups"
	}
	return "/servers/" + serverID + "?tab=" + tab + "&operation=" + operationID
}

var OperationStatusLabels = map[string]string{"queued": "Queued", "running": "Running", "succeeded": "Succeeded",
	"already_current": "Already up to date", "skipped": "Skipped", "failed": "Failed", "interrupted": "Interrupted",
	"cancelled": "Cancelled"}

var OperationTones = map[string]string{"queued": "active", "running": "active", "succeeded": "ok", "already_current": "ok",
	"skipped": "idle", "failed": "failed", "interrupted": "attention", "cancelled": "idle"}

// CancelledNote says who cancelled work, which never acted.
func CancelledNote(by *history.Actor) string {
	name := "someone"
	if by != nil && by.Name != "" {
		name = by.Name
	} else if by != nil && by.ID != "" {
		name = "a deleted user"
	}
	return "Cancelled by " + name + " before it changed anything."
}

// CanCancelActive reports whether the viewer of server may cancel the work
// it is waiting on.
func CanCancelActive(server servers.View) bool {
	operation := server.ActiveOperation
	if operation == nil {
		return false
	}
	job := &ops.Job{Kind: operation.Kind, Status: operation.Status, Phase: operation.Phase, Input: operation.Input}
	capabilities := jobs.StartCapabilities(job)
	if !ops.Waiting(job) || len(capabilities) == 0 {
		return false
	}
	for _, capability := range capabilities {
		if !server.Can(capability) {
			return false
		}
	}
	return true
}

// CommandReply is the redacted, bounded reply a scheduled command kept. It is
// console output, for viewers with console access only.
func CommandReply(result ops.Result) string { return result.Reply }

// SaveDetail explains the save step of a stop or restart.
func SaveDetail(result ops.Result) string {
	switch result.SaveStatus {
	case "sent":
		return "Save command sent before stopping."
	case "skipped":
		switch result.SaveReason {
		case "timed_out":
			return "Save command timed out; shutdown continued."
		case "console_unavailable":
			return "Save skipped: console unavailable. Shutdown continued."
		case "not_running":
			return "Save skipped: server was already stopped."
		case "restarting":
			return "Save skipped: the game kept stopping and starting again, so it couldn’t be asked to save."
		case "saves_on_stop":
			return "The game saves its world when it stops."
		case "unsupported":
			return "This game doesn’t have a save command."
		}
		return "Save skipped; shutdown continued."
	}
	return ""
}

// WarnDetail notes work asked to warn players that found nobody online.
func WarnDetail(result ops.Result) string {
	if result.WarnSkipped {
		return "Nobody was online, so players weren’t warned."
	}
	if result.WarnFailedNote != "" {
		return result.WarnFailedNote
	}
	return ""
}

// MovedDetail notes a restore of a backup taken before the server's data
// moved to other host storage.
func MovedDetail(result ops.Result) string {
	if result.DataMoved {
		return "The backup was taken before the server’s data moved."
	}
	return ""
}

// LeftOutDetail notes a backup that left out a folder holding a restore's
// stage, and with it the previous data that restore kept.
func LeftOutDetail(result ops.Result) string {
	if result.LeftOut != "" {
		return "The backup leaves out the hidden .ludock-restore-… folder in " + result.LeftOut + ", so the previous data an unfinished restore keeps there isn’t in it."
	}
	return ""
}

// ACLsLostDetail notes a restore whose storage couldn't hold some files'
// ACLs.
func ACLsLostDetail(result ops.Result) string {
	if result.ACLsLost {
		return "This storage can’t hold the access control lists (ACLs) some files had, which set permissions for specific users or groups, so those files were restored without them, with permissions that may be stricter."
	}
	return ""
}

// DockerWaitDetail notes work that waited for Docker to come back.
func DockerWaitDetail(result ops.Result) string {
	if result.WaitedForDocker {
		return "Waited for Docker to answer again."
	}
	return ""
}

// ConnectAddress is the address players type. An unset host uses the host
// the browser used to reach Ludock.
func ConnectAddress(connection *servers.Connection, pageHost string) string {
	if connection == nil {
		return ""
	}
	host := connection.Host
	if host == "" {
		host = pageHost
		if split, _, err := net.SplitHostPort(pageHost); err == nil {
			host = split
		}
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	if connection.Port == 0 {
		return host
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host + ":" + strconv.Itoa(connection.Port)
}

// ISO formats milliseconds for <time datetime>.
func ISO(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.RFC3339) }

// UTCText is the server-side fallback text for a timestamp; the browser
// replaces it with local formatting.
func UTCText(ms int64) string { return time.UnixMilli(ms).UTC().Format("Jan 2, 2006, 15:04 UTC") }

func FormatBytes(bytes int64) string { return datasize.Format(bytes) }

// GameChoices are the games an administrator can pick for a server, by
// name, then "Other game".
func GameChoices() []GameChoice {
	choices := make([]GameChoice, 0, len(games.Integrations)+1)
	for _, game := range games.Integrations {
		choices = append(choices, GameChoice{game.GameType, game.Name})
	}
	slices.SortFunc(choices, func(a, b GameChoice) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return append(choices, GameChoice{"unknown", games.DisplayName("unknown")})
}

// portList is the published ports players would use, such as
// "2456/udp, 2457/udp".
func portList(ports []servers.Port) string {
	parts := make([]string, len(ports))
	for index, port := range ports {
		parts[index] = strconv.Itoa(port.Public) + "/" + port.Type
	}
	return strings.Join(parts, ", ")
}

// browsers and systems are checked in order: Edge and Opera also name
// Chrome, Chrome names Safari, and Android names Linux.
var browsers = [][2]string{{"Edg", "Edge"}, {"OPR/", "Opera"}, {"SamsungBrowser/", "Samsung Internet"},
	{"Firefox/", "Firefox"}, {"FxiOS/", "Firefox"}, {"CriOS/", "Chrome"}, {"Chrome/", "Chrome"}, {"Safari/", "Safari"}}
var systems = [][2]string{{"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Android", "Android"}, {"CrOS", "ChromeOS"},
	{"Windows", "Windows"}, {"Macintosh", "macOS"}, {"Linux", "Linux"}}

// BrowserName names a session's browser from its user agent, such as
// "Firefox on Windows".
func BrowserName(userAgent string) string {
	browser, system := "", ""
	for _, candidate := range browsers {
		if strings.Contains(userAgent, candidate[0]) {
			browser = candidate[1]
			break
		}
	}
	for _, candidate := range systems {
		if strings.Contains(userAgent, candidate[0]) {
			system = candidate[1]
			break
		}
	}
	switch {
	case browser != "" && system != "":
		return browser + " on " + system
	case browser != "":
		return browser
	case system != "":
		return "A browser on " + system
	}
	return "Unknown browser"
}

// updatedSince reports an update of Ludock that succeeded since the last
// check of its image.
func updatedSince(last *selfupdate.Record, check *update.CheckStatus) bool {
	return last != nil && (last.State == selfupdate.StateSucceeded || last.State == selfupdate.StateCurrent) &&
		(check == nil || check.CheckedAt < last.FinishedAt)
}

func ludockUpdated(record selfupdate.Record) string {
	return "Ludock was updated to version " + record.To + "."
}

// ludockFailed says how a failed update ended, in plain words.
func ludockFailed(record selfupdate.Record) string {
	reason := strings.TrimSuffix(record.Reason, ".")
	put := "Ludock’s update didn’t finish: " + reason + ". Ludock put version " + record.From + " back"
	policy := ""
	if record.NoPolicy {
		policy = " Docker didn’t give it back its restart policy, so it won’t start by itself after a restart; run docker compose up -d ludock in the folder with its compose.yaml to set it again."
	}
	switch {
	case record.State == selfupdate.StatePutBack && record.Data == selfupdate.DataKept:
		return put + ", but found no copy of its data to put back with it, so it kept the newer data." + policy
	case record.State == selfupdate.StatePutBack && record.Data == selfupdate.DataNotRestored:
		return put + ", but couldn’t put the copy of its data from before the update back, so it runs on the newer data. The copy, " +
			selfupdate.CopyName + ", is beside its database." + policy
	case record.State == selfupdate.StatePutBack:
		return put + ", with its data as it was." + policy
	case record.State == selfupdate.StateInterrupted:
		return "Ludock’s update didn’t finish: it stopped partway, so Ludock may be on either version. Finish it from a terminal with the commands below." + policy
	}
	return "Ludock couldn’t update itself: " + reason + ". It is still on version " + record.From + "." + policy
}

// offerReason is why Update Ludock isn't offered, unless a refused click
// already shows it as an error.
func offerReason(offer selfupdate.Offer) string {
	if offer.Refused {
		return ""
	}
	return offer.Reason
}

func dismissValues(id string) string {
	encoded, _ := json.Marshal(map[string]string{"id": id})
	return string(encoded)
}
