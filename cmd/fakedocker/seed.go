package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
)

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
	// on 127.0.0.1. A game with a stdin console reads its commands.
	var stdinGames sync.Map
	engine.Attach = func(c *fakedocker.Container, stdin *bufio.Reader, stdout func([]byte)) {
		if c.CreateRequest.Labels["ludock.internal"] == "console-helper" {
			helper.RunNetworkConsole(stdin, writer(stdout))
		}
		if game, ok := stdinGames.Load(c.ID); ok {
			for {
				line, err := stdin.ReadString('\n')
				if line = strings.TrimRight(line, "\r\n"); line != "" {
					game.(func(string))(line)
				}
				if err != nil {
					return
				}
			}
		}
	}
	engine.AddGame = func(game, name string) bool {
		if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`).MatchString(name) {
			return false
		}
		switch game {
		case "terraria":
			id, command := terrariaGame(engine, dir, name)
			stdinGames.Store(id, command)
		case "factorio":
			factorioGame(engine, dir, name)
		case "minecraft":
			// Like Survival, its name in Ludock isn't its container's. A tag
			// of its own keeps its image updates apart from Survival's.
			minecraftServer(engine, dir, name, "itzg/minecraft-server:"+name, map[string]string{"ludock.name": "Realm " + name}, nil, nil)
		default:
			return false
		}
		return true
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
		// Like sh, a command exits with its own status.
		if len(cmd) > 7 && cmd[3] == "ludock-console" {
			command := cmd[len(cmd)-1]
			if text, ok := strings.CutPrefix(command, "echo "); ok {
				stdout([]byte(text + "\n"))
			}
			if code, ok := strings.CutPrefix(command, "exit "); ok {
				status, _ := strconv.Atoi(code)
				return status
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

// gameData makes a fake game's data folder under dir.
func gameData(dir, name string, files map[string]string) string {
	path := filepath.Join(dir, "data", name)
	for file, content := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(path, file)), 0o755)
		os.WriteFile(filepath.Join(path, file), []byte(content), 0o644)
	}
	return path
}

// devData makes a dev server's data folder under dir, with a world and
// server.properties.
func devData(dir, name string) string {
	path := filepath.Join(dir, "data", name)
	os.MkdirAll(filepath.Join(path, "world"), 0o755)
	os.WriteFile(filepath.Join(path, "server.properties"), []byte("motd=Hello from "+name+"\nmax-players=10\n"), 0o644)
	os.WriteFile(filepath.Join(path, "world", "level.dat"), []byte{0x1f, 0x8b, 0, 1, 2, 3}, 0o644)
	return path
}

// minecraftServer adds a running Minecraft server like the dev Survival,
// with its own console, status query, and data folder, and returns its ID.
// env and logs add to its own.
func minecraftServer(engine *fakedocker.Engine, dir, name, image string, labels map[string]string, env, logs []string) string {
	mc := devData(dir, name)
	game, rcon, query := minecraftGame("dev-rcon-secret")
	labels = maps.Clone(labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels["ludock.query.port"] = query
	id := engine.AddContainer(fakedocker.GameOptions{Name: name, Image: image, Running: true,
		Labels: labels, Env: append([]string{"RCON_PORT=" + rcon, "EULA=TRUE"}, env...),
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
		c.Logs = append(append([]string{"[Server thread/INFO]: Starting minecraft server version 1.21"}, logs...),
			"[Server thread/INFO]: Done (3.2s)! For help, type \"help\"")
	})
	return id
}

// seedDev adds the servers the journeys and local development use.
func seedDev(engine *fakedocker.Engine, dir string) {
	minecraftServer(engine, dir, "minecraft", "itzg/minecraft-server:java21", map[string]string{"ludock.name": "Survival", "com.docker.compose.project": "games",
		"com.docker.compose.service": "minecraft", "com.docker.compose.container-number": "1"},
		[]string{"DB_PASS=dev-db-secret", "STATS_URL=postgres://stats:dev-url-secret@db/stats"},
		[]string{"[Server thread/INFO]: Stats plugin using dev-db-secret for postgres://stats:dev-url-secret@db/stats"})
	valheim := devData(dir, "valheim")
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
	// Like the image's README, the server it downloads is mounted too.
	valheimServer := gameData(dir, "valheim-server", map[string]string{"valheim_server.x86_64": "server"})
	valheimContainer := engine.AddContainer(fakedocker.GameOptions{Name: "valheim", Image: "lloesche/valheim-server", Running: false,
		Labels: map[string]string{"ludock.query.port": fmt.Sprint(valheimQuery)},
		Mounts: []docker.MountPoint{{Type: "bind", Source: valheim, Destination: valheim, RW: true},
			{Type: "bind", Source: valheimServer, Destination: "/opt/valheim", RW: true}},
		Ports: map[string][]docker.PortBinding{"2456/udp": {{HostIP: "0.0.0.0", HostPort: "2456"}}}})
	valheimID.Store(valheimContainer)
	// 1 GiB of a 2 GiB limit; journeys change the use with POST /fake/stats.
	engine.Update(valheimContainer, func(c *fakedocker.Container) {
		c.Stats.MemoryStats.Usage, c.Stats.MemoryStats.Limit, c.Host.Memory = 1<<30, 2*(1<<30), 2*(1<<30)
	})
	// The image turns the REST API on in its own environment.
	palworldRCON, palworldREST := palworldGame("dev-palworld-secret")
	engine.AddContainer(fakedocker.GameOptions{Name: "palworld", Image: "thijsvanloef/palworld-server-docker", Running: false,
		Env: []string{"RCON_ENABLED=true", "RCON_PORT=" + palworldRCON, "ADMIN_PASSWORD=dev-palworld-secret",
			"REST_API_ENABLED=true", "REST_API_PORT=" + palworldREST},
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

// seedExtra adds a running Minecraft server with its own console, status
// query, and data folder, for runs that need many servers.
func seedExtra(engine *fakedocker.Engine, dir string, index int) {
	name := fmt.Sprintf("minecraft-%d", index)
	path := filepath.Join(dir, "data", name)
	os.MkdirAll(filepath.Join(path, "world"), 0o755)
	os.WriteFile(filepath.Join(path, "server.properties"), []byte("motd=Hello from "+name+"\n"), 0o644)
	os.WriteFile(filepath.Join(path, "world", "level.dat"), []byte{0x1f, 0x8b, 0, 1, 2, 3}, 0o644)
	game, rcon, query := minecraftGame("extra-rcon-secret")
	id := engine.AddContainer(fakedocker.GameOptions{Name: name, Image: "itzg/minecraft-server:java21", Running: true,
		Labels: map[string]string{"ludock.name": fmt.Sprintf("World %d", index), "ludock.query.port": query},
		Env:    []string{"RCON_PORT=" + rcon, "RCON_PASSWORD=extra-rcon-secret", "EULA=TRUE"},
		Mounts: []docker.MountPoint{{Type: "bind", Source: path, Destination: path, RW: true}}})
	engine.Update(id, func(c *fakedocker.Container) {
		c.Files = map[string]string{}
		game.publish = func(files map[string]string) {
			engine.Update(id, func(c *fakedocker.Container) { maps.Copy(c.Files, files) })
		}
		c.Logs = []string{"[Server thread/INFO]: Done (3.2s)! For help, type \"help\""}
	})
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
