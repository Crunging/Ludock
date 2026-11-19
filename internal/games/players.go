package games

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Players struct {
	Online int
	Max    int // 0 when the game doesn't say
	// Names may be fewer than Online when the game hides or truncates them.
	Names []string
}

// PlayerQuery is a console command that lists who is online, and its parser.
type PlayerQuery struct {
	Command string
	// Parse reads the command's response, or reports false when it is not
	// one. A parser never guesses: a reply it doesn't recognize is false,
	// not zero players.
	Parse func(response string) (Players, bool)
}

// Name limits keep a hostile or broken response from filling a page.
const (
	maxPlayerNames = 200
	maxNameRunes   = 64
)

func (p *Players) add(name string) {
	name = strings.TrimSpace(name)
	if name == "" || len(p.Names) == maxPlayerNames {
		return
	}
	if utf8.RuneCountInString(name) > maxNameRunes {
		name = string([]rune(name)[:maxNameRunes])
	}
	p.Names = append(p.Names, name)
}

// replyLines splits a reply into trimmed, non-blank lines, or reports false
// when it holds a NUL byte. Some RCON servers pad replies with trailing NULs.
func replyLines(response string) ([]string, bool) {
	response = strings.TrimRight(response, "\x00")
	if strings.Contains(response, "\x00") {
		return nil, false
	}
	var lines []string
	for _, line := range strings.Split(response, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, true
}

var (
	// Vanilla 1.13+ says "There are 2 of a max of 20 players online: A, B";
	// earlier versions and Bukkit say "2/20", Essentials "2 out of maximum 20".
	minecraftList = regexp.MustCompile(`(?is)^\s*there are\s+(\d+)\s*(?:of a max(?:imum)? of|/|out of maximum)\s*(\d+)\s+players?\s+online\s*[.:]?(.*)$`)
	// Section-sign formatting codes, such as §e, that plugins add.
	minecraftFormatting = regexp.MustCompile(`§.?`)
)

// ParseMinecraftList reads the response to Minecraft's list command.
// Plugins that group players print "Group: A, B" lines; the group is dropped.
func ParseMinecraftList(response string) (Players, bool) {
	match := minecraftList.FindStringSubmatch(minecraftFormatting.ReplaceAllString(response, ""))
	if match == nil {
		return Players{}, false
	}
	online, err := strconv.Atoi(match[1])
	if err != nil {
		return Players{}, false
	}
	capacity, err := strconv.Atoi(match[2])
	if err != nil {
		return Players{}, false
	}
	players := Players{Online: online, Max: capacity}
	if online == 0 {
		return players, true
	}
	for _, line := range strings.Split(match[3], "\n") {
		if group := strings.LastIndex(line, ": "); group >= 0 {
			line = line[group+2:]
		}
		for _, name := range strings.Split(line, ",") {
			players.add(name)
		}
	}
	return players, true
}

var (
	factorioHeader = regexp.MustCompile(`^Online players \((\d+)\):$`)
	// Player lines are indented; unindented "[JOIN]" or "[CHAT]" log lines
	// can arrive in the same reply.
	factorioPlayer = regexp.MustCompile(`^[ \t]+(\S.*?)[ \t]+\(online\)[ \t\r]*$`)
)

// ParseFactorioPlayers reads Factorio's "/players online":
//
//	Online players (2):
//	  Ada (online)
//	  Zed (online)
func ParseFactorioPlayers(response string) (Players, bool) {
	response = strings.TrimRight(response, "\x00")
	if strings.Contains(response, "\x00") {
		return Players{}, false
	}
	header := -1
	var players Players
	seen := map[string]bool{}
	for _, line := range strings.Split(response, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case factorioHeader.MatchString(trimmed):
			if header >= 0 {
				return Players{}, false
			}
			header, _ = strconv.Atoi(factorioHeader.FindStringSubmatch(trimmed)[1])
		case factorioPlayer.MatchString(line) && header >= 0:
			name := factorioPlayer.FindStringSubmatch(line)[1]
			if seen[name] {
				return Players{}, false
			}
			seen[name] = true
			players.Online++
			players.add(name)
		case strings.HasPrefix(line, "["):
		default:
			return Players{}, false
		}
	}
	if header < 0 || players.Online != header {
		return Players{}, false
	}
	return players, true
}

// palworldIDs are a row's player UID, all zeros while the player is still
// joining, and platform ID.
var palworldIDs = regexp.MustCompile(`^\d+,[A-Za-z0-9_-]*$`)

// ParsePalworldPlayers reads Palworld's ShowPlayers, a CSV table whose
// header alone means nobody is online:
//
//	name,playeruid,steamid
//	Ada,1234567890,76561198000000000
func ParsePalworldPlayers(response string) (Players, bool) {
	lines, ok := replyLines(response)
	if !ok || len(lines) == 0 || !strings.EqualFold(lines[0], "name,playeruid,steamid") {
		return Players{}, false
	}
	var players Players
	for _, line := range lines[1:] {
		// Names may contain commas; the two IDs never do.
		fields := strings.Split(line, ",")
		if len(fields) < 3 || !palworldIDs.MatchString(strings.Join(fields[len(fields)-2:], ",")) {
			return Players{}, false
		}
		players.Online++
		players.add(strings.Join(fields[:len(fields)-2], ","))
	}
	return players, true
}

