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
	// IDs are the ID each name's row gave, or "", for games whose commands
	// take one. They are never shown.
	IDs []string
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
	MaxNameRunes   = 64
)

func (p *Players) add(name string) { p.addWithID(name, "") }

func (p *Players) addWithID(name, id string) {
	name = strings.TrimSpace(name)
	if name == "" || len(p.Names) == maxPlayerNames {
		return
	}
	if utf8.RuneCountInString(name) > MaxNameRunes {
		name = string([]rune(name)[:MaxNameRunes])
	}
	p.Names = append(p.Names, name)
	p.IDs = append(p.IDs, id)
}

// ID is the ID of the one player listed as name, or false when none or
// several are.
func (p *Players) ID(name string) (string, bool) {
	id, found := "", false
	for index, listed := range p.Names {
		if listed != name {
			continue
		}
		if found || index >= len(p.IDs) || p.IDs[index] == "" {
			return "", false
		}
		id, found = p.IDs[index], true
	}
	return id, found
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
	// Only the vanilla reply, unformatted on one line, lists account names;
	// plugins' lists show nicknames and prefixes, so they give no IDs.
	vanilla := strings.HasPrefix(response, "There are ") && strings.Contains(response, " of a max of ") &&
		!strings.Contains(response, "§") && !strings.ContainsAny(strings.TrimRight(match[3], " \r\n"), "\n:")
	for _, line := range strings.Split(match[3], "\n") {
		if group := strings.LastIndex(line, ": "); group >= 0 {
			line = line[group+2:]
		}
		for _, name := range strings.Split(line, ",") {
			if vanilla {
				players.addWithID(name, strings.TrimSpace(name))
			} else {
				players.add(name)
			}
		}
	}
	return players, true
}

var (
	factorioHeader = regexp.MustCompile(`^Online players \((\d+)\):$`)
	factorioPlayer = regexp.MustCompile(`^[ \t]+(\S.*?)[ \t]+\(online\)[ \t\r]*$`)
	// Player lines are indented; log lines such as "[JOIN] Ada joined the
	// game" can arrive unindented in the same reply.
	factorioLog = regexp.MustCompile(`^\[(?:JOIN|LEAVE|CHAT|SHOUT|WHISPER|COMMAND|KICK|BAN|UNBANNED|PROMOTE|DEMOTE)\] `)
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
			name := strings.TrimSpace(factorioPlayer.FindStringSubmatch(line)[1])
			if name == "" || seen[name] {
				return Players{}, false
			}
			seen[name] = true
			players.Online++
			// Factorio lists account names, which its commands take.
			players.addWithID(name, name)
		case factorioLog.MatchString(line):
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
		players.addWithID(strings.Join(fields[:len(fields)-2], ","), fields[len(fields)-1])
	}
	return players, true
}

// arkPlayer ends with a 17-digit Steam ID, a 19-digit Survival Evolved Epic
// ID, or a 32-digit hexadecimal EOS ID.
var arkPlayer = regexp.MustCompile(`^(\d+)\. (.*), (\d{17}|\d{19}|[0-9A-Fa-f]{32})$`)

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
		players.addWithID(match[2], match[3])
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
		// Every player has a Funcom user ID and a platform ID.
		if err != nil || index < 0 || seen[index] || strings.TrimSpace(row[3]) == "" || strings.TrimSpace(row[4]) == "" {
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

const sevenDaysVector = `-?\d+(?:\.\d+)?, -?\d+(?:\.\d+)?, -?\d+(?:\.\d+)?`

var (
	sevenDaysPlayer = regexp.MustCompile(`^\d+\. id=(\d+), (.*?), pos=\(` + sevenDaysVector + `\), rot=\(` + sevenDaysVector +
		`\), remote=(?:True|False), health=\d+, deaths=\d+, zombies=\d+, players=\d+, score=-?\d+, level=\d+, .*, ping=-?\d+$`)
	sevenDaysTotal = regexp.MustCompile(`^Total of (\d+) in the game$`)
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
			players.addWithID(match[2], match[1])
		case sevenDaysTotal.MatchString(line) && total < 0:
			total, _ = strconv.Atoi(sevenDaysTotal.FindStringSubmatch(line)[1])
		default:
			return Players{}, false
		}
	}
	return players, total >= 0 && players.Online == total
}

// logLines splits a game's log output into its complete lines, without their
// line ends, or reports false when it holds a NUL byte.
func logLines(output string) ([]string, bool) {
	if strings.Contains(output, "\x00") {
		return nil, false
	}
	lines := strings.Split(output, "\n")
	// The last line is still being written.
	lines = lines[:len(lines)-1]
	for index, line := range lines {
		lines[index] = strings.TrimRight(terminalCodes.ReplaceAllString(line, ""), "\r ")
	}
	return lines, true
}

