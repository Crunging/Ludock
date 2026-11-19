package players

import (
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/games"
)

// A game that takes IDs is sent the ID of the one player with that name in
// the latest good reading; a name it doesn't list, lists twice, or a failed
// reading sends nothing.
func TestCommandByID(t *testing.T) {
	moderation := games.ResolveAdapter("palworld", nil).Moderation
	parsed, _ := games.ParsePalworldPlayers("name,playeruid,steamid\nAda,1,76561198000000000\nZed,2,76561198000000001\n" +
		"Zed,3,76561198000000002\nEve,4,76561198000000003 \n")
	reading := &Reading{Players: parsed}
	if command, err := Command(moderation, reading, games.Ban, "Ada"); err != nil || command != "BanPlayer 76561198000000000" {
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
		{&Reading{Players: parsed, Failed: true}, games.Kick, "Ada", "PLAYER_NOT_ONLINE"},
		{reading, games.Unban, "Ada", "PLAYER_ACTION_UNSUPPORTED"},
	} {
		command, err := Command(moderation, c.reading, c.action, c.name)
		if public, ok := apperr.As(err); !ok || public.Code != c.code {
			t.Errorf("%s %s: %q %v", c.action, c.name, command, err)
		}
	}
	if Actionable(moderation, reading, "Zed") || !Actionable(moderation, reading, "Ada") {
		t.Error("actionable players")
	}
	if _, err := Command(nil, reading, games.Kick, "Ada"); err == nil {
		t.Error("a game without moderation took a command")
	}
}
