// Package players reads who is online on running game servers whose console
// can list them, and keeps the latest reading of each in memory.
package players

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/console"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/servers"
)

var log = logging.New("players")

const (
	// Interval is how often a running server's players are read. The tick
	// that polls runs every 15 s, so a reading this close to due is taken.
	Interval = 30 * time.Second
	early    = 5 * time.Second
	// readLimit bounds one reading, including a console helper's setup.
	readLimit = 30 * time.Second
	// parallel readings across the fleet.
	parallel = 4
)

// Reading is one server's players as Ludock last read them.
type Reading struct {
	// ContainerID and StartedAt are the container run it was read from; a
	// replaced or restarted container has no reading until the next one.
	ContainerID string
	StartedAt   int64
	games.Players
	// Failed: the console could not be read, or did not answer with a
	// player list.
	Failed    bool
	CheckedAt int64 // Unix ms
}

func (r *Reading) same(other *Reading) bool {
	return other != nil && r.ContainerID == other.ContainerID && r.StartedAt == other.StartedAt && r.Failed == other.Failed &&
		r.Online == other.Online && r.Max == other.Max && slices.Equal(r.Names, other.Names)
}

// Service polls players.
type Service struct {
	Servers  *servers.Service
	Identity *identity.Store
	Observer *servers.Observer
	Runtime  *console.Runtime
	Locks    *locks.Table
	// Changed is told when a server's reading changes. It must not block.
	Changed func(serverID string)

	mu       sync.Mutex
	readings map[string]*Reading
	reading  map[string]bool
	slots    chan struct{}
	work     sync.WaitGroup
	stopped  bool
}

// Get is the latest reading of a server's current container run, or nil.
func (s *Service) Get(serverID string, container servers.Container) *Reading {
	s.mu.Lock()
	defer s.mu.Unlock()
	reading := s.readings[serverID]
	if reading == nil || reading.ContainerID != container.ID || reading.StartedAt != container.StateSince {
		return nil
	}
	return reading
}

// Poll starts a reading for every running server that lists players and is
// due, and forgets servers that stopped. It waits for the readings.
func (s *Service) Poll(ctx context.Context) {
	current, err := s.Servers.Refresh(ctx)
	if err != nil {
		return
	}
	logical, err := s.Identity.List(ctx, "")
	if err != nil {
		return
	}
	now := time.Now()
	listed := map[string]bool{}
	var wait sync.WaitGroup
	for _, server := range logical {
		container, found := current[server.ContainerID]
		if server.Status != identity.StatusActive || server.ReviewRequired || server.ContainerID == "" || !found ||
			container.State != "running" || container.Console == nil || !container.Console.ListsPlayers {
			continue
		}
		listed[server.ID] = true
		// A game whose health check is still starting isn't taking players.
		if container.Health == "starting" {
			continue
		}
		if reading := s.Get(server.ID, container); reading != nil && now.Sub(time.UnixMilli(reading.CheckedAt)) < Interval-early {
			continue
		}
		s.start(ctx, server, &wait)
	}
	s.mu.Lock()
	for id := range s.readings {
		if !listed[id] {
			delete(s.readings, id)
		}
	}
	s.mu.Unlock()
	wait.Wait()
}

// Check reads a server's players now, such as after a console command that
// may have changed them. It returns without waiting.
func (s *Service) Check(ctx context.Context, serverID string) {
	server, err := s.Identity.ResolveBinding(ctx, serverID, -1)
	if err != nil {
		return
	}
	s.start(ctx, *server, nil)
}

// Stop waits for readings to finish; none start after it.
func (s *Service) Stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.work.Wait()
}

func (s *Service) start(ctx context.Context, server identity.Server, wait *sync.WaitGroup) {
	s.mu.Lock()
	if s.stopped || s.reading[server.ID] {
		s.mu.Unlock()
		return
	}
	if s.reading == nil {
		s.reading, s.readings, s.slots = map[string]bool{}, map[string]*Reading{}, make(chan struct{}, parallel)
	}
	s.reading[server.ID] = true
	s.work.Add(1)
	if wait != nil {
		wait.Add(1)
	}
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.reading, server.ID)
			s.mu.Unlock()
			if wait != nil {
				wait.Done()
			}
			s.work.Done()
		}()
		select {
		case s.slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-s.slots }()
		reading, err := s.read(ctx, server)
		if err != nil {
			log.Debug("Skipped a player reading", logging.F("serverId", server.ID))
			return
		}
		if reading != nil {
			s.store(server.ID, reading)
		}
	}()
}

var errSkipped = errors.New("player reading skipped")

// read takes one reading under the server lock, as background work that
// anything needing the server cancels. It returns nil when the server no
// longer lists players.
func (s *Service) read(ctx context.Context, server identity.Server) (*Reading, error) {
	ctx, cancel := context.WithTimeout(ctx, readLimit)
	defer cancel()
	release, err := s.Locks.Acquire([]string{"server:" + server.ID}, cancel)
	if err != nil {
		return nil, err
	}
	// Held until the console helper, if any, is removed.
	defer release()
	observed, _, err := s.Observer.Inspect(ctx, server.ContainerID)
	if err != nil {
		return nil, err
	}
	if _, err := s.Identity.AssertObservedBinding(ctx, server.ID, observed.Observation, server.BindingRevision); err != nil {
		return nil, err
	}
	container := observed.Container
	adapter := games.ResolveAdapter(container.GameType, container.Labels)
	if container.State != "running" || adapter == nil || adapter.Players == nil {
		return nil, nil
	}
	assertBinding := func() error {
		_, err := s.Identity.ResolveBinding(ctx, server.ID, server.BindingRevision)
		return err
	}
	reading := &Reading{ContainerID: container.ID, StartedAt: container.StateSince}
	response, err := s.Runtime.Query(ctx, console.Target{ContainerID: container.ID, Labels: container.Labels, State: container.State},
		adapter, adapter.Players.Command, assertBinding)
	// Work that needed the server cancelled this reading, or Ludock is
	// stopping: neither says anything about the console.
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil, errSkipped
	}
	if err == nil {
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

func (s *Service) store(serverID string, reading *Reading) {
	s.mu.Lock()
	previous := s.readings[serverID]
	s.readings[serverID] = reading
	s.mu.Unlock()
	if !reading.same(previous) && s.Changed != nil {
		s.Changed(serverID)
	}
}
