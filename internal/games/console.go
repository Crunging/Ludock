package games

import "strings"

// Console labels.
const (
	LabelConsole            = "ludock.console"
	LabelConsolePort        = "ludock.console.port"
	LabelConsoleHost        = "ludock.console.host"
	LabelConsolePasswordEnv = "ludock.console.password-env"
	// LabelSecretEnv names more environment variables, comma-separated,
	// whose values never reach console or log output.
	LabelSecretEnv = "ludock.secret-env"
)

// AdapterID names a console adapter.
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
	TransportDockerExec     Transport = "docker-exec"
	TransportSourceRCON     Transport = "source-rcon"
	TransportRustWebRCON    Transport = "rust-webrcon"
	TransportTelnet         Transport = "telnet"
	TransportContainerStdin Transport = "container-stdin"
)

// Adapter is a resolved console adapter.
type Adapter struct {
	ID                    AdapterID
	Name                  string
	Transport             Transport
	CommandPlaceholder    string
	Commands              []QuickCommand
	DefaultPort           int
	PasswordEnvCandidates []string
}

var adapters = map[AdapterID]Adapter{
	MinecraftRCON: {ID: MinecraftRCON, Name: "Minecraft RCON", Transport: TransportDockerExec,
		CommandPlaceholder: "difficulty hard, whitelist add PlayerName, say Hello",
		Commands:           []QuickCommand{{"List players", "list"}, {"Save world", "save-all"}, {"Message players", "say "}}},
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

// MinecraftExec is the exec command for a Minecraft RCON console command.
func MinecraftExec(command string) []string {
	return []string{"rcon-cli", strings.TrimPrefix(command, "/")}
}

// ResolveAdapter picks the console adapter for a game type and its approved
// labels, or nil when the console is disabled or unsupported.
func ResolveAdapter(gameType string, labels map[string]string) *Adapter {
	configured := strings.ToLower(strings.TrimSpace(labels[LabelConsole]))
	if configured == "disabled" || configured == "none" {
		return nil
	}
	if adapter, ok := adapters[AdapterID(configured)]; ok && configured != "" {
		return &adapter
	}
	integration := Lookup(gameType)
	if integration == nil || integration.Console == nil {
		return nil
	}
	preset := integration.Console
	adapter := adapters[preset.Adapter]
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
