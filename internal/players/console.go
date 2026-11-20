package players

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Crunging/Ludock/internal/console"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/servers"
)

// GameSource asks the game itself: its console's list command, with the
// console's own helper and password, and its status query, in the same
// helper. A console list wins over a query's count, since it names players,
// and a query that names the same players as the last list stands in for
// the command, which games such as Minecraft log on every connection.
type GameSource struct {
	Servers *servers.Service
	Runtime *console.Runtime
	Locks   *locks.Table
}

func (c *GameSource) Lists(container servers.Container) bool {
	return (container.Console != nil && container.Console.ListsPlayers) || container.Query != nil
}

var errSkipped = errors.New("player reading skipped")

// Read takes one reading under the server lock, as background work that
// anything needing the server cancels.
func (c *GameSource) Read(ctx context.Context, server identity.Server, ask Ask) (*Reading, error) {
	ctx, cancel := context.WithTimeout(ctx, readLimit)
	defer cancel()
	release, err := c.Locks.Acquire([]string{locks.ServerKey(server.ID)}, cancel)
	if err != nil {
		return nil, err
	}
	// Held until the console helper, if any, is removed.
	defer release()
	observed, assertBinding, err := c.Servers.Observe(ctx, server)
	if err != nil {
		return nil, err
	}
	container := observed.Container
	adapter := games.ResolveAdapter(container.GameType, container.Labels)
	probe := console.Probe{Query: container.Query}
	lists := adapter != nil && adapter.Players != nil
	if lists && ask.Players {
		probe.Adapter, probe.Command = adapter, adapter.Players.Command
	}
	if container.State != "running" || (!lists && probe.Query == nil) {
		return nil, nil
	}
	if probe.Adapter == nil && probe.Query == nil {
		return nil, errSkipped
	}
	previous := ask.Previous
	if previous != nil && (previous.ContainerID != container.ID || previous.StartedAt != container.StateSince) {
		previous = nil
	}
	// A game that answered its query last time is asked it before the
	// command; one that didn't would hold the command up for the query's
	// whole timeout.
	if !ask.Fresh && previous != nil && previous.listed != nil && (probe.Adapter == nil || previous.Answer == Answered) {
		probe.Known = previous.listed
	}
	reading := &Reading{ContainerID: container.ID, StartedAt: container.StateSince, Failed: true,
		partial: lists && probe.Adapter == nil}
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
	if result.Unchanged {
		reading.Players = games.Players{Online: result.Status.Online, Max: previous.Max, Names: slices.Clone(previous.Names),
			IDs: slices.Clone(previous.IDs)}
		if result.Status.Max > 0 {
			reading.Max = result.Status.Max
		}
		reading.Failed, reading.Via, reading.listed, reading.partial = false, ViaConsole, previous.listed, false
	} else if probe.Adapter != nil && result.ResponseErr == nil {
		// Parsed raw, since a short secret can occur inside a player ID that
		// the strict parsers check; the names are what is kept, so they are
		// redacted instead.
		if players, ok := probe.Adapter.Players.Parse(result.Response); ok {
			// Never nil, so an empty list is still a known one.
			listed := append([]string{}, players.Names...)
			reading.listed = &listed
			secrets := append(slices.Clone(observed.Observation.Secrets), result.Secrets...)
			for index, name := range players.Names {
				var safe strings.Builder
				redactor := console.NewRedactor(secrets, func(part string) { safe.WriteString(part) })
				redactor.Push(name)
				// A name cut to the limit may end partway into a secret, so
				// that tail stays withheld.
				if utf8.RuneCountInString(name) < games.MaxNameRunes {
					redactor.End()
				}
				// A name that held a secret is no longer the account name a
				// command could take.
				if safe.String() != name && index < len(players.IDs) && players.IDs[index] == name {
					players.IDs[index] = ""
				}
				players.Names[index] = safe.String()
			}
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
