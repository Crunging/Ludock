package games

import (
	"strconv"
	"strings"
)

const (
	LabelConsole            = "ludock.console"
	LabelConsolePort        = "ludock.console.port"
	LabelConsoleHost        = "ludock.console.host"
	LabelConsolePasswordEnv = "ludock.console.password-env"
	// LabelSecretEnv names more environment variables, comma-separated,
	// whose values never reach console or log output.
	LabelSecretEnv = "ludock.secret-env"
)

type AdapterID string

const (
	MinecraftRCON AdapterID = "minecraft-rcon"
	SourceRCON    AdapterID = "source-rcon"
	RustWebRCON   AdapterID = "rust-webrcon"
	TelnetConsole AdapterID = "telnet-console"
	StdinConsole  AdapterID = "stdin-console"
)

// Transport is how a console reaches the game.
type Transport string

const (
	TransportSourceRCON     Transport = "source-rcon"
	TransportRustWebRCON    Transport = "rust-webrcon"
	TransportTelnet         Transport = "telnet"
	TransportContainerStdin Transport = "container-stdin"
	TransportPalworldREST   Transport = "palworld-rest"
)

// Announce sends the message of a command that starts with Prefix over
// another transport of the game, beside the console, with the console's
// password.
type Announce struct {
	Prefix    string
	Transport Transport
	Port      int
	// OnEnv and PortEnv are the variables images use to turn the transport
	// on, with one of OnValues in any case, and to move its port; the first
	// one set wins. Images set their defaults in their own environment,
	// which containers inherit.
	OnEnv    []string
	OnValues []string
	PortEnv  []string
}

func firstSet(env map[string]string, names []string) (string, string) {
	for _, name := range names {
		if value := strings.TrimSpace(env[name]); value != "" {
			return name, value
		}
	}
	return "", ""
}

// On reports whether the container's environment turns the transport on.
func (a *Announce) On(env map[string]string) bool {
	_, value := firstSet(env, a.OnEnv)
	for _, on := range a.OnValues {
		if strings.EqualFold(value, on) {
			return true
		}
	}
	return false
}

// PortFor is the transport's port in the container, or 0 and the variable
// when that variable isn't a port.
func (a *Announce) PortFor(env map[string]string) (int, string) {
	name, raw := firstSet(env, a.PortEnv)
	if raw == "" {
		return a.Port, ""
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, name
	}
	return port, name
}

type Adapter struct {
	ID                    AdapterID
	Name                  string
	Transport             Transport
	CommandPlaceholder    string
	Commands              []QuickCommand
	DefaultPort           int
	PasswordEnvCandidates []string
	// PasswordFile is a file in the container holding the password, read
	// when no candidate variable is set: on its PasswordKey line, such as
	// "password=<value>", or without a key on its first line.
	PasswordFile, PasswordKey string
	// PasswordProperty instead reads the file as XML: the value of its
	// <property name="PasswordProperty" value="..."/>. An empty value means
	// the console has no password.
	PasswordProperty string
	// TrimSlash drops a leading "/" from commands, which the game's console
	// doesn't take.
	TrimSlash bool
	// Players lists who is online, or is nil when Ludock can't.
	Players *PlayerQuery
	// Moderation acts on players in that list, or is nil.
	Moderation *Moderation
	// Announce, when set, sends announcements another way.
	Announce *Announce
}

