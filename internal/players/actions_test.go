package players

import (
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/games"
)

// Kick and ban are sent the ID of the one player with that name in a fresh,
// good reading; a name it doesn't lists, lists twice, or a failed or old
// reading sends nothing.
func TestCommandByID(t *testing.T) {
	moderation := games.ResolveAdapter("palworld", nil).Moderation
	parsed, _ := games.ParsePalworldPlayers("name,playeruid,steamid\nAda,1,76561198000000000\nZed,2,76561198000000001\n" +
		"Zed,3,76561198000000002\nEve,4,76561198000000003 \n")
	reading := &Reading{Players: parsed, CheckedAt: time.Now().UnixMilli()}
	if command, err := Command(moderation, reading, games.Ban, "Ada", Key("76561198000000000")); err != nil || command != "BanPlayer 76561198000000000" {
		t.Fatal(command, err)
	}
	for _, c := range []struct {
		reading *Reading
		action  games.PlayerAction
		name    string
		code    string
	}{
		{reading, games.Kick, "Zed", "PLAYER_NOT_ONLINE"}, {reading, games.Kick, "Mallory", "PLAYER_NOT_ONLINE"},
		{reading, games.Kick, "76561198000000000", "PLAYER_NOT_ONLINE"}, {nil, games.Kick, "Ada", "PLAYER_NOT_ONLINE"},
		{&Reading{Players: parsed, Failed: true, CheckedAt: reading.CheckedAt}, games.Kick, "Ada", "PLAYER_NOT_ONLINE"},
		{&Reading{Players: parsed, CheckedAt: time.Now().Add(-Fresh - time.Second).UnixMilli()}, games.Ban, "Ada", "PLAYER_NOT_ONLINE"},
		{reading, games.Unban, "Ada", "PLAYER_ACTION_UNSUPPORTED"},
	} {
		command, err := Command(moderation, c.reading, c.action, c.name, Key("76561198000000000"))
		if public, ok := apperr.As(err); !ok || public.Code != c.code {
			t.Errorf("%s %s: %q %v", c.action, c.name, command, err)
		}
	}
	// A page offered Ada while she had another ID: the key no longer matches.
	if command, err := Command(moderation, reading, games.Ban, "Ada", Key("76561198000000009")); err == nil {
		t.Error(command)
	}
	if OnlineKey(reading, "Ada") != Key("76561198000000000") || OnlineKey(reading, "Zed") != "" {
		t.Error("online keys")
	}
	if Actionable(moderation, reading, "Zed") || !Actionable(moderation, reading, "Ada") {
		t.Error("actionable players")
	}
	// A Minecraft name counts only from the vanilla list, never a plugin's
	// nickname list.
	minecraft := games.ResolveAdapter("minecraft", nil).Moderation
	vanilla, _ := games.ParseMinecraftList("There are 1 of a max of 5 players online: Ada")
	plugin, _ := games.ParseMinecraftList("There are 1/5 players online:\nAdmins: Ada")
	now := time.Now().UnixMilli()
	if command, err := Command(minecraft, &Reading{Players: vanilla, CheckedAt: now}, games.Kick, "Ada", Key("Ada")); command != "kick Ada" || err != nil {
		t.Error(command, err)
	}
	if command, err := Command(minecraft, &Reading{Players: plugin, CheckedAt: now}, games.Kick, "Ada", Key("Ada")); err == nil {
		t.Error(command)
	}
	if command, err := Command(minecraft, nil, games.Allow, "Ada", ""); command != "whitelist add Ada" || err != nil {
		t.Error(command, err)
	}
	if _, err := Command(nil, reading, games.Kick, "Ada", ""); err == nil {
		t.Error("a game without moderation took a command")
	}
}
