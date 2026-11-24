package games

import (
	"strings"
	"testing"
)

func moderationOf(t *testing.T, gameType string) *Moderation {
	t.Helper()
	adapter := ResolveAdapter(gameType, nil)
	if adapter == nil || adapter.Moderation == nil {
		t.Fatalf("%s has no moderation", gameType)
	}
	return adapter.Moderation
}

// Each game gets exactly its documented command for each action it takes,
// and nothing for the actions it doesn't.
func TestModerationCommands(t *testing.T) {
	for gameType, commands := range map[string]map[PlayerAction][2]string{
		"minecraft": {Kick: {"Ada_2", "kick Ada_2"}, Ban: {"Ada_2", "ban Ada_2"}, Unban: {"Ada_2", "pardon Ada_2"},
			Allow: {"Ada_2", "whitelist add Ada_2"}, Disallow: {"Ada_2", "whitelist remove Ada_2"}},
		"factorio": {Kick: {"Ada.B-2", "/kick Ada.B-2 Kicked by an admin"}, Ban: {"Ada.B-2", "/ban Ada.B-2 Banned by an admin"}},
		// Palworld 1.0 refuses a bare Steam ID.
		"palworld": {Kick: {"76561198000000000", "KickPlayer steam_76561198000000000"},
			Ban: {"steam_76561198000000000", "BanPlayer steam_76561198000000000"}},
		"ark-survival-evolved":  {Kick: {"76561198000000000", "KickPlayer 76561198000000000"}, Ban: {"76561198000000000", "BanPlayer 76561198000000000"}},
		"ark-survival-ascended": {Kick: {"0002a1b2c3d4e5f60718293a4b5c6d7e", "KickPlayer 0002a1b2c3d4e5f60718293a4b5c6d7e"}},
		"7-days-to-die":         {Kick: {"171", "kick 171"}},
		"terraria":              {Kick: {"Zed_2", "kick Zed_2"}, Ban: {"Zed_2", "ban Zed_2"}},
		"minecraft-bedrock":     {Kick: {"Steve42", "kick Steve42"}},
		"vintage-story":         {Kick: {"Ada_2", "/kick Ada_2"}},
	} {
		moderation := moderationOf(t, gameType)
		for _, action := range []PlayerAction{Kick, Ban, Unban, Allow, Disallow} {
			want, offered := commands[action]
			if gameType == "ark-survival-ascended" && action == Ban {
				want, offered = [2]string{"0002A1B2C3D4E5F60718293A4B5C6D7E", "BanPlayer 0002A1B2C3D4E5F60718293A4B5C6D7E"}, true
			}
			got, ok := moderation.Command(action, want[0])
			if moderation.Offers(action) != offered || ok != offered || got != want[1] {
				t.Errorf("%s %s: %q %v", gameType, action, got, ok)
			}
		}
	}
	for _, gameType := range []string{"project-zomboid", "conan-exiles", "cs2", "rust", "valheim"} {
		if adapter := ResolveAdapter(gameType, nil); adapter != nil && adapter.Moderation != nil {
			t.Errorf("%s has moderation", gameType)
		}
	}
	// A label naming another adapter drops the game's moderation with its
	// player list.
	if adapter := ResolveAdapter("factorio", map[string]string{LabelConsole: "telnet-console"}); adapter.Moderation != nil {
		t.Error("factorio over telnet has moderation")
	}
	// Minecraft's commands and list files belong to the Minecraft image, not
	// to every console a label sets to Minecraft RCON.
	if adapter := ResolveAdapter("unknown", map[string]string{LabelConsole: "minecraft-rcon"}); adapter.Moderation != nil {
		t.Error("a labeled console has Minecraft moderation")
	}
	if adapter := ResolveAdapter("minecraft", map[string]string{LabelConsole: "minecraft-rcon"}); adapter.Moderation == nil {
		t.Error("Minecraft lost moderation to its own label")
	}
	// A label naming the game's own console keeps where the game puts its
	// port and password too.
	if labeled, preset := ResolveAdapter("factorio", map[string]string{LabelConsole: "source-rcon"}), ResolveAdapter("factorio", nil); labeled.DefaultPort != preset.DefaultPort || labeled.PasswordFile != preset.PasswordFile || labeled.PasswordFile == "" {
		t.Error("Factorio lost its preset to its own label:", labeled.DefaultPort, labeled.PasswordFile)
	}
}

