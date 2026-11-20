package games

import (
	"slices"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/gamequery"
)

func TestParseAddress(t *testing.T) {
	for input, want := range map[string]Address{
		" Play.Example.com ": {"play.example.com", 0},
		"203.0.113.10:30000": {"203.0.113.10", 30000},
		"[2001:db8::1]:2456": {"2001:db8::1", 2456},
		"[2001:db8::1]":      {"2001:db8::1", 0},
		"2001:db8::1":        {"2001:db8::1", 0},
	} {
		if got := ParseAddress(input); got == nil || *got != want {
			t.Errorf("%q: %+v", input, got)
		}
	}
	for _, invalid := range []string{"", "https://play.example.com", "play.example.com:0", "play.example.com:65536",
		"play.example.com:25565/udp", "[play.example.com]:25565", "two words", "host:port:1"} {
		if got := ParseAddress(invalid); got != nil {
			t.Errorf("%q: %+v", invalid, got)
		}
	}
}

func TestEligibility(t *testing.T) {
	for _, c := range []struct {
		image  string
		labels map[string]string
		reason string
	}{
		{"itzg/minecraft-server:java21", nil, ReasonRecognizedImage},
		{"ghcr.io/itzg/minecraft-server@sha256:abc", nil, ReasonRecognizedImage},
		{"example/custom", nil, ReasonUnrecognizedImage},
		{"example/custom", map[string]string{LabelEnable: " TRUE "}, ReasonExplicitlyEnabled},
		{"itzg/minecraft-server", map[string]string{LabelEnable: "false"}, ReasonOptedOut},
		{"itzg/minecraft-server", map[string]string{LabelEnable: "yes"}, ReasonInvalidEnableLabel},
		{"itzg/minecraft-server", map[string]string{"com.docker.compose.oneoff": "True"}, ReasonComposeOneoff},
	} {
		if got := Evaluate(c.image, c.labels); got.Reason != c.reason {
			t.Errorf("%s %v: %s", c.image, c.labels, got.Reason)
		}
	}
	if NormalizeImageRepository("localhost:5000/Itzg/Minecraft-Server:latest") != "itzg/minecraft-server" {
		t.Fatal(NormalizeImageRepository("localhost:5000/Itzg/Minecraft-Server:latest"))
	}
}

func TestConsoleAdapters(t *testing.T) {
	if adapter := ResolveAdapter("ark", nil); adapter == nil || adapter.ID != SourceRCON || adapter.DefaultPort != 27020 || adapter.Name != "ARK RCON" {
		t.Fatalf("%+v", adapter)
	}
	if BroadcastTemplate("ark", nil) != "Broadcast {message}" {
		t.Fatal("ark broadcast")
	}
	// A transport override alone cannot establish broadcast syntax.
	if BroadcastTemplate("ark", map[string]string{LabelConsole: "telnet-console"}) != "" {
		t.Fatal("override kept broadcast")
	}
	if ResolveAdapter("minecraft", map[string]string{LabelConsole: "Disabled"}) != nil {
		t.Fatal("disabled console resolved")
	}
	if adapter := ResolveAdapter("valheim", nil); adapter != nil {
		t.Fatal("valheim has no console")
	}
}

// Only Terraria's own stdin console types its shutdown command; a label that
// turns the console off or names another one stops it.
func TestStopCommand(t *testing.T) {
	for _, test := range []struct {
		game   string
		labels map[string]string
		want   string
	}{
		{"terraria", nil, "exit"},
		{"terraria", map[string]string{LabelConsole: "stdin-console"}, "exit"},
		{"terraria", map[string]string{LabelConsole: "disabled"}, ""},
		{"terraria", map[string]string{LabelConsole: "source-rcon"}, ""},
		{"minecraft", nil, ""},
		{"vintage-story", nil, ""},
		{"unknown", nil, ""},
	} {
		if got := StopCommand(test.game, test.labels); got != test.want {
			t.Errorf("%s %v: %q", test.game, test.labels, got)
		}
	}
}

