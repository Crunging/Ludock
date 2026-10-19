package views

import (
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/datasize"
	"github.com/Crunging/Ludock/internal/servers"
)

// Status is a state label and its tone.
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
		return "Paused in Docker. Resume it through Docker or its owning manager."
	case "restarting":
		return "Restart in progress. Controls will be available when it finishes."
	case "removing":
		return "Removal in progress."
	}
	return "Container state: " + state + ". Check it in Docker or its owning manager before using server controls."
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
	"stopping": "Waiting for server to stop", "verifying_stop": "Checking shutdown", "starting": "Starting server"}

// OperationLabel describes an operation's progress.
func OperationLabel(status, phase string) string {
	switch status {
	case "already_current":
		return "Configured image is current"
	case "interrupted":
		return "Interrupted — review required"
	case "failed":
		return "Failed"
	case "succeeded":
		return "Completed"
	}
	if label, ok := phaseLabels[phase]; ok {
		return label
	}
	return strings.ReplaceAll(phase, "_", " ")
}

// OperationStatusLabels names operation outcomes.
var OperationStatusLabels = map[string]string{"queued": "Queued", "running": "Running", "succeeded": "Succeeded",
	"already_current": "Already current", "failed": "Failed", "interrupted": "Interrupted"}

// OperationTones colors operation outcomes.
var OperationTones = map[string]string{"queued": "active", "running": "active", "succeeded": "ok", "already_current": "ok",
	"failed": "failed", "interrupted": "attention"}

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

// Since describes when a state began.
func Since(state string, since int64) (string, bool) {
	if since == 0 {
		return "", false
	}
	if state == "running" {
		return "started", true
	}
	return "", true
}

// FormatBytes formats a size like v0.5.0.
func FormatBytes(bytes int64) string { return datasize.Format(bytes) }