// A name or ID is accepted only in the game's whole form, so nothing in it
// can end the command, start another, or widen its target.
func TestModerationRefusesHostileTargets(t *testing.T) {
	hostile := []string{"", " ", "Ada ", " Ada", "Ada op", "Ada\nop Eve", "Ada\r\nstop", "Ada\x00", "Ada\t", "Ada;stop",
		"Ada&&stop", "Ada|stop", `"Ada"`, "'Ada'", "Ada\"", "@a", "@a[name=Ada]", "@p", "@s", "@e", "@r", "Ada/op",
		"Ada\\n", "Åda", "Ada\u2028", "Ada\u00a0", "Ada\u200b", "$(id)", "`id`", "Ada%0Aop", "Ada,Eve", "#Ada", "Ada]",
		"76561198000000000 ", "76561198000000000\n0", "76561198000000000\nBanPlayer 1"}
	for _, gameType := range []string{"minecraft", "factorio", "palworld", "ark-survival-evolved", "7-days-to-die"} {
		moderation := moderationOf(t, gameType)
		for _, target := range hostile {
			for _, action := range []PlayerAction{Kick, Ban, Unban, Allow, Disallow} {
				if command, ok := moderation.Command(action, target); ok {
					t.Errorf("%s %s %q: sent %q", gameType, action, target, command)
				}
			}
		}
	}
	// IDs are only the game's own lengths and digits.
	for _, gameType := range []string{"palworld", "ark-survival-evolved", "7-days-to-die"} {
		for _, target := range []string{"Ada", "-1", "1e5", "0x1F", strings.Repeat("9", 18), strings.Repeat("a", 33), "steam_1"} {
			if command, ok := moderationOf(t, gameType).Command(Kick, target); ok {
				t.Errorf("%s %q: sent %q", gameType, target, command)
			}
		}
	}
	if moderationOf(t, "minecraft").Valid(strings.Repeat("A", 17)) {
		t.Error("Minecraft takes 17 letters")
	}
	// Every name a game takes leaves one command of printable ASCII, with the
	// target as its own word.
	for _, gameType := range []string{"minecraft", "factorio"} {
		moderation := moderationOf(t, gameType)
		for _, name := range []string{"A", "Ada_2", strings.Repeat("z", 16), "Ada.B-2"} {
			command, ok := moderation.Command(Kick, name)
			if !ok {
				continue
			}
			if strings.ContainsAny(command, "\r\n\x00\"@;") || !strings.Contains(" "+command+" ", " "+name+" ") {
				t.Errorf("%s %q: %q", gameType, name, command)
			}
		}
	}
	// A name the player list cut short is never a valid name.
	for _, gameType := range []string{"minecraft", "factorio"} {
		if moderationOf(t, gameType).Valid(strings.Repeat("a", MaxNameRunes)) {
			t.Errorf("%s takes a cut name", gameType)
		}
	}
}

