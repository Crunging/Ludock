package players

import (
	"context"
	"strings"

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

// Command is the console command for action on the player named name. A
// game that takes IDs gets the ID of the one player with that name in the
// latest reading, which is never shown.
func Command(moderation *games.Moderation, reading *Reading, action games.PlayerAction, name string) (string, error) {
	if !moderation.Offers(action) {
		return "", errNotTaken
	}
	target := name
	if moderation.ByID {
		id, ok := "", false
		if reading != nil && !reading.Failed {
			id, ok = reading.ID(name)
		}
		if !ok {
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

// Actionable reports whether a listed player can be acted on.
func Actionable(moderation *games.Moderation, reading *Reading, name string) bool {
	if moderation == nil || reading == nil {
		return false
	}
	if moderation.ByID {
		id, ok := reading.ID(name)
		return ok && moderation.Valid(id)
	}
	return moderation.Valid(name)
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
			if redacted(name, secrets) {
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

func redacted(text string, secrets []string) bool {
	var safe strings.Builder
	redactor := console.NewRedactor(secrets, func(part string) { safe.WriteString(part) })
	redactor.Push(text)
	redactor.End()
	return safe.String() != text
}