func TestComposeIdentity(t *testing.T) {
	complete := map[string]string{"com.docker.compose.project": "games", "com.docker.compose.service": "mc", "com.docker.compose.container-number": "1"}
	if InvalidComposeIdentity(complete) {
		t.Fatal("complete identity rejected")
	}
	for _, change := range []func(map[string]string){
		func(l map[string]string) { delete(l, "com.docker.compose.service") },
		func(l map[string]string) { l["com.docker.compose.container-number"] = "01" },
		func(l map[string]string) { l["com.docker.compose.project"] = "a\nb" },
	} {
		labels := map[string]string{}
		for k, v := range complete {
			labels[k] = v
		}
		change(labels)
		if !InvalidComposeIdentity(labels) {
			t.Errorf("accepted %v", labels)
		}
	}
}

// The list command's wording differs across Minecraft versions and plugins;
// anything else is not a player list.
func TestParseMinecraftList(t *testing.T) {
	for response, want := range map[string]Players{
		"There are 0 of a max of 20 players online: ":           {Online: 0, Max: 20, Names: nil},
		"There are 2 of a max of 20 players online: Mara, Theo": {Online: 2, Max: 20, Names: []string{"Mara", "Theo"}},
		"There are 1/10 players online:Steve":                   {Online: 1, Max: 10, Names: []string{"Steve"}},
		"There are 2/10 players online:\nAlex, Steve\n":         {Online: 2, Max: 10, Names: []string{"Alex", "Steve"}},
		"§6There are §c3§6 out of maximum §c50§6 players online.\n§6Admins§r: §fMara\n§6Default§r: Theo, Juno": {Online: 3, Max: 50,
			Names: []string{"Mara", "Theo", "Juno"}},
		// Some servers hide names; the count still stands.
		"There are 4 of a max of 8 players online:": {Online: 4, Max: 8, Names: nil},
	} {
		got, ok := ParseMinecraftList(response)
		if !ok || got.Online != want.Online || got.Max != want.Max || strings.Join(got.Names, "|") != strings.Join(want.Names, "|") {
			t.Errorf("%q: %+v %v", response, got, ok)
		}
	}
	for _, response := range []string{"", "Unknown command", "Command completed with no response", "There are many players online"} {
		if got, ok := ParseMinecraftList(response); ok {
			t.Errorf("%q parsed as %+v", response, got)
		}
	}
	long, _ := ParseMinecraftList("There are 300 of a max of 500 players online: " + strings.Repeat(strings.Repeat("x", 80)+", ", 300))
	if len(long.Names) != maxPlayerNames || len([]rune(long.Names[0])) != MaxNameRunes {
		t.Fatalf("%d names, first %d runes", len(long.Names), len([]rune(long.Names[0])))
	}
}

type playerCase struct {
	response string
	online   int
	names    []string
}

func checkPlayers(t *testing.T, parse func(string) (Players, bool), lists []playerCase, rejected []string) {
	t.Helper()
	for _, c := range lists {
		got, ok := parse(c.response)
		if !ok || got.Online != c.online || got.Max != 0 || strings.Join(got.Names, "|") != strings.Join(c.names, "|") {
			t.Errorf("%q: %+v %v", c.response, got, ok)
		}
	}
	for _, response := range rejected {
		if got, ok := parse(response); ok {
			t.Errorf("%q parsed as %+v", response, got)
		}
	}
}

// Factorio indents each player under a counted header, and can mix log
// lines into the reply; a count that doesn't match its rows is not trusted.
func TestParseFactorioPlayers(t *testing.T) {
	checkPlayers(t, ParseFactorioPlayers, []playerCase{
		{"Online players (0):\n", 0, nil},
		{"Online players (1):\r\n  Ada (online)\r\n", 1, []string{"Ada"}},
		{"\nOnline players (2):\n  Zed (online)\n  Alice_Smith (online)\n", 2, []string{"Zed", "Alice_Smith"}},
		{"[JOIN] Zed joined the game\nOnline players (1):\n  Zed (online)\n", 1, []string{"Zed"}},
	}, []string{
		"", "Unknown command \"player\".", "Online players (1):\n", "Online players (0):\n  Ada (online)\n",
		"Online players (1):\nAda (online)\n", "Online players (1):\n  Ada (offline)\n",
		"Players (1):\n  Ada (online)\n", "Online players (1):\n  Ada (online)\nOnline players (1):\n  Ada (online)\n",
		"  Ada (online)\nOnline players (1):\n", "Online players (2):\n  Ada (online)\n  Ada (online)\n",
		"Online players (1):\n  Ada\x00 (online)\n", "Online players (0):\n[garbled]\n",
		"Online players (2):\n  Ada (online)\n  \u00a0Ada (online)\n",
	})
}