// Minecraft's replies say whether the action changed anything; any other
// reply is unknown, not success.
func TestMinecraftOutcome(t *testing.T) {
	moderation := moderationOf(t, "minecraft")
	for reply, want := range map[[2]string]Outcome{
		{"kick", "Kicked Ada: Kicked by an operator"}:                 Done,
		{"kick", "No player was found"}:                               NoPlayer,
		{"ban", "Banned Ada: Banned by an operator"}:                  Done,
		{"ban", "Nothing changed. The player is already banned"}:      Unchanged,
		{"unban", "Unbanned Ada"}:                                     Done,
		{"unban", "Nothing changed. The player isn't banned"}:         Unchanged,
		{"allow", "Added Ada to the whitelist"}:                       Done,
		{"allow", "Player is already whitelisted"}:                    Unchanged,
		{"allow", "That player does not exist"}:                       NoPlayer,
		{"disallow", "Removed Ada from the whitelist"}:                Done,
		{"disallow", "Player is not whitelisted"}:                     Unchanged,
		{"kick", "Banned Ada: Banned by an operator"}:                 OutcomeUnknown,
		{"allow", "Added Ada"}:                                        OutcomeUnknown,
		{"ban", "Unknown or incomplete command, see below for error"}: OutcomeUnknown,
		{"ban", ""}: OutcomeUnknown,
	} {
		if got := moderation.Outcome(PlayerAction(reply[0]), reply[1]); got != want {
			t.Errorf("%v: %v", reply, got)
		}
	}
}

// Palworld 1.0.5's replies, captured from thijsvanloef/palworld-server-docker
// with nobody online; a kick that went through was never seen.
func TestPalworldOutcome(t *testing.T) {
	moderation := moderationOf(t, "palworld")
	for reply, want := range map[[2]string]Outcome{
		{"kick", "Failed to Kick: steam_76561198000000000\n"}: NoPlayer,
		{"ban", "Baned: steam_76561198000000000\n"}:           Done,
		{"ban", "Failed to Ban: steam_76561198000000000\n"}:   NoPlayer,
		{"kick", "Baned: steam_76561198000000000\n"}:          OutcomeUnknown,
	} {
		if got := moderation.Outcome(PlayerAction(reply[0]), reply[1]); got != want {
			t.Errorf("%v: %v", reply, got)
		}
	}
}

// Factorio 2.1's replies, captured from factoriotools/factorio. A kick that
// went through was never seen, so it stays unknown.
func TestFactorioOutcome(t *testing.T) {
	moderation := moderationOf(t, "factorio")
	for reply, want := range map[[2]string]Outcome{
		{"kick", "Player Nobody doesn't exist.\n"}: NoPlayer,
		{"ban", "Added Nobody to the banlist.\n"}:  Done,
		{"ban", "Nobody is already banned.\n"}:     Unchanged,
		{"kick", "Kicked Ada"}:                     OutcomeUnknown,
		{"kick", "Added Ada to the banlist.\n"}:    OutcomeUnknown,
		{"ban", "Player Ada doesn't exist.\n"}:     OutcomeUnknown,
	} {
		if got := moderation.Outcome(PlayerAction(reply[0]), reply[1]); got != want {
			t.Errorf("%v: %v", reply, got)
		}
	}
}

// The allowlist and ban list files keep the names in the game's form; other
// entries, such as a Bedrock player's, are counted.
func TestParseMinecraftPlayerList(t *testing.T) {
	moderation := moderationOf(t, "minecraft")
	list, ok := moderation.ParseMinecraftPlayerList([]byte(`[{"uuid":"1","name":"Ada"},{"name":".BedrockAda"},
		{"uuid":"2"},{"name":"Zed","created":"2026-11-19 10:00:00 +0000","source":"Server","expires":"forever","reason":"x"},
		{"name":"Eve\nop Mallory"}]`))
	if !ok || strings.Join(list.Names, "|") != "Ada|Zed" || list.Others != 3 {
		t.Fatal(list, ok)
	}
	if list, ok := moderation.ParseMinecraftPlayerList([]byte("[]")); !ok || len(list.Names) != 0 || list.Others != 0 {
		t.Fatal(list, ok)
	}
	for _, broken := range []string{"", "{}", `{"name":"Ada"}`, "[", `["Ada"]`} {
		if _, ok := moderation.ParseMinecraftPlayerList([]byte(broken)); ok {
			t.Errorf("%q parsed", broken)
		}
	}
	for text, want := range map[string][2]bool{
		"motd=Hi\nwhite-list=true\n":        {true, true},
		"white-list = false\r\n":            {false, true},
		"#white-list=true\nwhite-list=TRUE": {true, true},
		"motd=Hi\n":                         {false, false},
		"white-list=maybe\n":                {false, true},
		"white-list: true\n":                {true, true},
		"white-list=true \n":                {false, true},
		"enforce-white-list=true\n":         {false, false},
	} {
		if on, set := ParseProperty([]byte(text), "white-list"); on != want[0] || set != want[1] {
			t.Errorf("%q: %v %v", text, on, set)
		}
	}
}

