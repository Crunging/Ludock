package players

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/console"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/servers"
)

// ConsoleSource reads players by sending the game's list command through
// its console adapter, with the console's own helper and password.
type ConsoleSource struct {
	Identity *identity.Store
	Observer *servers.Observer
	Runtime  *console.Runtime
	Locks    *locks.Table
}

func (c *ConsoleSource) Lists(container servers.Container) bool {
	return container.Console != nil && container.Console.ListsPlayers
}

var errSkipped = errors.New("player reading skipped")

// Read takes one reading under the server lock, as background work that
// anything needing the server cancels.
func (c *ConsoleSource) Read(ctx context.Context, server identity.Server) (*Reading, error) {
	ctx, cancel := context.WithTimeout(ctx, readLimit)
	defer cancel()
	release, err := c.Locks.Acquire([]string{"server:" + server.ID}, cancel)
	if err != nil {
		return nil, err
	}
	// Held until the console helper, if any, is removed.
	defer release()
	observed, _, err := c.Observer.Inspect(ctx, server.ContainerID)
	if err != nil {
		return nil, err
	}
	if _, err := c.Identity.AssertObservedBinding(ctx, server.ID, observed.Observation, server.BindingRevision); err != nil {
		return nil, err
	}
	container := observed.Container
	adapter := games.ResolveAdapter(container.GameType, container.Labels)
	if container.State != "running" || adapter == nil || adapter.Players == nil {
		return nil, nil
	}
	assertBinding := func() error {
		_, err := c.Identity.ResolveBinding(ctx, server.ID, server.BindingRevision)
		return err
	}
	reading := &Reading{ContainerID: container.ID, StartedAt: container.StateSince}
	response, err := c.Runtime.Query(ctx, console.Target{ContainerID: container.ID, Labels: container.Labels, State: container.State},
		adapter, adapter.Players.Command, assertBinding)
	// Work that needed the server cancelled this reading, or Ludock is
	// stopping: neither says anything about the console.
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil, errSkipped
	}
	if err == nil {
		// Redacted before parsing, so no name carries a secret.
		var redacted strings.Builder
		redactor := console.NewRedactor(observed.Observation.Secrets, func(value string) { redacted.WriteString(value) })
		redactor.Push(response)
		redactor.End()
		players, ok := adapter.Players.Parse(redacted.String())
		reading.Players, reading.Failed = players, !ok
	} else {
		reading.Failed = true
	}
	reading.CheckedAt = time.Now().UnixMilli()
	return reading, nil
}