// Palworld answers with a CSV table, padded with NULs on some versions;
// names keep their commas.
func TestParsePalworldPlayers(t *testing.T) {
	checkPlayers(t, ParsePalworldPlayers, []playerCase{
		{"name,playeruid,steamid\n", 0, nil},
		{"name,playeruid,steamid\n\x00\x00", 0, nil},
		{"name,playeruid,steamid\nAda,1234567890,76561198000000000\nZed,987654321,76561198000000001\n", 2, []string{"Ada", "Zed"}},
		{"name,playeruid,steamid\nSmith, Jo,1234567890,76561198000000000\n", 1, []string{"Smith, Jo"}},
		// A player still joining has no name or ID yet, but is online.
		{"name,playeruid,steamid\n,00000000,00000000000000000\n", 1, nil},
	}, []string{"", "Unknown command", "Ada,1234567890,76561198000000000", "name,playeruid,steamid\nAda,1234567890\n",
		"name,playeruid,steamid\nname,playeruid,steamid\n", "name,playeruid,steamid\nAda,,\n",
		"name,playeruid,steamid\nAda,12\x0034,76561198000000000\n"})
}

// ARK numbers rows from 0, with Steam IDs on Survival Evolved and EOS IDs
// on Survival Ascended, and says so when nobody is online.
func TestParseARKPlayers(t *testing.T) {
	checkPlayers(t, ParseARKPlayers, []playerCase{
		{"No Players Connected\n \n", 0, nil},
		{"\n0. Ada, 76561198000000000\n1. Zed Smith, 76561198000000001\n \n", 2, []string{"Ada", "Zed Smith"}},
		{"0. Ada, Lovelace, 0002a1b2c3d4e5f60718293a4b5c6d7e\n", 1, []string{"Ada, Lovelace"}},
		// Survival Evolved gives Epic Games Store players 19-digit IDs.
		{"0. Ada, 76561198000000000\n1. Zed, 2535412345678901234\n", 2, []string{"Ada", "Zed"}},
	}, []string{
		"", " \n", "Server received, But no response!! \n", "1. Ada, 76561198000000000\n",
		"0. Ada, 76561198000000000\n0. Zed, 76561198000000001\n", "0. Ada 76561198000000000\n",
		"01. SteamID: 76561197960435530, Name: Haldor, Ping: 18\n",
		"0. Ada, 76561198000000000\nNo Players Connected\n", "0. Ada, 7656119800\n", "0. Ada, 0002a1b2c3d4e5f6\n",
	})
}

// Project Zomboid counts players in its header, then lists each after a dash.
func TestParseZomboidPlayers(t *testing.T) {
	checkPlayers(t, ParseZomboidPlayers, []playerCase{
		{"Players connected (0): \n", 0, nil},
		{"Players connected (2): \n-Ada\n-Zed Smith\n", 2, []string{"Ada", "Zed Smith"}},
	}, []string{
		"", "Unknown command players", "Players connected (1): \n", "Players connected (0): \n-Ada\n",
		"Players connected (1): \nAda\n", "Players connected (1): \n-\n", "Players connected (2): <LINE> -Ada <LINE> -Zed <LINE> ",
	})
}

