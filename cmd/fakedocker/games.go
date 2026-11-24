package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
)

// fakeGame is who is online on a fake game, shared by its console and
// status query.
type fakeGame struct {
	mu                     sync.Mutex
	online                 []string
	outageUntil, hungUntil time.Time
	// allowed and banned are the game's lists, and seen everyone who joined.
	allowed, banned, seen []string
	// said is every message broadcast with "say".
	said        []string
	allowlistOn bool
	// publish writes files into the game's container.
	publish func(files map[string]string)
	// zeroIDs answers every console command with ID 0.
	zeroIDs bool
}

func (g *fakeGame) status() (int, int, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.online), 10, !time.Now().Before(g.hungUntil)
}

// sample is the status with Minecraft's player sample, which a console
// outage also hides, as hide-online-players does.
func (g *fakeGame) sample() ([]string, int, int, bool) {
	online, capacity, ok := g.status()
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Now().Before(g.outageUntil) {
		return nil, online, capacity, ok
	}
	return slices.Clone(g.online), online, capacity, ok
}

// rcon serves an RCON console that answers the game's list command with
// format, and the game's own commands with commands, and returns its port.
// "say MESSAGE" answers with the message as players see it. Besides those,
// it takes commands only the fake has: "fake-said" lists what "say" sent,
// "fake-join NAME" and "fake-leave
// NAME" change who is online, "fake-outage SECONDS" makes the list fail, and
// hides Minecraft's player sample, for that long, and "fake-hang SECONDS"
// also silences the status query, as a game that hangs while its container
// runs.
func (g *fakeGame) rcon(password, list string, format func(online []string) string,
	commands func(verb, argument string) (string, bool)) string {
	serve := fakedocker.ServeRCON
	if g.zeroIDs {
		serve = fakedocker.ServePalworldRCON
	}
	port, err := serve(password, func(command string) string {
		g.mu.Lock()
		defer g.mu.Unlock()
		verb, argument, _ := strings.Cut(command, " ")
		if commands != nil {
			if reply, ok := commands(verb, argument); ok {
				return reply
			}
		}
		switch verb {
		case list:
			if time.Now().Before(g.outageUntil) {
				return "An unexpected error occurred trying to execute that command"
			}
			return format(g.online)
		case "say":
			g.said = append(g.said, argument)
			return "[Server] " + argument
		case "save-all":
			return "Saving the game (this may take a moment!)Saved the game"
		case "fake-said":
			return "Said: " + strings.Join(g.said, " | ")
		case "fake-join":
			if slices.Contains(g.banned, argument) {
				return argument + " is banned"
			}
			if !slices.Contains(g.online, argument) {
				g.online = append(g.online, argument)
			}
			return argument + " joined the game"
		case "fake-leave":
			g.online = slices.DeleteFunc(g.online, func(name string) bool { return name == argument })
			return argument + " left the game"
		case "fake-outage", "fake-hang":
			seconds, _ := strconv.Atoi(argument)
			g.outageUntil = time.Now().Add(time.Duration(seconds) * time.Second)
			if verb == "fake-hang" {
				g.hungUntil = g.outageUntil
				return "The game hangs for " + argument + " seconds"
			}
			return "RCON player lists fail for " + argument + " seconds"
		}
		return ""
	})
	if err != nil {
		panic(err)
	}
	return fmt.Sprint(port)
}

