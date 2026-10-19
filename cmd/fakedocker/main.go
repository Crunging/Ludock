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

	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
)

func main() {
	directory := flag.String("dir", "", "directory for the socket and server data")
	self := flag.String("self", "ludock-dev", "name of the fake Ludock container (LUDOCK_SELF_CONTAINER)")
	backups := flag.String("backups", "", "backup root mounted into the fake Ludock container")
	flag.Parse()
	if *directory == "" {
		fmt.Fprintln(os.Stderr, "Usage: fakedocker -dir DIR [-backups DIR]")
		os.Exit(2)
	}
	os.MkdirAll(*directory, 0o755)
	engine, err := fakedocker.Start(*directory)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	seed(engine, *directory, *self, *backups)
	fmt.Println(engine.Socket)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	engine.Close()
}

// seed adds a few game servers whose data lives under dir.
func seed(engine *fakedocker.Engine, dir, self, backups string) {
	helper.HostRoot = "/"
	image := "ghcr.io/crunging/ludock@sha256:" + strings.Repeat("ab", 32)
	engine.AddImage(image)
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