// Conan Exiles answers with a table whose header alone means nobody is
// online; a character's name stands for its player.
func TestParseConanPlayers(t *testing.T) {
	header := "Idx | Char name | Player name | User ID | Platform ID | Platform Name\n"
	checkPlayers(t, ParseConanPlayers, []playerCase{
		{header, 0, nil},
		{header + "  0 | Ada       | ada_1       | 1A2B3C4D | 76561198000000000 | Steam\n" +
			"  1 |           | zed_2       | 5E6F7A8B | 76561198000000001 | Steam\n", 2, []string{"Ada", "zed_2"}},
	}, []string{
		"", "Couldn't find the command: ListPlayers", "Idx | Player name\n",
		header + "  0 | Ada | ada_1 | 1A2B3C4D\n", header + "  x | Ada | ada_1 | 1A2B3C4D | 7656 | Steam\n",
		// A header cut short could be any table.
		"Idx | Char name | Player name\n",
		header + "  0 | Ada | ada_1 | 1A | 7656 | Steam\n  0 | Zed | zed_2 | 2B | 7657 | Steam\n",
		header + " -1 | Ada | ada_1 | 1A | 7656 | Steam\n", header + "  0 |  |  |  |  |\n",
	})
}

// The 7 Days to Die Telnet console streams its log into the reply; only the
// player rows and their total count.
func TestParse7DaysPlayers(t *testing.T) {
	row := "0. id=171, Aona Suzutsuki, pos=(741.9, 45.1, 951.5), rot=(-11.2, -406.4, 0.0), remote=True, health=92, " +
		"deaths=4, zombies=4, players=1, score=2, level=1, pltfmid=Steam_76561198010715714, " +
		"crossid=EOS_0002bfc568d6401ca9de387e0ae914c9, ip=192.168.0.81, ping=0"
	log := "2026-11-19T10:00:00 512.345 INF Executing command 'listplayers' by Telnet from 127.0.0.1:50000"
	checkPlayers(t, Parse7DaysPlayers, []playerCase{
		{"Total of 0 in the game\r\n", 0, nil},
		{log + "\r\n" + row + "\r\n" + strings.Replace(row, "0. id=171, Aona Suzutsuki", "1. id=172, Zed", 1) +
			"\r\nTotal of 2 in the game\r\n", 2, []string{"Aona Suzutsuki", "Zed"}},
	}, []string{"", log, row, row + "\nTotal of 2 in the game", "Total of 0 in the game\nTotal of 0 in the game",
		// A row cut short, or text that isn't a row or the server log.
		"0. id=171, Ada, pos=(\nTotal of 1 in the game", "0. id=171, Ada, p\x00os=(\nTotal of 0 in the game",
		"garbled\nTotal of 0 in the game", "0. id=171, Ada, pos=(garbage, ping=0\nTotal of 1 in the game", "0. id=171, Ada, pos=(1, 2, 3), rot=(4, 5, 6), remote=True, garbage, ping=0\nTotal of 1 in the game", "Total of 1 in the game\n" + row})
}

// Each game with a player list gets it from its preset, also when a label
// names the game's own adapter; other adapters and games have none.
func TestResolveAdapterPlayers(t *testing.T) {
	for game, command := range map[string]string{"minecraft": "list", "factorio": "/players online", "palworld": "ShowPlayers",
		"ark-survival-evolved": "ListPlayers", "ark-survival-ascended": "ListPlayers", "project-zomboid": "players",
		"conan-exiles": "ListPlayers", "7-days-to-die": "listplayers", "terraria": "playing", "minecraft-bedrock": "list",
		"vintage-story": "/list clients"} {
		if adapter := ResolveAdapter(game, nil); adapter == nil || adapter.Players == nil || adapter.Players.Command != command {
			t.Errorf("%s: %+v", game, adapter)
		}
	}
	if adapter := ResolveAdapter("palworld", map[string]string{LabelConsole: "source-rcon"}); adapter.Players == nil {
		t.Error("the game's own adapter lost its player list")
	}
	for game, labels := range map[string]map[string]string{"cs2": nil, "v-rising": nil, "rust": nil,
		"palworld": {LabelConsole: "telnet-console"}, "unknown": {LabelConsole: "source-rcon"}} {
		if adapter := ResolveAdapter(game, labels); adapter != nil && adapter.Players != nil {
			t.Errorf("%s %v lists players", game, labels)
		}
	}
}

