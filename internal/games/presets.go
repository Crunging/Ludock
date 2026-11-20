// Package games recognizes game images and describes their integrations:
// player ports, stop grace periods, and console adapters. Recognition selects
// an integration; it does not attest to an image's provenance or safety.
package games

import (
	"regexp"
	"slices"
	"strings"

	"github.com/Crunging/Ludock/internal/gamequery"
)

// QuickCommand is a console shortcut; a trailing space expects more input.
type QuickCommand struct {
	Label   string
	Command string
}

// ConsolePreset customizes a console adapter for a game.
type ConsolePreset struct {
	Adapter AdapterID
	// BroadcastTemplate is a fixed command; only generated, non-user text
	// replaces {message}.
	BroadcastTemplate string
	// StopCommand shuts the game down and saves, for a stdin console whose
	// game never gets Docker's stop signal.
	StopCommand           string
	Name                  string
	Placeholder           string
	Commands              []QuickCommand // nil uses the adapter's
	DefaultPort           int
	PasswordEnvCandidates []string
	// PasswordFile is a file in the container whose first line is the
	// console password.
	PasswordFile string
	// Players lists who is online through this preset's adapter, and
	// Moderation acts on them.
	Players    *PlayerQuery
	Moderation *Moderation
	Announce   *Announce
}

type Port struct {
	Port     int
	Protocol string
}

type Integration struct {
	GameType string
	Name     string
	// GamePort is the port players connect with, used to pick the address.
	GamePort     *Port
	Repositories []string
	// UpdatesOnStart: every listed image downloads game updates on startup.
	UpdatesOnStart bool
	Aliases        []string
	// StopTimeoutSeconds is the Docker stop grace period; 0 means 120.
	StopTimeoutSeconds int
	Console            *ConsolePreset
	// Query asks the game itself whether it answers, from its documented
	// default port.
	Query *StatusQuery
	// ServerFiles are folders where the images keep the game's own download,
	// left out of files and backups unless ludock.files names them.
	ServerFiles []string
	// SavesOnStop: the game saves its world when Docker stops it, so a stop
	// without a save command loses nothing.
	SavesOnStop bool
	// HealthCheck: the listed images ship a Docker health check, which
	// stands in for a status check. Only what the reference claims.
	HealthCheck bool
	// Saves are folders inside ServerFiles where the game keeps its worlds,
	// offered in place of a server-files mount that holds them.
	Saves []string
}

