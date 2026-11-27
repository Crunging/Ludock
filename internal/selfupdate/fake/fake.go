// Package fake runs Ludock's update helper in fakedocker's process, as its
// container would, against the fake socket.
package fake

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/selfupdate"
)

// Options adjust the fake helper for tests.
type Options struct {
	Poll, HealthWait time.Duration
	// Started sees each helper run start, with what ends it; a helper ended
	// so never exits, as one killed with its host.
	Started func(cancel context.CancelFunc)
	// Unhealthy is an image ID whose containers report unhealthy once
	// started, as a broken release would.
	Unhealthy string
}

// Dev are the options for a development daemon.
func Dev() Options {
	return Options{Unhealthy: "sha256:" + strings.Repeat("b", 64)}
}

// Run installs the helper on engine.
func Run(engine *fakedocker.Engine, options Options) {
	if engine.Starts == nil {
		engine.Starts = map[string]func(*fakedocker.Container){}
	}
	engine.Starts["self-update"] = func(c *fakedocker.Container) {
		cmd := c.CreateRequest.Cmd
		if len(cmd) < 4 || cmd[0] != "self-update" {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		if options.Started != nil {
			options.Started(cancel)
		}
		run := &selfupdate.Helper{Docker: docker.New(engine.Socket), Container: cmd[1], Image: cmd[2], Operation: cmd[3],
			Poll: options.Poll, HealthWait: options.HealthWait}
		if len(cmd) > 4 {
			run.Policy = json.RawMessage(cmd[4])
		}
		go func() {
			code := run.Run(ctx)
			if ctx.Err() == nil {
				engine.Exit(c.ID, code)
			}
			cancel()
		}()
	}
	engine.Starts[""] = func(c *fakedocker.Container) {
		if options.Unhealthy != "" && c.Image == options.Unhealthy {
			engine.Update(c.ID, func(c *fakedocker.Container) { c.State.Health = &docker.HealthState{Status: "unhealthy"} })
		}
	}
}