// Labels turn a game's status query off, move its port, or enable one for
// a game without a preset; an invalid port turns it off rather than
// guessing. The image's documented port variable wins over the default.
func TestResolveQuery(t *testing.T) {
	cases := []struct {
		game     string
		labels   map[string]string
		env      map[string]string
		protocol gamequery.Protocol
		port     int
	}{
		{"minecraft", nil, nil, gamequery.MinecraftPing, 25565},
		{"minecraft", nil, map[string]string{"SERVER_PORT": "25570"}, gamequery.MinecraftPing, 25570},
		{"minecraft", map[string]string{LabelQueryPort: "30000"}, map[string]string{"SERVER_PORT": "25570"}, gamequery.MinecraftPing, 30000},
		{"minecraft", nil, map[string]string{"SERVER_PORT": "nope"}, gamequery.MinecraftPing, 0},
		{"valheim", nil, nil, gamequery.ValveA2S, 2457},
		{"ark", nil, map[string]string{"QUERY_PORT": "27016"}, gamequery.ValveA2S, 27016},
		{"enshrouded", nil, nil, gamequery.ValveA2S, 15637},
		{"enshrouded", nil, map[string]string{"SERVER_QUERYPORT": "15700"}, gamequery.ValveA2S, 15700},
		{"enshrouded", nil, map[string]string{"PORT": "15800"}, gamequery.ValveA2S, 15800},
		{"minecraft-bedrock", nil, nil, "", 0},
		{"satisfactory", nil, nil, "", 0},
		{"dont-starve-together", nil, nil, "", 0},
		{"core-keeper", nil, nil, "", 0},
		{"vintage-story", nil, nil, "", 0},
		{"v-rising", map[string]string{LabelQuery: "a2s", LabelQueryPort: "9877"}, nil, gamequery.ValveA2S, 9877},
		{"minecraft", map[string]string{LabelQuery: "A2S", LabelQueryPort: "25565"}, nil, gamequery.ValveA2S, 25565},
		{"v-rising", map[string]string{LabelQuery: "a2s"}, nil, "", 0},
		{"factorio", nil, nil, "", 0},
		{"minecraft", map[string]string{LabelQuery: "disabled"}, nil, "", 0},
		{"minecraft", map[string]string{LabelQuery: "gopher"}, nil, "", 0},
		{"minecraft", map[string]string{LabelQueryPort: "70000"}, nil, "", 0},
		{"minecraft", map[string]string{LabelQueryPort: ""}, nil, "", 0},
	}
	for _, c := range cases {
		query := ResolveQuery(c.game, c.labels)
		if c.protocol == "" {
			if query != nil {
				t.Errorf("%s %v: %+v", c.game, c.labels, query)
			}
			continue
		}
		if query == nil || query.Protocol != c.protocol || query.QueryPort(c.env) != c.port {
			t.Errorf("%s %v %v: %+v", c.game, c.labels, c.env, query)
		}
	}
}

// Each image is recognized by its repository alone, whether pulled from
// Docker Hub or another registry, by tag or by digest; a lookalike is not.
func TestImageIntegrationRecognizesNewGames(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("a", 64)
	for repository, game := range map[string]string{
		"itzg/minecraft-bedrock-server":     "minecraft-bedrock",
		"robtme/satisfactory-server":        "satisfactory",
		"wolveix/satisfactory-server":       "satisfactory",
		"sknnr/enshrouded-dedicated-server": "enshrouded",
		"mornedhels/enshrouded-server":      "enshrouded",
		"jamesits/dst-server":               "dont-starve-together",
		"escaping/core-keeper-dedicated":    "core-keeper",
		"zsuatem/vintagestory":              "vintage-story",
		"devidian/vintagestory":             "vintage-story",
	} {
		for _, image := range []string{repository, "docker.io/" + repository + ":latest", "ghcr.io/" + repository + ":1.2.3",
			repository + digest, "registry.example.com:5000/" + repository + ":stable" + digest} {
			if got := InferGameType(image); got != game {
				t.Errorf("%s: %s", image, got)
			}
		}
	}
	for _, image := range []string{"itzg/minecraft-bedrock-server-proxy", "sknnr/enshrouded", "vintagestory", "escaping/core-keeper"} {
		if got := InferGameType(image); got != "unknown" {
			t.Errorf("%s: %s", image, got)
		}
	}
	// Java and Bedrock editions stay apart.
	if InferGameType("itzg/minecraft-server") != "minecraft" {
		t.Error("java edition")
	}
}