// terminalCodes are escape sequences a game under a TTY prints, such as
// Terraria's window title and the cursor query before its ": " prompt.
var terminalCodes = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[=>]`)

var (
	terrariaSummary = regexp.MustCompile(`^(?:: )?(?:(No) players connected\.|(1) player connected\.|(\d+) players connected\.)$`)
	terrariaPlayer  = regexp.MustCompile(`^(?:: )?(.+) \([0-9A-Fa-f.:\[\]]+\)$`)
)

// ParseTerrariaPlaying reads Terraria's reply to playing from the log that
// follows the command: a row per player with their address, then a count.
//
//	Ada (172.18.0.1:51234)
//	1 player connected.
//
// Lines before the reply are skipped; the rows must be right above the
// count, and only names are kept.
func ParseTerrariaPlaying(output string) (Players, bool) {
	lines, ok := logLines(output)
	if !ok {
		return Players{}, false
	}
	for index, line := range lines {
		match := terrariaSummary.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		count := 0
		switch {
		case match[2] != "":
			count = 1
		case match[3] != "":
			count, _ = strconv.Atoi(match[3])
		}
		if count > index || count > 255 {
			return Players{}, false
		}
		var players Players
		for _, row := range lines[index-count : index] {
			player := terrariaPlayer.FindStringSubmatch(row)
			if player == nil {
				return Players{}, false
			}
			players.Online++
			// Terraria's commands take the whole name, as it shows.
			players.addWithID(player[1], player[1])
		}
		return players, true
	}
	return Players{}, false
}

var (
	// Bedrock's own log lines start with a time and level; a command's
	// reply may too.
	bedrockPrefix  = regexp.MustCompile(`^(?:: )?(?:\[\d{4}-\d\d-\d\d \d\d:\d\d:\d\d(?::\d{3})? [A-Z]+\] )?`)
	bedrockHeader  = regexp.MustCompile(`^There are (\d+)/(\d+) players online:$`)
	bedrockLogLine = regexp.MustCompile(`^(?:: )?\[\d{4}-\d\d-\d\d \d\d:\d\d:\d\d(?::\d{3})? [A-Z]+\] `)
)

// ParseBedrockList reads Minecraft Bedrock's reply to list from the log that
// follows the command: a count, then the names on the next line.
//
//	There are 2/10 players online:
//	Ada, Zed
func ParseBedrockList(output string) (Players, bool) {
	lines, ok := logLines(output)
	if !ok {
		return Players{}, false
	}
	for index, line := range lines {
		match := bedrockHeader.FindStringSubmatch(bedrockPrefix.ReplaceAllString(line, ""))
		if match == nil {
			continue
		}
		online, _ := strconv.Atoi(match[1])
		capacity, _ := strconv.Atoi(match[2])
		players := Players{Online: online, Max: capacity}
		if online == 0 {
			return players, true
		}
		// The names line has no time and level, unlike log lines the server
		// writes meanwhile.
		next := index + 1
		for next < len(lines) && bedrockLogLine.MatchString(lines[next]) {
			next++
		}
		if next >= len(lines) {
			return Players{}, false
		}
		names := strings.Split(lines[next], ", ")
		if len(names) != online {
			return Players{}, false
		}
		for _, name := range names {
			if strings.TrimSpace(name) == "" {
				return Players{}, false
			}
			// Bedrock's commands take the gamertag, as it shows.
			players.addWithID(name, name)
		}
		return players, true
	}
	return Players{}, false
}

var (
	// Vintage Story's log lines start with a date, a time, and a level.
	vintageStoryPrefix = regexp.MustCompile(`^\d\d\.\d\d\.\d{4} \d\d:\d\d:\d\d \[Server [A-Za-z]+\] `)
	// A connected client is "{4} [{0}] {1} {2} ({3}ms) ({5}s inactive)": its
	// client ID, account name, address, and ping, by the server's own format.
	vintageStoryClient = regexp.MustCompile(`^(?:.* )?\[\d+\] ([A-Za-z0-9_-]{1,32}) \S+ \(-?\d+ms\)(?: \(\d+s inactive\))?$`)
)

// ParseVintageStoryClients reads Vintage Story's reply to /list clients from
// the log that follows the command: a header, a row per player, and a blank
// line that ends the reply.
//
//	20.11.2026 19:04:32 [Server Notification] List of online Players
//	 [3] Ada 172.18.0.1:51234 (12ms) (0s inactive)
func ParseVintageStoryClients(output string) (Players, bool) {
	lines, ok := logLines(output)
	if !ok {
		return Players{}, false
	}
	for index, line := range lines {
		if vintageStoryPrefix.ReplaceAllString(line, "") != "List of online Players" || !vintageStoryPrefix.MatchString(line) {
			continue
		}
		var players Players
		for _, row := range lines[index+1:] {
			if row == "" {
				return players, true
			}
			client := vintageStoryClient.FindStringSubmatch(row)
			if client == nil {
				return Players{}, false
			}
			players.Online++
			// Its commands take the account name.
			players.addWithID(client[1], client[1])
		}
		return Players{}, false
	}
	return Players{}, false
}