// minecraftGame serves a Minecraft RCON console and Server List Ping, and
// returns the game and their ports. Its console kicks, bans, and keeps an
// allowlist with the vanilla server's replies, and writes both lists where
// the itzg image keeps them once the game's publish is set.
func minecraftGame(password string, online ...string) (*fakeGame, string, string) {
	game := &fakeGame{online: online}
	member := func(list []string, name string) bool { return slices.Contains(list, name) }
	remove := func(list []string, name string) []string {
		return slices.DeleteFunc(list, func(listed string) bool { return listed == name })
	}
	rcon := game.rcon(password, "list", func(online []string) string {
		return fmt.Sprintf("There are %d of a max of 10 players online: %s", len(online), strings.Join(online, ", "))
	}, func(verb, name string) (string, bool) {
		switch verb {
		case "list":
			if name != "uuids" || time.Now().Before(game.outageUntil) {
				return "", false
			}
			var accounts []string
			for _, player := range game.online {
				accounts = append(accounts, fmt.Sprintf("%s (00000000-0000-4000-8000-%012x)", player, len(player)))
			}
			return fmt.Sprintf("There are %d of a max of 10 players online: %s", len(accounts), strings.Join(accounts, ", ")), true
		case "kick":
			if !member(game.online, name) {
				return "No player was found", true
			}
			game.online = remove(game.online, name)
			return "Kicked " + name + ": Kicked by an operator", true
		case "ban":
			if member(game.banned, name) {
				return "Nothing changed. The player is already banned", true
			}
			game.banned, game.online = append(game.banned, name), remove(game.online, name)
		case "pardon":
			if !member(game.banned, name) {
				return "Nothing changed. The player isn't banned", true
			}
			game.banned = remove(game.banned, name)
		case "whitelist":
			change, name, _ := strings.Cut(name, " ")
			switch {
			case change == "add" && member(game.allowed, name):
				return "Player is already whitelisted", true
			case change == "add":
				game.allowed = append(game.allowed, name)
			case change == "remove" && !member(game.allowed, name):
				return "Player is not whitelisted", true
			case change == "remove":
				game.allowed = remove(game.allowed, name)
			default:
				return "Unknown or incomplete command, see below for error", true
			}
			game.publishLists()
			return map[string]string{"add": "Added ", "remove": "Removed "}[change] + name +
				map[string]string{"add": " to the whitelist", "remove": " from the whitelist"}[change], true
		default:
			return "", false
		}
		game.publishLists()
		return map[string]string{"ban": "Banned " + name + ": Banned by an operator", "pardon": "Unbanned " + name}[verb], true
	})
	query, err := fakedocker.ServeMinecraftPlayers(game.sample)
	if err != nil {
		panic(err)
	}
	return game, rcon, fmt.Sprint(query)
}

// publishLists writes the game's allowlist and ban list as the server's
// whitelist.json and banned-players.json. The caller holds g.mu.
func (g *fakeGame) publishLists() {
	if g.publish == nil {
		return
	}
	entries := func(names []string) string {
		list := []map[string]string{}
		for _, name := range names {
			list = append(list, map[string]string{"uuid": "00000000-0000-0000-0000-000000000000", "name": name})
		}
		encoded, _ := json.Marshal(list)
		return string(encoded)
	}
	g.publish(map[string]string{"/data/whitelist.json": entries(g.allowed), "/data/banned-players.json": entries(g.banned),
		"/data/server.properties": "motd=A Minecraft Server\nwhite-list=" + strconv.FormatBool(g.allowlistOn) + "\n"})
}

