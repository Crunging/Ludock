// Command fakedocker serves an in-memory Docker Engine API for development
// and browser tests. It never talks to a real daemon.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
)

func main() {
	directory := flag.String("dir", "", "directory for the socket and server data")
	self := flag.String("self", "ludock-dev", "name of the fake Ludock container (LUDOCK_SELF_CONTAINER)")
	backups := flag.String("backups", "", "backup roots mounted into the fake Ludock container, ':'-separated")
	demo := flag.Bool("demo", false, "seed the servers shown in the README screenshots")
	flag.Parse()
	if *directory == "" {
		fmt.Fprintln(os.Stderr, "Usage: fakedocker -dir DIR [-backups DIR] [-demo]")
		os.Exit(2)
	}
	os.MkdirAll(*directory, 0o755)
	engine, err := fakedocker.Start(*directory)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	seed(engine, *directory, *self, *backups, *demo)
	fmt.Println(engine.Socket)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	engine.Close()
}

// seed adds a few game servers whose data lives under dir.
func seed(engine *fakedocker.Engine, dir, self, backups string, demo bool) {
	helper.HostRoot = "/"
	image := "ghcr.io/crunging/ludock@sha256:" + strings.Repeat("ab", 32)
	engine.AddImage(image)
	if demo {
		seedDemo(engine, dir)
	} else {
		seedDev(engine, dir)
	}
	var selfMounts []docker.MountPoint
	for _, root := range strings.Split(backups, ":") {
		if root != "" {
			os.MkdirAll(root, 0o755)
			selfMounts = append(selfMounts, docker.MountPoint{Type: "bind", Source: root, Destination: root, RW: true})
		}
	}
	engine.AddContainer(fakedocker.GameOptions{ID: self, Name: self, Image: image, Running: true, Mounts: selfMounts})
	engine.OnStart = func(c *fakedocker.Container) {
		if c.CreateRequest.Labels["ludock.internal"] == "mount-validator" {
			var out bytes.Buffer
			helper.RunMountProof(c.CreateRequest.Cmd[2], "1", &out)
			engine.Update(c.ID, func(c *fakedocker.Container) { c.Logs = []string{strings.TrimSpace(out.String())} })
		}
	}
	// Console helpers run for real, so commands reach the fake game consoles
	// on 127.0.0.1.
	engine.Attach = func(c *fakedocker.Container, stdin *bufio.Reader, stdout func([]byte)) {
		if c.CreateRequest.Labels["ludock.internal"] == "console-helper" {
			helper.RunNetworkConsole(stdin, writer(stdout))
		}
	}
	engine.Exec = func(c *fakedocker.Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int {
		if len(cmd) > 2 && cmd[0] == helper.Binary {
			args := append([]string(nil), cmd[2:]...)
			if c.CreateRequest.HostConfig != nil {
				for _, mount := range c.CreateRequest.HostConfig.Mounts {
					for index := range args {
						args[index] = strings.ReplaceAll(args[index], `"`+mount.Target, `"`+mount.Source)
					}
				}
			}
			// Journeys set disk figures with POST /fake/disk.
			var request helper.FileRequest
			if args[0] == "file" && len(args) > 1 && json.Unmarshal([]byte(args[1]), &request) == nil && request.Operation == "space" {
				if result, ok := engine.Disk(request.Root); ok {
					if result == "" {
						stderr([]byte(helper.FileFailure))
						return 1
					}
					stdout([]byte(result))
					return 0
				}
			}
			var out, errs bytes.Buffer
			code := helper.Main(args, stdin, &out, &errs)
			stdout(out.Bytes())
			stderr(errs.Bytes())
			return code
		}
		// Shell commands behind the bounded wrapper: timeout -k 5 60 /bin/sh -c command.
		if len(cmd) > 7 && cmd[3] == "ludock-console" {
			if text, ok := strings.CutPrefix(cmd[len(cmd)-1], "echo "); ok {
				stdout([]byte(text + "\n"))
			}
		}
		return 0
	}
}

type writer func([]byte)

func (w writer) Write(data []byte) (int, error) {
	w(data)
	return len(data), nil
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
	query, err := fakedocker.ServeMinecraftStatus(game.status)
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

// palworldRCON serves a Palworld RCON console and returns its port. Each
// player keeps the Steam ID they first joined with.
func palworldRCON(password string) string {
	game := &fakeGame{}
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
		if verb != "KickPlayer" && verb != "BanPlayer" {
			return "", false
		}
		for _, name := range game.online {
			if steamID(name) == id {
				game.online = slices.DeleteFunc(game.online, func(listed string) bool { return listed == name })
				if verb == "BanPlayer" {
					game.banned = append(game.banned, name)
				}
				return map[string]string{"KickPlayer": "Kicked: ", "BanPlayer": "Banned: "}[verb] + name, true
			}
		}
		return "Failed: no player with that ID", true
	})
}

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
}