// Each listed player keeps the ID the game's commands take beside the name;
// a name listed twice has no single ID.
func TestPlayerIDs(t *testing.T) {
	ark, _ := ParseARKPlayers("0. Ada, 76561198000000000\n1. Zed, 0002a1b2c3d4e5f60718293a4b5c6d7e\n2. Ada, 76561198000000001\n")
	palworld, _ := ParsePalworldPlayers("name,playeruid,steamid\n,000,00000000000000000\nAda,1,steam_76561198000000000\n")
	seven, _ := Parse7DaysPlayers("0. id=171, Ada, pos=(1, 2, 3), rot=(4, 5, 6), remote=True, health=1, deaths=0, zombies=0, " +
		"players=0, score=0, level=1, pltfmid=Steam_1, ping=0\nTotal of 1 in the game\n")
	for _, c := range []struct {
		players  Players
		name, id string
		ok       bool
	}{
		{ark, "Zed", "0002a1b2c3d4e5f60718293a4b5c6d7e", true}, {ark, "Ada", "", false}, {ark, "Eve", "", false},
		{palworld, "Ada", "steam_76561198000000000", true}, {seven, "Ada", "171", true},
	} {
		if id, ok := c.players.ID(c.name); id != c.id || ok != c.ok {
			t.Errorf("%+v %s: %q %v", c.players, c.name, id, ok)
		}
	}
	// Vanilla Minecraft and Factorio list account names; a plugin's list
	// shows nicknames and gives none.
	minecraft, _ := ParseMinecraftList("There are 2 of a max of 5 players online: Ada, Zed")
	factorio, _ := ParseFactorioPlayers("Online players (1):\n  Ada (online)\n")
	for _, players := range []Players{minecraft, factorio} {
		if id, ok := players.ID("Ada"); id != "Ada" || !ok {
			t.Errorf("%+v: %q %v", players, id, ok)
		}
	}
	for _, list := range []string{"There are 1/5 players online:Ada", "There are 1 of a max of 5 players online: §eAda",
		"There are 1 out of maximum 5 players online.\nAdmins: Ada", "There are 1 of a max of 5 players online:\nAdmins: Ada"} {
		if players, _ := ParseMinecraftList(list); len(players.Names) != 1 || players.IDs[0] != "" {
			t.Errorf("%q: %+v", list, players)
		}
	}
}

// Minecraft's "list uuids" names accounts; only its exact vanilla reply
// confirms one.
func TestParseMinecraftAccounts(t *testing.T) {
	accounts := moderationOf(t, "minecraft").Accounts
	got, ok := accounts.Parse("There are 2 of a max of 20 players online: Ada (069a79f4-44e9-4726-a5be-fca90e38aaf5), " +
		"Zed_2 (00000000-0000-4000-8000-000000000002)\n")
	if !ok || strings.Join(got.IDs, ",") != "Ada,Zed_2" {
		t.Fatal(got, ok)
	}
	if got, ok := accounts.Parse("There are 0 of a max of 20 players online: "); !ok || len(got.IDs) != 0 {
		t.Fatal(got, ok)
	}
	for _, reply := range []string{"", "There are 1 of a max of 20 players online: Ada",
		"There are 2 of a max of 20 players online: Ada (069a79f4-44e9-4726-a5be-fca90e38aaf5)",
		"There are 1 of a max of 20 players online: [VIP] Ada (069a79f4-44e9-4726-a5be-fca90e38aaf5)",
		"There are 1 out of maximum 20 players online.\nAdmins: Ada", "Group uuids not found"} {
		if got, ok := accounts.Parse(reply); ok {
			t.Errorf("%q: %+v", reply, got)
		}
	}
}