var adapters = map[AdapterID]Adapter{
	// The itzg image writes the password it was given, or generated, to
	// /data/.rcon-cli.env for rcon-cli.
	MinecraftRCON: {ID: MinecraftRCON, Name: "Minecraft RCON", Transport: TransportSourceRCON, DefaultPort: 25575,
		CommandPlaceholder:    "difficulty hard, whitelist add PlayerName, say Hello",
		Commands:              []QuickCommand{{"List players", "list"}, {"Save world", "save-all"}, {"Message players", "say "}},
		PasswordEnvCandidates: []string{"RCON_PASSWORD"}, PasswordFile: "/data/.rcon-cli.env", PasswordKey: "password=",
		TrimSlash: true,
		Players:   &PlayerQuery{Command: "list", Parse: ParseMinecraftList}},
	SourceRCON: {ID: SourceRCON, Name: "RCON", Transport: TransportSourceRCON, CommandPlaceholder: "status, save, broadcast Hello",
		Commands:              []QuickCommand{},
		PasswordEnvCandidates: []string{"RCON_PASSWORD", "ADMIN_PASSWORD", "SERVER_ADMIN_PASSWORD", "ARK_ADMIN_PASSWORD", "SRCDS_RCONPW"}},
	RustWebRCON: {ID: RustWebRCON, Name: "Rust WebRCON", Transport: TransportRustWebRCON, DefaultPort: 28016,
		CommandPlaceholder:    "status, say Hello, server.save",
		Commands:              []QuickCommand{{"Server status", "status"}, {"Save world", "server.save"}, {"Message players", "say "}},
		PasswordEnvCandidates: []string{"RCON_PASSWORD"}},
	TelnetConsole: {ID: TelnetConsole, Name: "Telnet console", Transport: TransportTelnet,
		CommandPlaceholder: "help, listplayers, say Hello", Commands: []QuickCommand{}, PasswordEnvCandidates: []string{"TELNET_PASSWORD"}},
	StdinConsole: {ID: StdinConsole, Name: "Server console", Transport: TransportContainerStdin,
		CommandPlaceholder: "help, playing, save", Commands: []QuickCommand{}},
}

// Command is a console command as the game takes it.
func (a *Adapter) Command(command string) string {
	if a.TrimSlash {
		return strings.TrimPrefix(command, "/")
	}
	return command
}

// ResolveAdapter picks the console adapter for a game type and its approved
// labels, or nil when the console is disabled or unsupported.
func ResolveAdapter(gameType string, labels map[string]string) *Adapter {
	configured := strings.ToLower(strings.TrimSpace(labels[LabelConsole]))
	if configured == "disabled" || configured == "none" {
		return nil
	}
	integration := Lookup(gameType)
	// A label naming the game's own adapter changes nothing. Any other
	// adapter comes bare: the game's port, password, commands, player list
	// and moderation belong to its own console.
	if adapter, ok := adapters[AdapterID(configured)]; ok && configured != "" &&
		(integration == nil || integration.Console == nil || integration.Console.Adapter != adapter.ID) {
		return &adapter
	}
	if integration == nil || integration.Console == nil {
		return nil
	}
	preset := integration.Console
	adapter := adapters[preset.Adapter]
	if preset.Players != nil {
		adapter.Players = preset.Players
	}
	adapter.Moderation, adapter.Announce = preset.Moderation, preset.Announce
	if preset.Name != "" {
		adapter.Name = preset.Name
	}
	if preset.Placeholder != "" {
		adapter.CommandPlaceholder = preset.Placeholder
	}
	if preset.Commands != nil {
		adapter.Commands = preset.Commands
	}
	if preset.DefaultPort != 0 {
		adapter.DefaultPort = preset.DefaultPort
	}
	if preset.PasswordEnvCandidates != nil {
		adapter.PasswordEnvCandidates = preset.PasswordEnvCandidates
	}
	if preset.PasswordFile != "" {
		adapter.PasswordFile, adapter.PasswordKey, adapter.PasswordProperty = preset.PasswordFile, "", preset.PasswordProperty
	}
	return &adapter
}

// BroadcastTemplate returns the game's broadcast command, or "". A transport
// override alone cannot establish a game's broadcast syntax.
func BroadcastTemplate(gameType string, labels map[string]string) string {
	integration := Lookup(gameType)
	adapter := ResolveAdapter(gameType, labels)
	if integration == nil || integration.Console == nil || adapter == nil ||
		integration.Console.BroadcastTemplate == "" || adapter.ID != integration.Console.Adapter {
		return ""
	}
	return integration.Console.BroadcastTemplate
}

// StopCommand returns the game's shutdown command for its stdin console, or
// "" when it has none or a label names another console.
func StopCommand(gameType string, labels map[string]string) string {
	integration := Lookup(gameType)
	adapter := ResolveAdapter(gameType, labels)
	if integration == nil || integration.Console == nil || adapter == nil || adapter.Transport != TransportContainerStdin ||
		adapter.ID != integration.Console.Adapter {
		return ""
	}
	return integration.Console.StopCommand
}