// arkPlayer ends with a 17-digit Steam ID or a 32-digit hexadecimal Epic or
// EOS ID.
var arkPlayer = regexp.MustCompile(`^(\d+)\. (.*), (\d{17}|[0-9A-Fa-f]{32})$`)

// ParseARKPlayers reads ListPlayers from ARK, one numbered row per player
// such as "0. Ada, 76561198000000000", with Steam IDs on Survival Evolved and
// EOS IDs on Survival Ascended.
func ParseARKPlayers(response string) (Players, bool) {
	lines, ok := replyLines(response)
	if !ok {
		return Players{}, false
	}
	if len(lines) == 1 && lines[0] == "No Players Connected" {
		return Players{}, true
	}
	var players Players
	for _, line := range lines {
		match := arkPlayer.FindStringSubmatch(line)
		// Rows are numbered from 0; anything else is another command's reply.
		if match == nil || match[1] != strconv.Itoa(players.Online) {
			return Players{}, false
		}
		players.Online++
		players.add(match[2])
	}
	return players, players.Online > 0
}

var zomboidHeader = regexp.MustCompile(`^Players connected \((\d+)\):$`)

// ParseZomboidPlayers reads Project Zomboid's players command:
//
//	Players connected (2):
//	-Ada
//	-Zed
func ParseZomboidPlayers(response string) (Players, bool) {
	lines, ok := replyLines(response)
	if !ok || len(lines) == 0 || !zomboidHeader.MatchString(lines[0]) {
		return Players{}, false
	}
	count, _ := strconv.Atoi(zomboidHeader.FindStringSubmatch(lines[0])[1])
	var players Players
	for _, line := range lines[1:] {
		name, ok := strings.CutPrefix(line, "-")
		if !ok || strings.TrimSpace(name) == "" {
			return Players{}, false
		}
		players.Online++
		players.add(name)
	}
	return players, players.Online == count
}

var conanHeader = []string{"idx", "char name", "player name", "user id", "platform id", "platform name"}

// ParseConanPlayers reads Conan Exiles' ListPlayers, a table whose header
// alone means nobody is online:
//
//	Idx | Char name | Player name | User ID | Platform ID | Platform Name
//	  0 | Ada       | ada_1       | 1A2B... | 7656119... | Steam
//
// Names are characters' names, or the player's while a character has none.
func ParseConanPlayers(response string) (Players, bool) {
	lines, ok := replyLines(response)
	if !ok || len(lines) == 0 {
		return Players{}, false
	}
	header := strings.Split(lines[0], "|")
	if len(header) != len(conanHeader) {
		return Players{}, false
	}
	for index, column := range header {
		if strings.ToLower(strings.TrimSpace(column)) != conanHeader[index] {
			return Players{}, false
		}
	}
	var players Players
	seen := map[int]bool{}
	for _, line := range lines[1:] {
		row := strings.Split(line, "|")
		if len(row) != len(header) {
			return Players{}, false
		}
		index, err := strconv.Atoi(strings.TrimSpace(row[0]))
		if err != nil || index < 0 || seen[index] {
			return Players{}, false
		}
		seen[index] = true
		players.Online++
		name := strings.TrimSpace(row[1])
		if name == "" {
			name = row[2]
		}
		players.add(name)
	}
	return players, true
}

var (
	sevenDaysPlayer = regexp.MustCompile(`^\d+\. id=\d+, (.*?), pos=\(.*, ping=\d+$`)
	sevenDaysTotal  = regexp.MustCompile(`^Total of (\d+) in the game$`)
	// Log lines the Telnet console streams, such as
	// "2026-11-19T10:00:00 512.345 INF Player connected".
	sevenDaysLog = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d \d+\.\d+ [A-Z]{3} `)
)

// Parse7DaysPlayers reads 7 Days to Die's listplayers: rows such as
// "0. id=171, Ada, pos=(741.9, 45.1, 951.5), ..., ping=0", then
// "Total of 1 in the game". Server log lines in between are skipped; the rows
// must add up to the total.
func Parse7DaysPlayers(response string) (Players, bool) {
	lines, ok := replyLines(response)
	if !ok {
		return Players{}, false
	}
	total := -1
	var players Players
	for _, line := range lines {
		switch match := sevenDaysPlayer.FindStringSubmatch(line); {
		case sevenDaysLog.MatchString(line):
		case match != nil && total < 0:
			players.Online++
			players.add(match[1])
		case sevenDaysTotal.MatchString(line) && total < 0:
			total, _ = strconv.Atoi(sevenDaysTotal.FindStringSubmatch(line)[1])
		default:
			return Players{}, false
		}
	}
	return players, total >= 0 && players.Online == total
}
