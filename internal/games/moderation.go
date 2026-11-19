package games

import (
	"encoding/json"
	"regexp"
	"strings"
)

// PlayerAction is something a game's console can do to one player.
type PlayerAction string

const (
	Kick     PlayerAction = "kick"
	Ban      PlayerAction = "ban"
	Unban    PlayerAction = "unban"
	Allow    PlayerAction = "allow"
	Disallow PlayerAction = "disallow"
)

// Moderation is how a game's console acts on players. A player's name or ID
// is hostile text from the game or a form: a command is built only from a
// target that matches the whole of the game's own form, which never allows
// whitespace, quotes, separators, or selectors.
type Moderation struct {
	// ByID: commands take the ID the player list gives, not the name.
	ByID bool
	// target is the form of a name or ID the game takes. Every form stays
	// shorter than MaxNameRunes, so a name the list cut short never matches.
	target *regexp.Regexp
	// commands holds each action's text before and after the target.
	commands map[PlayerAction][2]string
	// Lists are the game's files of allowed and banned players, or nil.
	Lists *PlayerLists
	// outcome reads a reply, or is nil when Ludock doesn't know the game's.
	outcome func(action PlayerAction, reply string) Outcome
}

// Offers reports whether the game takes the action.
func (m *Moderation) Offers(action PlayerAction) bool {
	if m == nil {
		return false
	}
	_, ok := m.commands[action]
	return ok
}

// Valid reports whether target is a name or ID the game's commands take.
func (m *Moderation) Valid(target string) bool {
	return m != nil && m.target.MatchString(target)
}

// Command is the one console command for action on target, or false when
// the game doesn't take the action or target isn't in the game's form.
func (m *Moderation) Command(action PlayerAction, target string) (string, bool) {
	parts, ok := m.commands[action]
	if !ok || !m.Valid(target) {
		return "", false
	}
	command := parts[0] + target + parts[1]
	// The forms already exclude these; a mistake in one must not send more.
	if strings.ContainsFunc(command, func(r rune) bool { return r < 0x20 || r > 0x7e || r == ';' || r == '"' || r == '@' }) {
		return "", false
	}
	return command, true
}

// Outcome is what a game's reply says an action did.
type Outcome int8

const (
	// OutcomeUnknown: Ludock doesn't recognize the reply.
	OutcomeUnknown Outcome = iota
	Done
	// Unchanged: the player already was as asked, such as already banned.
	Unchanged
	// NoPlayer: the game knows no such player, or they aren't online.
	NoPlayer
)

// Outcome reads the game's reply to action.
func (m *Moderation) Outcome(action PlayerAction, reply string) Outcome {
	if m == nil || m.outcome == nil {
		return OutcomeUnknown
	}
	return m.outcome(action, strings.TrimSpace(reply))
}

// PlayerLists are files in the container where the game keeps who may join.
type PlayerLists struct {
	Allowed, Banned string
	// Properties holds whether the allowlist is on, under AllowlistKey.
	Properties, AllowlistKey string
}

// ListedPlayers is one of a game's player lists.
type ListedPlayers struct {
	// Names are the entries in the game's form, so commands can take them.
	Names []string
	// Others counts entries in another form, such as a Bedrock player's.
	Others int
}

// maxListed bounds a list a page shows; the rest are counted as others.
const maxListed = 500

// ParseMinecraftPlayerList reads whitelist.json or banned-players.json, an
// array of objects that each have a "name".
func (m *Moderation) ParseMinecraftPlayerList(data []byte) (ListedPlayers, bool) {
	var entries []struct {
		Name *string `json:"name"`
	}
	if json.Unmarshal(data, &entries) != nil {
		return ListedPlayers{}, false
	}
	var list ListedPlayers
	for _, entry := range entries {
		if entry.Name == nil || !m.Valid(*entry.Name) || len(list.Names) == maxListed {
			list.Others++
			continue
		}
		list.Names = append(list.Names, *entry.Name)
	}
	return list, true
}

// ParseProperty reads a true or false key from a Java properties file, or
// reports false when the file doesn't set it.
func ParseProperty(data []byte, key string) (on, set bool) {
	for _, line := range strings.Split(string(data), "\n") {
		name, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || strings.TrimSpace(name) != key {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true":
			on, set = true, true
		case "false":
			on, set = false, true
		default:
			return false, false
		}
	}
	return on, set
}

// Minecraft Java names are Mojang account names. Commands take a game
// profile argument, where a selector such as @a would also match.
var minecraftModeration = &Moderation{
	target: regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`),
	commands: map[PlayerAction][2]string{Kick: {"kick ", ""}, Ban: {"ban ", ""}, Unban: {"pardon ", ""},
		Allow: {"whitelist add ", ""}, Disallow: {"whitelist remove ", ""}},
	// The server keeps both lists and its settings in its working directory,
	// /data in the itzg image.
	Lists: &PlayerLists{Allowed: "/data/whitelist.json", Banned: "/data/banned-players.json",
		Properties: "/data/server.properties", AllowlistKey: "white-list"},
	outcome: minecraftOutcome,
}

// minecraftOutcome reads the vanilla server's English replies.
func minecraftOutcome(action PlayerAction, reply string) Outcome {
	switch {
	case reply == "No player was found" || reply == "That player does not exist":
		return NoPlayer
	case action == Kick && strings.HasPrefix(reply, "Kicked "),
		action == Ban && strings.HasPrefix(reply, "Banned "),
		action == Unban && strings.HasPrefix(reply, "Unbanned "),
		action == Allow && strings.HasPrefix(reply, "Added ") && strings.HasSuffix(reply, " to the whitelist"),
		action == Disallow && strings.HasPrefix(reply, "Removed ") && strings.HasSuffix(reply, " from the whitelist"):
		return Done
	case action == Ban && reply == "Nothing changed. The player is already banned",
		action == Unban && reply == "Nothing changed. The player isn't banned",
		action == Allow && reply == "Player is already whitelisted",
		action == Disallow && reply == "Player is not whitelisted":
		return Unchanged
	}
	return OutcomeUnknown
}

// Factorio names are letters, digits, and "._-"; the command's reason is
// required.
var factorioModeration = &Moderation{
	target:   regexp.MustCompile(`^[A-Za-z0-9._-]{1,60}$`),
	commands: map[PlayerAction][2]string{Kick: {"/kick ", " Kicked by an admin"}, Ban: {"/ban ", " Banned by an admin"}},
}

// Palworld takes the Steam ID its ShowPlayers gives.
var palworldModeration = &Moderation{ByID: true,
	target:   regexp.MustCompile(`^(?:steam_)?[0-9]{17}$`),
	commands: map[PlayerAction][2]string{Kick: {"KickPlayer ", ""}, Ban: {"BanPlayer ", ""}},
}

// ARK takes a Survival Evolved Steam ID or a Survival Ascended EOS ID.
var arkModeration = &Moderation{ByID: true,
	target:   regexp.MustCompile(`^(?:[0-9]{17}|[0-9A-Fa-f]{32})$`),
	commands: map[PlayerAction][2]string{Kick: {"KickPlayer ", ""}, Ban: {"BanPlayer ", ""}},
}

// 7 Days to Die kicks by the entity ID listplayers gives.
var sevenDaysModeration = &Moderation{ByID: true,
	target:   regexp.MustCompile(`^[0-9]{1,10}$`),
	commands: map[PlayerAction][2]string{Kick: {"kick ", ""}},
}

// Pattern is the name form for a browser's pattern attribute.
func (m *Moderation) Pattern() string {
	return strings.TrimSuffix(strings.TrimPrefix(m.target.String(), "^"), "$")
}