// Integrations lists the supported games. Repository suffixes omit
// registries.
var Integrations = []Integration{
	{GameType: "minecraft", Name: "Minecraft", GamePort: &Port{25565, "tcp"}, Repositories: []string{"itzg/minecraft-server"},
		UpdatesOnStart: true, Console: &ConsolePreset{Adapter: MinecraftRCON, BroadcastTemplate: "say {message}", Moderation: minecraftModeration},
		Query: &StatusQuery{Protocol: gamequery.MinecraftPing, Port: 25565, PortEnv: []string{"SERVER_PORT"}}},
	{GameType: "factorio", Name: "Factorio", HealthCheck: true, GamePort: &Port{34197, "udp"}, Repositories: []string{"factoriotools/factorio"},
		// The image generates the password and passes it on the command line.
		// Text that isn't a command is chat from <server>.
		Console: &ConsolePreset{Adapter: SourceRCON, Name: "Factorio RCON", DefaultPort: 27015,
			PasswordFile: "/factorio/config/rconpw", BroadcastTemplate: "{message}",
			Placeholder: "/players, /server-save, /config get",
			Commands:    []QuickCommand{{"List players", "/players online"}, {"Save world", "/server-save"}},
			Players:     &PlayerQuery{Command: "/players online", Parse: ParseFactorioPlayers}, Moderation: factorioModeration}},
	{GameType: "palworld", Name: "Palworld", HealthCheck: true, GamePort: &Port{8211, "udp"},
		Repositories:       []string{"thijsvanloef/palworld-server-docker", "jammsen/palworld-dedicated-server"},
		StopTimeoutSeconds: 180, UpdatesOnStart: true,
		// Both images download the server into /palworld and keep the world
		// inside it.
		ServerFiles: []string{"/palworld"}, Saves: []string{"/palworld/Pal/Saved"},
		// RCON's Broadcast sends only the first word, so it goes over the REST
		// API, which both images turn on by default.
		Console: &ConsolePreset{Adapter: SourceRCON, Name: "Palworld RCON", DefaultPort: 25575,
			BroadcastTemplate: "Broadcast {message}",
			Announce: &Announce{Prefix: "Broadcast ", Transport: TransportPalworldREST, Port: 8212,
				OnEnv: []string{"REST_API_ENABLED", "RESTAPI_ENABLED"}, OnValues: []string{"true", "1"},
				PortEnv: []string{"REST_API_PORT", "RESTAPI_PORT"}},
			Placeholder: "Info, ShowPlayers, Broadcast Hello",
			Commands:    []QuickCommand{{"List players", "ShowPlayers"}, {"Save world", "Save"}, {"Server info", "Info"}},
			Players:     &PlayerQuery{Command: "ShowPlayers", Parse: ParsePalworldPlayers}, Moderation: palworldModeration}},
	{GameType: "ark-survival-evolved", Name: "ARK: Survival Evolved", GamePort: &Port{7777, "udp"},
		Repositories:       []string{"hermsi/ark-server", "hermsi1337/ark-server", "indifferentbroccoli/ark-server-docker"},
		StopTimeoutSeconds: 180, Aliases: []string{"ark"}, Console: arkConsole(),
		Query: &StatusQuery{Protocol: gamequery.ValveA2S, Port: 27015, PortEnv: []string{"SERVER_LIST_PORT", "QUERY_PORT"}}},
	{GameType: "ark-survival-ascended", Name: "ARK: Survival Ascended", GamePort: &Port{7777, "udp"},
		Repositories:       []string{"sknnr/ark-ascended-server", "mschnitzer/asa-linux-server"},
		StopTimeoutSeconds: 180, Aliases: []string{"asa"}, Console: arkConsole()},
	{GameType: "cs2", Name: "Counter-Strike 2", GamePort: &Port{27015, "udp"}, Repositories: []string{"joedwards32/cs2"},
		UpdatesOnStart: true, Aliases: []string{"csgo", "counter-strike-2"},
		Console: &ConsolePreset{Adapter: SourceRCON, Name: "Source RCON", BroadcastTemplate: "say {message}", DefaultPort: 27015,
			Placeholder:           "status, changelevel de_dust2, say Hello",
			Commands:              []QuickCommand{{"Server status", "status"}, {"Message players", "say "}},
			PasswordEnvCandidates: []string{"CS2_RCONPW", "SRCDS_RCONPW", "RCON_PASSWORD"}},
		Query: &StatusQuery{Protocol: gamequery.ValveA2S, Port: 27015, PortEnv: []string{"CS2_PORT"}}},
	{GameType: "project-zomboid", Name: "Project Zomboid", GamePort: &Port{16261, "udp"},
		Repositories: []string{"renegademaster/zomboid-dedicated-server", "renegade-master/zomboid-dedicated-server",
			"renegade_master/zomboid-dedicated-server"},
		Aliases: []string{"projectzomboid"},
		Console: &ConsolePreset{Adapter: SourceRCON, Name: "Project Zomboid RCON", BroadcastTemplate: `servermsg "{message}"`,
			DefaultPort: 27015, Placeholder: "players, save, servermsg Hello",
			Commands: []QuickCommand{{"List players", "players"}, {"Save world", "save"}, {"Message players", "servermsg "}},
			Players:  &PlayerQuery{Command: "players", Parse: ParseZomboidPlayers}}},
	{GameType: "conan-exiles", Name: "Conan Exiles", GamePort: &Port{7777, "udp"},
		Repositories: []string{"indifferentbroccoli/conan-exiles-enhanced-server-docker"}, UpdatesOnStart: true,
		Console: &ConsolePreset{Adapter: SourceRCON, Name: "Conan Exiles RCON", BroadcastTemplate: "Broadcast {message}",
			DefaultPort: 25575, Commands: []QuickCommand{{"List players", "ListPlayers"}, {"Message players", "Broadcast "}},
			Players: &PlayerQuery{Command: "ListPlayers", Parse: ParseConanPlayers, Characters: true}},
		Query: &StatusQuery{Protocol: gamequery.ValveA2S, Port: 27015, PortEnv: []string{"QUERY_PORT"}}},
	{GameType: "v-rising", Name: "V Rising", GamePort: &Port{9876, "udp"}, Repositories: []string{"trueosiris/vrising"},
		Console: &ConsolePreset{Adapter: SourceRCON, Name: "V Rising RCON", DefaultPort: 25575,
			Commands: []QuickCommand{{"Message players", "announce "}}}},
	{GameType: "rust", Name: "Rust", GamePort: &Port{28015, "udp"}, Repositories: []string{"didstopia/rust-server"},
		UpdatesOnStart: true, Console: &ConsolePreset{Adapter: RustWebRCON, BroadcastTemplate: `say "{message}"`,
			PasswordEnvCandidates: []string{"RUST_RCON_PASSWORD", "RCON_PASSWORD"}}},
	{GameType: "7-days-to-die", StopTimeoutSeconds: 180, Name: "7 Days to Die", GamePort: &Port{26900, "tcp"},
		Repositories: []string{"vinanrra/7dtd-server"}, Aliases: []string{"7dtd"},
		Console: &ConsolePreset{Adapter: TelnetConsole, Name: "7 Days to Die Telnet", BroadcastTemplate: `say "{message}"`,
			DefaultPort: 8081, Placeholder: "listplayers, saveworld, say Hello",
			Commands: []QuickCommand{{"List players", "listplayers"}, {"Save world", "saveworld"}, {"Message players", "say "}},
			Players:  &PlayerQuery{Command: "listplayers", Parse: Parse7DaysPlayers}, Moderation: sevenDaysModeration}},
	{GameType: "valheim", StopTimeoutSeconds: 180, SavesOnStop: true, Name: "Valheim", GamePort: &Port{2456, "udp"},
		Repositories: []string{"community-valheim-tools/valheim-server", "lloesche/valheim-server"}, UpdatesOnStart: true,
		ServerFiles: []string{"/opt/valheim"},
		// Valheim answers on its game port plus one, and only when public.
		Query: &StatusQuery{Protocol: gamequery.ValveA2S, Port: 2457, OffEnv: "SERVER_PUBLIC", OffValues: []string{"false", "0"},
			OffReason: "Valheim answers the status check only while it’s listed publicly, and SERVER_PUBLIC is off."}},
	{GameType: "terraria", Name: "Terraria", GamePort: &Port{7777, "tcp"},
		Repositories: []string{"hexlo/terraria-server-docker", "beardedio/terraria", "ryshe/terraria"},
		// Every listed image runs the server where SIGTERM never reaches it:
		// under a shell that doesn't forward it, or as PID 1 without a handler.
		Console: &ConsolePreset{Adapter: StdinConsole, Name: "Terraria console", BroadcastTemplate: "say {message}",
			StopCommand: "exit", Placeholder: "playing, save, say Hello",
			Commands: []QuickCommand{{"List players", "playing"}, {"Save world", "save"}, {"Message players", "say "}},
			Players:  &PlayerQuery{Command: "playing", Parse: ParseTerrariaPlaying}, Moderation: terrariaModeration}},
	// From Bedrock 1.26.50 the default NetherNet transport connects over TCP
	// first, then plays on UDP ports the image leaves unset.
	{GameType: "minecraft-bedrock", Name: "Minecraft: Bedrock Edition", HealthCheck: true, GamePort: &Port{19132, "tcp"},
		Repositories: []string{"itzg/minecraft-bedrock-server"}, UpdatesOnStart: true, SavesOnStop: true, Aliases: []string{"bedrock"},
		Console: &ConsolePreset{Adapter: StdinConsole, Name: "Bedrock console", BroadcastTemplate: "say {message}",
			Placeholder: "list, gamerule dofiretick false, say Hello",
			Commands:    []QuickCommand{{"List players", "list"}, {"Message players", "say "}},
			Players:     &PlayerQuery{Command: "list", Parse: ParseBedrockList}, Moderation: bedrockModeration}},
	{GameType: "satisfactory", Name: "Satisfactory", GamePort: &Port{7777, "udp"},
		Repositories: []string{"robtme/satisfactory-server", "wolveix/satisfactory-server"}, UpdatesOnStart: true},
	{GameType: "enshrouded", Name: "Enshrouded", GamePort: &Port{15637, "udp"},
		Repositories: []string{"sknnr/enshrouded-dedicated-server", "mornedhels/enshrouded-server"}, UpdatesOnStart: true,
		// Enshrouded takes players and answers queries on one port.
		Query: &StatusQuery{Protocol: gamequery.ValveA2S, Port: 15637, PortEnv: []string{"SERVER_QUERYPORT", "PORT"}}},
	{GameType: "dont-starve-together", StopTimeoutSeconds: 360, Name: "Don't Starve Together", GamePort: &Port{10999, "udp"},
		Repositories: []string{"jamesits/dst-server"}, UpdatesOnStart: true, Aliases: []string{"dst", "don-t-starve-together"}},
	// Core Keeper connects through Steam's relay by default and takes direct
	// connections only on a port the image leaves unset.
	{GameType: "core-keeper", Name: "Core Keeper", Repositories: []string{"escaping/core-keeper-dedicated"},
		UpdatesOnStart: true, Aliases: []string{"corekeeper"}},
	{GameType: "vintage-story", Name: "Vintage Story", HealthCheck: true, GamePort: &Port{42420, "tcp"},
		Repositories: []string{"zsuatem/vintagestory", "devidian/vintagestory"}, Aliases: []string{"vintagestory"},
		Console: &ConsolePreset{Adapter: StdinConsole, Name: "Vintage Story console", BroadcastTemplate: "/announce {message}",
			Placeholder: "/list clients, /autosavenow, /announce Hello",
			Commands: []QuickCommand{{"List players", "/list clients"}, {"Save world", "/autosavenow"},
				{"Message players", "/announce "}},
			Players: &PlayerQuery{Command: "/list clients", Parse: ParseVintageStoryClients}, Moderation: vintageStoryModeration}},
}