// The new games connect where their images document, wait longer to stop
// only where an image says the game needs it, and update on start only when
// every listed image does.
func TestNewGameIntegrations(t *testing.T) {
	for game, want := range map[string]struct {
		port    *Port
		stop    int
		updates bool
	}{
		"minecraft-bedrock":    {&Port{19132, "tcp"}, 120, true},
		"satisfactory":         {&Port{7777, "udp"}, 120, true},
		"enshrouded":           {&Port{15637, "udp"}, 120, true},
		"dont-starve-together": {&Port{10999, "udp"}, 360, true},
		"core-keeper":          {nil, 120, true},
		"vintage-story":        {&Port{42420, "tcp"}, 120, false},
	} {
		integration := Lookup(game)
		if integration == nil {
			t.Errorf("%s: missing", game)
			continue
		}
		if (integration.GamePort == nil) != (want.port == nil) || (want.port != nil && *integration.GamePort != *want.port) ||
			integration.StopTimeout() != want.stop || integration.UpdatesOnStart != want.updates {
			t.Errorf("%s: %+v", game, integration)
		}
	}
	for alias, game := range map[string]string{"bedrock": "minecraft-bedrock", "dst": "dont-starve-together",
		"Don't Starve Together": "dont-starve-together", "corekeeper": "core-keeper", "VintageStory": "vintage-story"} {
		if integration := Lookup(alias); integration == nil || integration.GameType != game {
			t.Errorf("%s: %+v", alias, integration)
		}
	}
}

// Bedrock and Vintage Story read commands from the process's input, with
// their own broadcast syntax and a list read from their output; the others
// have no console Ludock speaks.
func TestNewGameConsoles(t *testing.T) {
	for game, broadcast := range map[string]string{"minecraft-bedrock": "say {message}", "vintage-story": "/announce {message}"} {
		adapter := ResolveAdapter(game, nil)
		if adapter == nil || adapter.Transport != TransportContainerStdin || adapter.Players == nil ||
			BroadcastTemplate(game, nil) != broadcast {
			t.Errorf("%s: %+v", game, adapter)
		}
	}
	// Bedrock has no single save command; the stop doesn't send one.
	for _, command := range ResolveAdapter("minecraft-bedrock", nil).Commands {
		if command.Label == "Save world" {
			t.Error("bedrock save shortcut")
		}
	}
	for _, game := range []string{"satisfactory", "enshrouded", "dont-starve-together", "core-keeper"} {
		if adapter := ResolveAdapter(game, nil); adapter != nil {
			t.Errorf("%s: %+v", game, adapter)
		}
	}
}

// Terraria 1.4.5.8 on hexlo/terraria-server-docker, with tty, prints its
// replies to playing among other lines, behind its prompt and terminal
// codes. These are its replies with two real players, one, and none; the
// rows give each player's address, which is never kept.
func TestParseTerrariaPlaying(t *testing.T) {
	two := "\x1b]0;Terraria Server: rig\x07: \x1b[6nServer started\r\n172.18.0.1:47558 is connecting...\r\nZed_2 has joined.\r\n" +
		"playing\r\nAda (172.18.0.1:47554)\r\nZed_2 (172.18.0.1:47558)\r\n2 players connected.\r\n"
	one := ": \x1b[6nZed_2 has left.\r\nkick Nobody\r\n: \x1b[6nplaying\r\nAda (172.18.0.1:47554)\r\n1 player connected.\r\n"
	none := "ban Nobody\r\n: \x1b[6nplaying\r\nNo players connected.\r\n"
	cases := []struct {
		output string
		names  []string
	}{{two, []string{"Ada", "Zed_2"}}, {one, []string{"Ada"}}, {none, nil}}
	for _, c := range cases {
		got, ok := ParseTerrariaPlaying(c.output)
		if !ok || got.Online != len(c.names) || !slices.Equal(got.Names, c.names) || !slices.Equal(got.IDs, c.names) {
			t.Errorf("%q: %+v %v", c.output, got, ok)
		}
	}
	for _, output := range []string{
		// The count hasn't arrived, or its line hasn't ended.
		"playing\r\nAda (172.18.0.1:47554)\r\n", "playing\r\nNo players connected.",
		// Rows missing above the count, or something else in their place.
		"playing\r\n2 players connected.\r\n", "playing\r\n<Ada> hi\r\nZed (172.18.0.1:1)\r\n2 players connected.\r\n",
		// A player's chat can't pass for the reply.
		"<Ada> No players connected.\r\n", "",
	} {
		if got, ok := ParseTerrariaPlaying(output); ok {
			t.Errorf("%q: %+v", output, got)
		}
	}
}