// palworldGame serves a Palworld RCON console and REST API and returns their
// ports. Each player keeps the Steam ID they first joined with. Like
// Palworld, RCON's Broadcast sends only the first word, and the REST API's
// announce the whole message; fake-said lists both.
func palworldGame(password string) (string, string) {
	// Like Palworld 1.0, it answers with ID 0 and takes IDs only as steam_<ID>.
	game := &fakeGame{zeroIDs: true}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	go http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, given, ok := r.BasicAuth()
		var body struct{ Message string }
		switch {
		case !ok || user != "admin" || given != password:
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method != http.MethodPost || r.URL.Path != "/v1/api/announce" || json.NewDecoder(r.Body).Decode(&body) != nil:
			w.WriteHeader(http.StatusBadRequest)
		default:
			game.mu.Lock()
			game.said = append(game.said, body.Message)
			game.mu.Unlock()
			w.Write([]byte("OK"))
		}
	}))
	steamID := func(name string) string {
		index := slices.Index(game.seen, name)
		if index < 0 {
			game.seen, index = append(game.seen, name), len(game.seen)
		}
		return fmt.Sprintf("7656119800000%04d", index)
	}
	return game.rcon(password, "ShowPlayers", func(online []string) string {
		list := "name,playeruid,steamid\n"
		for index, name := range online {
			list += fmt.Sprintf("%s,%d,%s\n", name, 1000000+index, steamID(name))
		}
		return list
	}, func(verb, id string) (string, bool) {
		if verb == "Broadcast" {
			word, _, _ := strings.Cut(id, " ")
			game.said = append(game.said, word)
			return "Broadcasted: " + word, true
		}
		if verb != "KickPlayer" && verb != "BanPlayer" {
			return "", false
		}
		for _, name := range game.online {
			if "steam_"+steamID(name) == id {
				game.online = slices.DeleteFunc(game.online, func(listed string) bool { return listed == name })
				if verb == "BanPlayer" {
					game.banned = append(game.banned, name)
					return "Baned: " + id, true
				}
				return "Kicked: " + name, true
			}
		}
		return map[string]string{"KickPlayer": "Failed to Kick: ", "BanPlayer": "Failed to Ban: "}[verb] + id, true
	}), fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
}

// terrariaGame adds a running Terraria server like hexlo/terraria-server-docker's,
// and returns its ID and its console, which writes the real server's replies
// to its log. Like the real image, it never gets Docker's stop signal and
// quits only on exit. fake-join NAME and fake-leave NAME change who is
// online, as players connecting would.
func terrariaGame(engine *fakedocker.Engine, dir, name string) (string, func(string)) {
	data := gameData(dir, name, map[string]string{"world.wld": "terraria world"})
	id := engine.AddContainer(fakedocker.GameOptions{Name: name, Image: "hexlo/terraria-server-docker:latest", Running: true,
		OpenStdin: true, Mounts: []docker.MountPoint{{Type: "bind", Source: data, Destination: data, RW: true}},
		Ports: map[string][]docker.PortBinding{"7777/tcp": {{HostIP: "0.0.0.0", HostPort: "7777"}}}})
	engine.Update(id, func(c *fakedocker.Container) {
		c.IgnoresStop = true
		c.Logs = []string{"Terraria Server v1.4.5.8", "", "Listening on port 7777", "Type 'help' for a list of commands.", "",
			"\x1b]0;Terraria Server: " + name + "\x07: \x1b[6nServer started"}
	})
	var mu sync.Mutex
	var online []string
	address := func(player string) string { return fmt.Sprintf("172.18.0.1:%d", 40000+len(player)) }
	return id, func(line string) {
		mu.Lock()
		defer mu.Unlock()
		verb, player, _ := strings.Cut(line, " ")
		// The real server's replies, behind its prompt and cursor query.
		var reply []string
		switch {
		case verb == "playing":
			for _, player := range online {
				reply = append(reply, player+" ("+address(player)+")")
			}
			reply = append(reply, map[int]string{0: "No players connected.", 1: "1 player connected."}[len(online)])
			if len(online) > 1 {
				reply[len(reply)-1] = fmt.Sprintf("%d players connected.", len(online))
			}
		case (verb == "kick" || verb == "ban") && slices.Contains(online, player):
			reason := map[string]string{"kick": "Kicked from server.", "ban": "Banned from server."}[verb]
			reply = []string{address(player) + " was booted: " + reason}
			// Like the real server, it lists the player until they have left.
			time.AfterFunc(time.Second, func() {
				mu.Lock()
				defer mu.Unlock()
				online = slices.DeleteFunc(online, func(listed string) bool { return listed == player })
				engine.Update(id, func(c *fakedocker.Container) { c.Logs = append(c.Logs, ": \x1b[6n"+player+" has left.") })
			})
		case verb == "fake-join" && !slices.Contains(online, player):
			online = append(online, player)
			reply = []string{address(player) + " is connecting...", player + " has joined."}
		case verb == "fake-leave":
			online = slices.DeleteFunc(online, func(listed string) bool { return listed == player })
			reply = []string{player + " has left."}
		case verb == "say":
			reply = []string{"<Server> " + player}
		case line == "save":
			reply = []string{"Saving world data: 100%", "Validating world save: 100%", "Backing up world file"}
		case line == "exit":
			reply = []string{"Saving before exit...", "Saving world data: 100%", "Validating world save: 100%", "Backing up world file"}
		case line == "version":
			reply = []string{"Terraria Server v1.4.5.8"}
		}
		engine.Update(id, func(c *fakedocker.Container) {
			c.Logs = append(c.Logs, append([]string{": \x1b[6n" + line}, reply...)...)
		})
		if line == "exit" {
			engine.Exit(id, 0)
		}
	}
}

