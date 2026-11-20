package views

import (
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/datasize"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/history"
	"github.com/Crunging/Ludock/internal/jobs"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
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
	case "restarting":
		return Status{"Restarting", "active"}
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
func LifecycleAction(state string) string {
	switch state {
	case "running":
		return "stop"
	case "created", "exited":
		return "start"
	}
	return ""
}

// StateGuidance explains states without a lifecycle control.
func StateGuidance(state string) string {
	if LifecycleAction(state) != "" {
		return ""
	}
	switch state {
	case "paused":
		return "Paused in Docker. Resume it with the tool that runs it, such as Compose or Portainer."
	case "restarting":
		return "Restart in progress. Controls will be available when it finishes."
	case "removing":
		return "Removal in progress."
	}
	return "Docker reports it as " + state + ". Check it with the tool that runs it, such as Compose or Portainer, before using the controls."
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
	"checking_players": "Checking who is online", "warning": "Giving players 1 minute", "warned": "Going ahead", "stopping": "Waiting for server to stop", "verifying_stop": "Checking shutdown", "starting": "Starting server",
	"sending_command": "Sending command", "pulling": "Downloading the new image", "creating": "Preparing the updated server", "replacing": "Swapping in the updated server"}

// OperationLabel describes an operation's progress.
func OperationLabel(status, phase string) string {
	switch status {
	case "already_current":
		return "Already up to date"
	case "skipped":
		return "Skipped"
	case "interrupted":
		return "Interrupted, needs a look"
	case "failed":
		return "Failed"
	case "succeeded":
		return "Completed"
	case "cancelled":
		return "Cancelled"
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
	"already_current": "Already current", "skipped": "Skipped", "failed": "Failed", "interrupted": "Interrupted",
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
func CommandReply(result map[string]any) string {
	text, _ := result["reply"].(string)
	return text
}

// SaveDetail explains the save step of a stop or restart.
func SaveDetail(result map[string]any) string {
	switch result["saveStatus"] {
	case "sent":
		return "Save command sent before stopping."
	case "skipped":
		switch result["saveReason"] {
		case "timed_out":
			return "Save command timed out; shutdown continued."
		case "console_unavailable":
			return "Save skipped: console unavailable. Shutdown continued."
		case "not_running":
			return "Save skipped: server was already stopped."
		case "unsupported":
			return "No save command is configured for this game."
		}
		return "Save skipped; shutdown continued."
	}
	return ""
}

// WarnDetail notes work asked to warn players that found nobody online.
func WarnDetail(result map[string]any) string {
	if result["warnSkipped"] == true {
		return "Nobody was online, so players weren’t warned."
	}
	return ""
}

// MovedDetail notes a restore of a backup taken before the server's data
// moved to other host storage.
func MovedDetail(result map[string]any) string {
	if result["dataMoved"] == true {
		return "The backup was taken before the server’s data moved."
	}
	return ""
}

// DockerWaitDetail notes work that waited for Docker to come back.
func DockerWaitDetail(result map[string]any) string {
	if result["waitedForDocker"] == true {
		return "Waited for Docker, which was briefly unreachable."
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
