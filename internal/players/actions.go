package players

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/console"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/games"
)

var (
	errNotTaken    = apperr.New("PLAYER_ACTION_UNSUPPORTED", 409, "Ludock can’t do that to players on this game")
	errBadName     = apperr.New("PLAYER_NAME_INVALID", 400, "That isn’t a player name this game takes. Check the spelling.")
	errNotOnline   = apperr.New("PLAYER_NOT_ONLINE", 409, "That player isn’t in the latest list of who’s online. Wait for it to refresh, then try again.")
	errListsUnread = apperr.New("PLAYER_LISTS_UNREADABLE", 409, "Ludock couldn’t read the server’s player lists")
)

// Command is the console command for action on the player named name. Kick
// and ban target the one player listed as name in a fresh reading, by the ID
// that reading gave, and only while key, from the page that offered the
// action, is still that ID's key: a confirmation left open never reaches
// another player who took the name since.
func Command(moderation *games.Moderation, reading *Reading, action games.PlayerAction, name, key string) (string, error) {
	if !moderation.Offers(action) {
		return "", errNotTaken
	}
	target := name
	if games.Online(action) {
		id, ok := onlineID(reading, name)
		if !ok || key != Key(id) {
			return "", errNotOnline
		}
		target = id
	}
	command, ok := moderation.Command(action, target)
	if !ok {
		return "", errBadName
	}
	return command, nil
}

// Key identifies a player's ID on a page without showing it.
func Key(id string) string {
	sum := sha256.Sum256([]byte("ludock:player:" + id))
	return hex.EncodeToString(sum[:12])
}

// OnlineKey is the key of the player listed as name, or "".
func OnlineKey(reading *Reading, name string) string {
	if id, ok := onlineID(reading, name); ok {
		return Key(id)
	}
	return ""
}

func onlineID(reading *Reading, name string) (string, bool) {
	if reading == nil || reading.Failed || time.Since(time.UnixMilli(reading.CheckedAt)) > Fresh {
		return "", false
	}
	return reading.ID(name)
}

// Actionable reports whether an online player can be kicked or banned.
func Actionable(moderation *games.Moderation, reading *Reading, name string) bool {
	id, ok := onlineID(reading, name)
	return ok && moderation.Valid(id)
}

// Lists are who may join a server, as its game keeps them.
type Lists struct {
	Allowed, Banned games.ListedPlayers
	// AllowlistOn is the game's own setting, when AllowlistKnown.
	AllowlistOn, AllowlistKnown bool
}

const listLimit = 4 << 20

// ReadLists reads the game's list files from the container. A name that
// holds a secret is counted, not shown.
func ReadLists(ctx context.Context, client *docker.Client, containerID string, moderation *games.Moderation, secrets []string) (*Lists, error) {
	if moderation == nil || moderation.Lists == nil {
		return nil, errNotTaken
	}
	files := moderation.Lists
	read := func(file string) (games.ListedPlayers, error) {
		data, err := client.ReadFile(ctx, containerID, file, listLimit)
		// The server writes a list once it first changes.
		if docker.StatusCode(err) == 404 {
			return games.ListedPlayers{}, nil
		}
		if err != nil {
			return games.ListedPlayers{}, errListsUnread
		}
		list, ok := moderation.ParseMinecraftPlayerList(data)
		if !ok {
			return games.ListedPlayers{}, errListsUnread
		}
		shown := list.Names[:0]
		for _, name := range list.Names {
			if Redacted(name, secrets) {
				list.Others++
			} else {
				shown = append(shown, name)
			}
		}
		list.Names = shown
		return list, nil
	}
	var lists Lists
	var err error
	if lists.Allowed, err = read(files.Allowed); err != nil {
		return nil, err
	}
	if lists.Banned, err = read(files.Banned); err != nil {
		return nil, err
	}
	if data, err := client.ReadFile(ctx, containerID, files.Properties, listLimit); err == nil {
		lists.AllowlistOn, lists.AllowlistKnown = games.ParseProperty(data, files.AllowlistKey)
	}
	return &lists, nil
}

// Redacted reports whether text holds one of secrets.
func Redacted(text string, secrets []string) bool {
	var safe strings.Builder
	redactor := console.NewRedactor(secrets, func(part string) { safe.WriteString(part) })
	redactor.Push(text)
	redactor.End()
	return safe.String() != text
}