func arkConsole() *ConsolePreset {
	return &ConsolePreset{Adapter: SourceRCON, Name: "ARK RCON", BroadcastTemplate: "Broadcast {message}", DefaultPort: 27020,
		Placeholder: "ListPlayers, SaveWorld, Broadcast Hello",
		Commands:    []QuickCommand{{"List players", "ListPlayers"}, {"Save world", "SaveWorld"}, {"Message players", "Broadcast "}},
		Players:     &PlayerQuery{Command: "ListPlayers", Parse: ParseARKPlayers}, Moderation: arkModeration}
}

// StopTimeout is the integration's stop grace period in seconds.
func (i *Integration) StopTimeout() int {
	if i == nil || i.StopTimeoutSeconds == 0 {
		return 120
	}
	return i.StopTimeoutSeconds
}

// InferGameType recognizes an image, or returns "unknown".
func InferGameType(image string) string {
	if integration := ImageIntegration(image); integration != nil {
		return integration.GameType
	}
	return "unknown"
}

// ImageIntegration finds the integration of an image. Startup behavior
// belongs to the recognized image, never a game label.
func ImageIntegration(image string) *Integration {
	repository := NormalizeImageRepository(image)
	for index := range Integrations {
		for _, known := range Integrations[index].Repositories {
			if repository == known || strings.HasSuffix(repository, "/"+known) {
				return &Integrations[index]
			}
		}
	}
	return nil
}

// DisplayName is a game's published name; label overrides keep their text.
func DisplayName(gameType string) string {
	if gameType == "unknown" {
		return "Other game"
	}
	if integration := Lookup(gameType); integration != nil {
		return integration.Name
	}
	return gameType
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Lookup finds an integration by game type or alias.
func Lookup(gameType string) *Integration {
	normalized := nonSlug.ReplaceAllString(strings.ToLower(strings.TrimSpace(gameType)), "-")
	for index := range Integrations {
		game := &Integrations[index]
		if game.GameType == normalized || slices.Contains(game.Aliases, normalized) {
			return game
		}
	}
	return nil
}

// NormalizeImageRepository strips a registry, tag, and digest.
func NormalizeImageRepository(image string) string {
	repository := strings.ToLower(strings.TrimSpace(image))
	repository, _, _ = strings.Cut(repository, "@")
	lastSlash := strings.LastIndex(repository, "/")
	if lastColon := strings.LastIndex(repository, ":"); lastColon > lastSlash {
		repository = repository[:lastColon]
	}
	if firstSlash := strings.Index(repository, "/"); firstSlash != -1 {
		first := repository[:firstSlash]
		if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
			repository = repository[firstSlash+1:]
		}
	}
	return repository
}