// factorioGame adds a running Factorio server like factoriotools/factorio's,
// whose RCON password is only in the file the image generates. Its console
// answers with Factorio 2's replies, and text that isn't a command is chat,
// which fake-said lists.
func factorioGame(engine *fakedocker.Engine, dir, name string) {
	data := gameData(dir, name, map[string]string{"saves/_autosave1.zip": "factorio save", "config/rconpw": "dev-factorio-secret\n"})
	game := &fakeGame{}
	member := func(list []string, name string) bool {
		return slices.ContainsFunc(list, func(listed string) bool { return strings.EqualFold(listed, name) })
	}
	rcon := game.rcon("dev-factorio-secret", "/players", func(online []string) string {
		return fmt.Sprintf("Players (%d):\n", len(online))
	}, func(verb, argument string) (string, bool) {
		name, _, _ := strings.Cut(argument, " ")
		switch {
		case verb == "/players" && argument == "online":
			list := fmt.Sprintf("Online players (%d):\n", len(game.online))
			for _, player := range game.online {
				list += "  " + player + " (online)\n"
			}
			return list, true
		case verb == "/server-save":
			return "Saving the map\n", true
		case verb == "/kick" && !member(game.online, name):
			return "Player " + name + " doesn't exist.\n", true
		case verb == "/kick":
			game.online = slices.DeleteFunc(game.online, func(listed string) bool { return listed == name })
			return "", true
		case verb == "/ban" && member(game.banned, name):
			return name + " is already banned.\n", true
		case verb == "/ban":
			game.banned = append(game.banned, name)
			game.online = slices.DeleteFunc(game.online, func(listed string) bool { return listed == name })
			return "Added " + name + " to the banlist.\n", true
		case !strings.HasPrefix(verb, "/") && !strings.HasPrefix(verb, "fake-"):
			game.said = append(game.said, strings.TrimSpace(verb+" "+argument))
			return "", true
		}
		return "", false
	})
	id := engine.AddContainer(fakedocker.GameOptions{Name: name, Image: "factoriotools/factorio:stable", Running: true,
		Env:    []string{"PORT=34197", "RCON_PORT=" + rcon, "CONFIG=/factorio/config"},
		Mounts: []docker.MountPoint{{Type: "bind", Source: data, Destination: data, RW: true}},
		Ports: map[string][]docker.PortBinding{"27015/tcp": {{HostIP: "0.0.0.0", HostPort: "27015"}},
			"34197/udp": {{HostIP: "0.0.0.0", HostPort: "34197"}}}})
	engine.Update(id, func(c *fakedocker.Container) {
		c.Files = map[string]string{"/factorio/config/rconpw": "dev-factorio-secret\n"}
		// Like the real image's, its health check asks the game over RCON.
		c.ExtraConfig = map[string]any{"Healthcheck": map[string]any{"Test": []any{"CMD-SHELL", "rcon-health"}}}
		c.State.Health = &docker.HealthState{Status: "healthy",
			Log: []docker.HealthResult{{End: time.Now().UTC().Format(time.RFC3339Nano)}}}
	})
}
