// Command fakedocker serves an in-memory Docker Engine API for development
// and browser tests. It never talks to a real daemon.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
)

func main() {
	directory := flag.String("dir", "", "directory for the socket and server data")
	self := flag.String("self", "ludock-dev", "name of the fake Ludock container (LUDOCK_SELF_CONTAINER)")
	backups := flag.String("backups", "", "backup root mounted into the fake Ludock container")
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
	if backups != "" {
		os.MkdirAll(backups, 0o755)
		selfMounts = append(selfMounts, docker.MountPoint{Type: "bind", Source: backups, Destination: backups, RW: true})
	}
	engine.AddContainer(fakedocker.GameOptions{ID: self, Name: self, Image: image, Running: true, Mounts: selfMounts})
	engine.OnStart = func(c *fakedocker.Container) {
		if c.CreateRequest.Labels["ludock.internal"] == "mount-validator" {
			var out bytes.Buffer
			helper.RunMountProof(c.CreateRequest.Cmd[2], "1", &out)
			engine.Update(c.ID, func(c *fakedocker.Container) { c.Logs = []string{strings.TrimSpace(out.String())} })
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
			var out, errs bytes.Buffer
			code := helper.Main(args, stdin, &out, &errs)
			stdout(out.Bytes())
			stderr(errs.Bytes())
			return code
		}
		// Console commands behind the bounded wrapper.
		if len(cmd) > 8 && cmd[3] == "ludock-console" {
			stdout([]byte("> " + strings.Join(cmd[8:], " ") + "\nThere are 0 of a max of 10 players online.\n"))
		}
		return 0
	}
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
	id := engine.AddContainer(fakedocker.GameOptions{Name: "minecraft", Image: "itzg/minecraft-server:java21", Running: true,
		Labels: map[string]string{"ludock.name": "Survival", "com.docker.compose.project": "games", "com.docker.compose.service": "minecraft",
			"com.docker.compose.container-number": "1"},
		Env:    []string{"RCON_PASSWORD=dev-rcon-secret", "EULA=TRUE"},
		Mounts: []docker.MountPoint{{Type: "bind", Source: mc, Destination: mc, RW: true}},
		Ports:  map[string][]docker.PortBinding{"25565/tcp": {{HostIP: "0.0.0.0", HostPort: "25565"}}}})
	engine.Update(id, func(c *fakedocker.Container) {
		c.Logs = []string{"[Server thread/INFO]: Starting minecraft server version 1.21", "[Server thread/INFO]: Done (3.2s)! For help, type \"help\""}
	})
	valheim := data("valheim")
	engine.AddContainer(fakedocker.GameOptions{Name: "valheim", Image: "lloesche/valheim-server", Running: false,
		Mounts: []docker.MountPoint{{Type: "bind", Source: valheim, Destination: valheim, RW: true}},
		Ports:  map[string][]docker.PortBinding{"2456/udp": {{HostIP: "0.0.0.0", HostPort: "2456"}}}})
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
		c.Config.Env = []string{"EULA=TRUE", "RCON_PASSWORD=demo-rcon-secret"}
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