// Vintage Story 1.21 on zsuatem/vintagestory, with tty, answers /list clients
// in its log: the empty reply is real; the row follows the server's own
// format for a connected client, "{4} [{0}] {1} {2} ({3}ms) ({5}s inactive)",
// since a player needs a game account.
func TestParseVintageStoryClients(t *testing.T) {
	empty := "\x1b[39;49m/list clients\r\n20.11.2026 19:04:32 [Server Notification] Handling Console Command /list clients\r\n" +
		"\x1b[39;49m20.11.2026 19:04:32 [Server Notification] List of online Players\r\n\r\n"
	if got, ok := ParseVintageStoryClients(empty); !ok || got.Online != 0 {
		t.Errorf("empty: %+v %v", got, ok)
	}
	two := "\x1b[39;49m20.11.2026 19:04:32 [Server Notification] List of online Players\r\n" +
		" [3] Ada 172.18.0.1:51234 (12ms) (0s inactive)\r\n [4] Zed_2 172.18.0.1:51236 (9ms) (31s inactive)\r\n\r\n"
	if got, ok := ParseVintageStoryClients(two); !ok || !slices.Equal(got.Names, []string{"Ada", "Zed_2"}) || !slices.Equal(got.IDs, got.Names) {
		t.Errorf("two: %+v %v", got, ok)
	}
	for _, output := range []string{
		// The blank line that ends the reply hasn't arrived.
		"20.11.2026 19:04:32 [Server Notification] List of online Players\r\n [3] Ada 172.18.0.1:51234 (12ms) (0s inactive)\r\n",
		// Another line in place of a row, and chat that repeats the header.
		"20.11.2026 19:04:32 [Server Notification] List of online Players\r\n20.11.2026 19:04:33 [Server Event] Ada joined\r\n\r\n",
		"20.11.2026 19:04:32 [Server Chat] Ada: List of online Players\r\n\r\n", "",
	} {
		if got, ok := ParseVintageStoryClients(output); ok {
			t.Errorf("%q: %+v", output, got)
		}
	}
}

// Bedrock 1.26.66 on itzg/minecraft-bedrock-server answers list in its log:
// a count line with its time and level, then the names line, which is blank
// with nobody online. The empty reply is real; the names follow the game's
// commands.players.list.names, since joining needs a Microsoft account.
func TestParseBedrockList(t *testing.T) {
	cases := map[string][]string{
		"list\r\n[2026-11-20 19:11:52:477 INFO] There are 0/10 players online:\r\n\r\n":              nil,
		"list\r\n[2026-11-20 19:11:52:477 INFO] There are 2/10 players online:\r\nSteve42, Alex\r\n": {"Steve42", "Alex"},
		// A log line the server writes meanwhile is no name.
		"[2026-11-20 19:11:52:477 INFO] There are 1/10 players online:\r\n[2026-11-20 19:11:52:480 INFO] Running AutoCompaction...\r\nSteve42\r\n": {"Steve42"},
	}
	for output, names := range cases {
		got, ok := ParseBedrockList(output)
		if !ok || got.Online != len(names) || got.Max != 10 || !slices.Equal(got.Names, names) {
			t.Errorf("%q: %+v %v", output, got, ok)
		}
	}
	for _, output := range []string{
		// The names haven't arrived whole, or don't add up to the count.
		"[2026-11-20 19:11:52:477 INFO] There are 2/10 players online:\r\nSteve42, Al",
		"[2026-11-20 19:11:52:477 INFO] There are 2/10 players online:\r\nSteve42\r\n",
		"[2026-11-20 19:11:54:895 ERROR] No targets matched selector\r\n", "",
	} {
		if got, ok := ParseBedrockList(output); ok {
			t.Errorf("%q: %+v", output, got)
		}
	}
}
