package games

import "strings"

// Features is what Ludock does for a game beyond start, stop, files, and
// backups. Settings → System and the reference's supported games list both
// come from it.
type Features struct {
	// Console is the console's name, or "" without one; ServerInput means
	// commands go to the server's input rather than a port.
	Console     string
	ConsolePort int
	ServerInput bool
	// AnnouncePort is where warnings go when they bypass the console.
	AnnouncePort int
	// StatusCheck is the status check's name, or "" without one;
	// ImageHealthCheck means the image's own health check stands in.
	StatusCheck      string
	StatusPort       int
	StatusPortEnv    []string
	ImageHealthCheck bool
	Players          string // "Names", "Character names", "Count", or ""
	Moderation       string // such as "Kick, ban, allowlist", or ""
	Warnings         bool
	RestartUpdates   bool
}

// Features describes the game without labels.
func (i *Integration) Features() Features {
	f := Features{Warnings: BroadcastTemplate(i.GameType, nil) != "", RestartUpdates: i.UpdatesOnStart}
	adapter := ResolveAdapter(i.GameType, nil)
	if adapter != nil {
		f.Console, f.ConsolePort, f.ServerInput = adapter.Name, adapter.DefaultPort, adapter.Transport == TransportContainerStdin
	}
	if i.Console != nil && i.Console.Announce != nil {
		f.AnnouncePort = i.Console.Announce.Port
	}
	if i.Query != nil {
		f.StatusCheck, f.StatusPort, f.StatusPortEnv = i.Query.Name(), i.Query.Port, i.Query.PortEnv
	} else {
		f.ImageHealthCheck = i.HealthCheck
	}
	switch {
	case adapter != nil && adapter.Players != nil && adapter.Players.Characters:
		f.Players = "Character names"
	case adapter != nil && adapter.Players != nil:
		f.Players = "Names"
	case i.Query != nil:
		f.Players = "Count"
	}
	if adapter != nil {
		var actions []string
		if adapter.Moderation.Offers(Kick) {
			actions = append(actions, "kick")
		}
		if adapter.Moderation.Offers(Ban) {
			actions = append(actions, "ban")
		}
		if adapter.Moderation != nil && adapter.Moderation.Lists != nil {
			actions = append(actions, "allowlist")
		}
		if len(actions) > 0 {
			text := strings.Join(actions, ", ")
			f.Moderation = strings.ToUpper(text[:1]) + text[1:]
		}
	}
	return f
}

// Extras reports whether the game gets anything beyond a console and a
// status check.
func (f Features) Extras() bool {
	return f.Players != "" || f.Moderation != "" || f.Warnings || f.RestartUpdates
}
