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

// GameSource asks the game itself: its console's list command, with the
// console's own helper and password, and its status query, in the same
// helper. A console list wins over a query's count, since it names players.
type GameSource struct {
	Identity *identity.Store
	Observer *servers.Observer
	Runtime  *console.Runtime
	Locks    *locks.Table
}

func (c *GameSource) Lists(container servers.Container) bool {
	return (container.Console != nil && container.Console.ListsPlayers) || container.Query != nil
}

var errSkipped = errors.New("player reading skipped")

// Read takes one reading under the server lock, as background work that
// anything needing the server cancels.
func (c *GameSource) Read(ctx context.Context, server identity.Server) (*Reading, error) {
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
	probe := console.Probe{Query: container.Query}
	if adapter != nil && adapter.Players != nil {
		probe.Adapter, probe.Command = adapter, adapter.Players.Command
	}
	if container.State != "running" || (probe.Adapter == nil && probe.Query == nil) {
		return nil, nil
	}
	assertBinding := func() error {
		_, err := c.Identity.ResolveBinding(ctx, server.ID, server.BindingRevision)
		return err
	}
	reading := &Reading{ContainerID: container.ID, StartedAt: container.StateSince, Failed: true}
	result, err := c.Runtime.Probe(ctx, console.Target{ContainerID: container.ID, Labels: container.Labels, State: container.State},
		probe, assertBinding)
	// Work that needed the server cancelled this reading, or Ludock is
	// stopping: neither says anything about the game.
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil, errSkipped
	}
	reading.CheckedAt = time.Now().UnixMilli()
	if err != nil {
		return reading, nil
	}
	if probe.Adapter != nil && result.ResponseErr == nil {
		// Redacted before parsing, so no name carries a secret.
		var redacted strings.Builder
		redactor := console.NewRedactor(observed.Observation.Secrets, func(value string) { redacted.WriteString(value) })
		redactor.Push(result.Response)
		redactor.End()
		if players, ok := probe.Adapter.Players.Parse(redacted.String()); ok {
			reading.Players, reading.Failed, reading.Via = players, false, ViaConsole
		}
	}
	if status := result.Status; status != nil {
		reading.Answer, reading.AnswerAt = Silent, reading.CheckedAt
		if status.Answered {
			reading.Answer, reading.AnsweredAt = Answered, reading.CheckedAt
		}
		if reading.Failed && status.Answered && status.Counted {
			reading.Players = games.Players{Online: status.Online, Max: status.Max}
			reading.Failed, reading.Via = false, ViaQuery
		}
	}
	return reading, nil
}