func (g *fakeGame) status() (int, int, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.online), 10, !time.Now().Before(g.hungUntil)
}

// rcon serves an RCON console that answers the game's list command with
// format, and the game's own commands with commands, and returns its port.
// "say MESSAGE" answers with the message as players see it. Besides those,
// it takes commands only the fake has: "fake-said" lists what "say" sent,
// "fake-join NAME" and "fake-leave
// NAME" change who is online, "fake-outage SECONDS" makes the list fail for
// that long, and "fake-hang SECONDS" also silences the status query, as a
// game that hangs while its container runs.
func (g *fakeGame) rcon(password, list string, format func(online []string) string,
	commands func(verb, argument string) (string, bool)) string {
	port, err := fakedocker.ServeRCON(password, func(command string) string {
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

// seedDev adds the servers the journeys and local development use.
func seedDev(engine *fakedocker.Engine, dir string) {
	data := func(name string) string {
		path := filepath.Join(dir, "data", name)
		os.MkdirAll(filepath.Join(path, "world"), 0o755)
		os.WriteFile(filepath.Join(path, "server.properties"), []byte("motd=Hello from "+name+"\nmax-players=10\n"), 0o644)
		os.WriteFile(filepath.Join(path, "world", "level.dat"), []byte{0x1f, 0x8b, 0, 1, 2, 3}, 0o644)
		return path
	}
	mc := data("minecraft")
	game, rcon, query := minecraftGame("dev-rcon-secret")
	id := engine.AddContainer(fakedocker.GameOptions{Name: "minecraft", Image: "itzg/minecraft-server:java21", Running: true,
		Labels: map[string]string{"ludock.name": "Survival", "com.docker.compose.project": "games", "com.docker.compose.service": "minecraft",
			"com.docker.compose.container-number": "1", "ludock.query.port": query},
		Env: []string{"RCON_PORT=" + rcon, "EULA=TRUE", "DB_PASS=dev-db-secret",
			"STATS_URL=postgres://stats:dev-url-secret@db/stats"},
		Mounts: []docker.MountPoint{{Type: "bind", Source: mc, Destination: mc, RW: true}},
		Ports:  map[string][]docker.PortBinding{"25565/tcp": {{HostIP: "0.0.0.0", HostPort: "25565"}}}})
	// Like the real image, every version of the fake one has a healthcheck,
	// and pulling it yields a newer version.
	healthcheck := map[string]any{"Healthcheck": map[string]any{"Test": []any{"CMD-SHELL", "mc-health"}}}
	for _, version := range []string{"0", "1"} {
		engine.SetImageConfig("sha256:"+strings.Repeat(version, 64), healthcheck)
	}
	engine.Update(id, func(c *fakedocker.Container) {
		c.ExtraConfig = healthcheck
		c.State.Health = &docker.HealthState{Status: "healthy"}
		// A quarter of one core of four, and 1.5 GiB of a 4 GiB limit once
		// 0.5 GiB of reclaimable file cache is left out.
		c.Stats.CPUStats.CPUUsage.TotalUsage, c.Stats.CPUStats.SystemCPUUsage, c.Stats.CPUStats.OnlineCPUs = 125e6, 2e9, 4
		c.Stats.MemoryStats.Usage, c.Stats.MemoryStats.Limit, c.Host.Memory = 2*(1<<30), 4*(1<<30), 4*(1<<30)
		c.Stats.MemoryStats.Stats = map[string]float64{"inactive_file": 0.5 * (1 << 30)}
		// Like the real image, which generates a password when given none.
		c.Files = map[string]string{"/data/.rcon-cli.env": "password=dev-rcon-secret\n",
			"/data/server.properties": "motd=A Minecraft Server\nwhite-list=false\n"}
		game.publish = func(files map[string]string) {
			engine.Update(id, func(c *fakedocker.Container) { maps.Copy(c.Files, files) })
		}
		c.Logs = []string{"[Server thread/INFO]: Starting minecraft server version 1.21",
			"[Server thread/INFO]: Stats plugin using dev-db-secret for postgres://stats:dev-url-secret@db/stats",
			"[Server thread/INFO]: Done (3.2s)! For help, type \"help\""}
	})
	valheim := data("valheim")
	// Valheim has no console; its status query says two of ten are online,
	// and goes silent while a file named fake-silent is in its data folder.
	// A file named fake-hang silences it only until the container starts
	// again, as a hang that a restart cures.
	var valheimID atomic.Value
	valheimQuery, err := fakedocker.ServeA2S("Valheim", func() (int, int, bool) {
		if _, err := os.Stat(filepath.Join(valheim, "fake-silent")); err == nil {
			return 2, 10, false
		}
		if hang, err := os.Stat(filepath.Join(valheim, "fake-hang")); err == nil {
			id, _ := valheimID.Load().(string)
			if c := engine.Container(id); c != nil {
				started, _ := time.Parse(time.RFC3339Nano, c.State.StartedAt)
				return 2, 10, hang.ModTime().Before(started)
			}
		}
		return 2, 10, true
	})
	if err != nil {
		panic(err)
	}
	valheimContainer := engine.AddContainer(fakedocker.GameOptions{Name: "valheim", Image: "lloesche/valheim-server", Running: false,
		Labels: map[string]string{"ludock.query.port": fmt.Sprint(valheimQuery)},
		Mounts: []docker.MountPoint{{Type: "bind", Source: valheim, Destination: valheim, RW: true}},
		Ports:  map[string][]docker.PortBinding{"2456/udp": {{HostIP: "0.0.0.0", HostPort: "2456"}}}})
	valheimID.Store(valheimContainer)
	// 1 GiB of a 2 GiB limit; journeys change the use with POST /fake/stats.
	engine.Update(valheimContainer, func(c *fakedocker.Container) {
		c.Stats.MemoryStats.Usage, c.Stats.MemoryStats.Limit, c.Host.Memory = 1<<30, 2*(1<<30), 2*(1<<30)
	})
	engine.AddContainer(fakedocker.GameOptions{Name: "palworld", Image: "thijsvanloef/palworld-server-docker", Running: false,
		Env:   []string{"RCON_ENABLED=true", "RCON_PORT=" + palworldRCON("dev-palworld-secret"), "ADMIN_PASSWORD=dev-palworld-secret"},
		Ports: map[string][]docker.PortBinding{"8211/udp": {{HostIP: "0.0.0.0", HostPort: "8211"}}}})
	engine.AddContainer(fakedocker.GameOptions{Name: "enshrouded", Image: "sknnr/enshrouded-dedicated-server:latest",
		Ports: map[string][]docker.PortBinding{"15636/udp": {{HostIP: "0.0.0.0", HostPort: "15636"}},
			"15637/udp": {{HostIP: "0.0.0.0", HostPort: "15637"}}}})
	// Containers Ludock doesn't manage by itself: a game on an image it
	// doesn't recognize, one its owner opted out, a tool that controls
	// Docker, and a game that does too.
	engine.AddContainer(fakedocker.GameOptions{Name: "satisfactory", Image: "ich777/steamcmd:satisfactory",
		Ports: map[string][]docker.PortBinding{"7777/udp": {{HostIP: "0.0.0.0", HostPort: "7777"}}}})
	engine.AddContainer(fakedocker.GameOptions{Name: "valheim-old", Image: "ich777/steamcmd:valheim",
		Labels: map[string]string{"ludock.enable": "false"}})
	engine.AddContainer(fakedocker.GameOptions{Name: "portainer", Image: "portainer/portainer-ce:2.21.4", Running: true,
		Mounts: []docker.MountPoint{{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", RW: true}}})
	engine.AddContainer(fakedocker.GameOptions{Name: "factorio", Image: "factoriotools/factorio:stable",
		Mounts: []docker.MountPoint{{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", RW: true}},
		Ports:  map[string][]docker.PortBinding{"34197/udp": {{HostIP: "0.0.0.0", HostPort: "34197"}}}})
}

// seedDemo adds the servers shown in the README screenshots, with files and
// start times that look like a few days of real use.
func seedDemo(engine *fakedocker.Engine, dir string) {
	now := time.Now()
	ago := func(d time.Duration) time.Time { return now.Add(-d).Truncate(time.Minute) }
	write := func(root, name, content string, modified time.Time) {
		path := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(content), 0o644)
		os.Chtimes(path, modified, modified)
	}
	touchDirs := func(root string, modified time.Time, names ...string) {
		for _, name := range names {
			path := filepath.Join(root, name)
			os.MkdirAll(path, 0o755)
			os.Chtimes(path, modified, modified)
		}
	}
	server := func(name, title, image, port string, running bool, since time.Duration) (string, string) {
		root := filepath.Join(dir, "data", name)
		os.MkdirAll(root, 0o755)
		labels := map[string]string{"ludock.name": title, "com.docker.compose.project": "games",
			"com.docker.compose.service": name, "com.docker.compose.container-number": "1"}
		id := engine.AddContainer(fakedocker.GameOptions{Name: name, Image: image, Running: running, Labels: labels,
			Mounts: []docker.MountPoint{{Type: "bind", Source: root, Destination: root, RW: true}},
			Ports:  map[string][]docker.PortBinding{port: {{HostIP: "0.0.0.0", HostPort: strings.Split(port, "/")[0]}}}})
		engine.Update(id, func(c *fakedocker.Container) {
			at := ago(since).UTC().Format(time.RFC3339Nano)
			if running {
				c.State.StartedAt = at
			} else {
				c.State.StartedAt, c.State.FinishedAt = ago(since+72*time.Hour).UTC().Format(time.RFC3339Nano), at
			}
		})
		return id, root
	}

	id, minecraft := server("minecraft", "Friends\u2019 survival world", "itzg/minecraft-server:java21", "25565/tcp", true, 3*time.Hour)
	engine.Update(id, func(c *fakedocker.Container) {
		game, rcon, query := minecraftGame("demo-rcon-secret", "Mara", "Theo")
		game.allowed, game.allowlistOn, c.Files = []string{"Mara", "Theo", "Juno", "Rafi"}, true, map[string]string{}
		game.publish = func(files map[string]string) {
			engine.Update(id, func(c *fakedocker.Container) { maps.Copy(c.Files, files) })
		}
		go func() {
			game.mu.Lock()
			defer game.mu.Unlock()
			game.publishLists()
		}()
		c.Config.Labels["ludock.query.port"] = query
		c.Config.Env = []string{"EULA=TRUE", "RCON_PASSWORD=demo-rcon-secret", "RCON_PORT=" + rcon}
		c.Logs = []string{"[Server thread/INFO]: Starting minecraft server version 1.21.4",
			"[Server thread/INFO]: Preparing level \"world\"", "[Server thread/INFO]: Done (4.1s)! For help, type \"help\"",
			"[Server thread/INFO]: Mara joined the game", "[Server thread/INFO]: Theo joined the game"}
	})
	touchDirs(minecraft, ago(2*time.Hour), "logs", "plugins", "world", "world_nether", "world_the_end")
	write(minecraft, "eula.txt", "eula=true\n", ago(9*24*time.Hour))
	write(minecraft, "server.properties", "motd=Friends' survival world\ndifficulty=normal\nmax-players=12\nview-distance=12\nwhite-list=true\n"+
		strings.Repeat("# tuned for a small group of friends\n", 30), ago(26*time.Hour))
	write(minecraft, "whitelist.json", `[{"name":"Mara"},{"name":"Theo"},{"name":"Juno"},{"name":"Rafi"}]`+"\n", ago(26*time.Hour))
	write(minecraft, "ops.json", `[{"name":"Mara","level":4}]`+"\n", ago(8*24*time.Hour))
	write(minecraft, "world/level.dat", string([]byte{0x1f, 0x8b, 0, 1, 2, 3}), ago(2*time.Hour))
	os.Chtimes(filepath.Join(minecraft, "world"), ago(2*time.Hour), ago(2*time.Hour))

	_, factorio := server("factorio", "Factorio weekend", "factoriotools/factorio:stable", "34197/udp", false, 20*time.Hour)
	touchDirs(factorio, ago(20*time.Hour), "config", "mods", "saves")
	write(factorio, "config/server-settings.json", `{"name":"Factorio weekend","max_players":8}`+"\n", ago(5*24*time.Hour))

	_, valheim := server("valheim", "Valheim co-op", "lloesche/valheim-server", "2456/udp", true, 27*time.Hour)
	touchDirs(valheim, ago(4*time.Hour), "config", "worlds_local")
	write(valheim, "config/adminlist.txt", "// Steam IDs of admins\n", ago(6*24*time.Hour))
}
