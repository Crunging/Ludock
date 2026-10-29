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
	// Parse reads the command's response, or reports false when it is not one.
	Parse func(response string) (Players, bool)
}

// Name limits keep a hostile or broken response from filling a page.
const (
	maxPlayerNames = 200
	maxNameRunes   = 64
)

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
			name = strings.TrimSpace(name)
			if name == "" || len(players.Names) == maxPlayerNames {
				continue
			}
			if utf8.RuneCountInString(name) > maxNameRunes {
				name = string([]rune(name)[:maxNameRunes])
			}
			players.Names = append(players.Names, name)
		}
	}
	return players, true
}
