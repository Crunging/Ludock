package games

import (
	"strings"
	"testing"
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
		"There are 0 of a max of 20 players online: ":           {0, 20, nil},
		"There are 2 of a max of 20 players online: Mara, Theo": {2, 20, []string{"Mara", "Theo"}},
		"There are 1/10 players online:Steve":                   {1, 10, []string{"Steve"}},
		"There are 2/10 players online:\nAlex, Steve\n":         {2, 10, []string{"Alex", "Steve"}},
		"§6There are §c3§6 out of maximum §c50§6 players online.\n§6Admins§r: §fMara\n§6Default§r: Theo, Juno": {3, 50,
			[]string{"Mara", "Theo", "Juno"}},
		// Some servers hide names; the count still stands.
		"There are 4 of a max of 8 players online:": {4, 8, nil},
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
	if len(long.Names) != maxPlayerNames || len([]rune(long.Names[0])) != maxNameRunes {
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
		"conan-exiles": "ListPlayers", "7-days-to-die": "listplayers"} {
		if adapter := ResolveAdapter(game, nil); adapter == nil || adapter.Players == nil || adapter.Players.Command != command {
			t.Errorf("%s: %+v", game, adapter)
		}
	}
	if adapter := ResolveAdapter("palworld", map[string]string{LabelConsole: "source-rcon"}); adapter.Players == nil {
		t.Error("the game's own adapter lost its player list")
	}
	for game, labels := range map[string]map[string]string{"terraria": nil, "cs2": nil, "v-rising": nil, "rust": nil,
		"palworld": {LabelConsole: "telnet-console"}, "unknown": {LabelConsole: "source-rcon"}} {
		if adapter := ResolveAdapter(game, labels); adapter != nil && adapter.Players != nil {
			t.Errorf("%s %v lists players", game, labels)
		}
	}
}
