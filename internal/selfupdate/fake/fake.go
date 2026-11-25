// Package fake runs Ludock's update helpers in fakedocker's process, as
// their containers would: the helper against the fake socket, and its data
// steps on the host folders their mounts name.
package fake

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/selfupdate"
	"github.com/Crunging/Ludock/internal/update"
)

// Options adjust the fake helpers for tests.
type Options struct {
	Poll, HealthWait time.Duration
	// Step, when set, sees each data step's command before it runs, and
	// answers its exit code in its place when handled.
	Step func(cmd []string) (code int, handled bool)
	// Started sees each helper run start, with what ends it and what closes
	// once it ended.
	Started func(cancel context.CancelFunc, done <-chan struct{})
	// Unhealthy is an image ID whose containers report unhealthy once
	// started, as a broken release would.
	Unhealthy string
	// Ready has a replacement Ludock write its readiness marker once it
	// starts, for a Ludock that isn't in the container it stands for.
	Ready bool
}

// Dev are the options for a development daemon, whose Ludock isn't in the
// container it stands for: that Ludock keeps its data open while its
// container is "stopped", so putting a version back leaves the data alone.
func Dev() Options {
	return Options{Unhealthy: "sha256:" + strings.Repeat("b", 64), Ready: true, Step: func([]string) (int, bool) { return 0, true }}
}

// Run installs the helpers on engine.
func Run(engine *fakedocker.Engine, options Options) {
	if engine.Starts == nil {
		engine.Starts = map[string]func(*fakedocker.Container){}
	}
	engine.Starts["self-update"] = func(c *fakedocker.Container) {
		cmd := c.CreateRequest.Cmd
		if len(cmd) != 5 {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		if options.Started != nil {
			options.Started(cancel, done)
		}
		run := &selfupdate.Helper{Docker: docker.New(engine.Socket), Container: cmd[2], Image: cmd[3], Operation: cmd[4],
			Poll: options.Poll, HealthWait: options.HealthWait}
		go func() {
			defer close(done)
			defer cancel()
			run.Run(ctx)
		}()
	}
	client := docker.New(engine.Socket)
	engine.Starts[""] = func(c *fakedocker.Container) {
		if options.Unhealthy != "" && c.Image == options.Unhealthy {
			engine.Update(c.ID, func(c *fakedocker.Container) { c.State.Health = &docker.HealthState{Status: "unhealthy"} })
		}
		if operation := c.Config.Labels[update.OperationLabel]; options.Ready && operation != "" {
			if info := engine.Container(c.ID); info != nil {
				name, body := selfupdate.ReadyMarker(operation, info.ID, info.State.StartedAt, info.Image)
				go client.Create(context.Background(), name, "", body)
			}
		}
	}
	engine.Starts["self-update-data"] = func(c *fakedocker.Container) {
		code, handled := 0, false
		if options.Step != nil {
			code, handled = options.Step(c.CreateRequest.Cmd)
		}
		if !handled {
			code = data(c)
		}
		engine.Exit(c.ID, code)
	}
}

func data(c *fakedocker.Container) int {
	cmd := c.CreateRequest.Cmd
	dataDir := ""
	for _, entry := range c.CreateRequest.Env {
		if value, ok := strings.CutPrefix(entry, "LUDOCK_DATA_DIR="); ok {
			dataDir = value
		}
	}
	dataDir = host(c, dataDir)
	switch {
	case len(cmd) > 0 && cmd[0] == "self-update-data":
		return selfupdate.DataMain(cmd[1:], dataDir, io.Discard)
	case len(cmd) == 2 && cmd[0] == "restore-data":
		if _, err := db.RestoreSnapshot(host(c, cmd[1]), filepath.Join(dataDir, "ludock.db")); err != nil {
			return db.RestoreExitCode(err)
		}
		return 0
	}
	return 127
}

// host maps a path in the container to the host folder its mount names.
func host(c *fakedocker.Container, path string) string {
	if c.CreateRequest.HostConfig == nil {
		return path
	}
	for _, mount := range c.CreateRequest.HostConfig.Mounts {
		if helper.Within(path, mount.Target) {
			return filepath.Join(mount.Source, strings.TrimPrefix(path, mount.Target))
		}
	}
	return path
}
