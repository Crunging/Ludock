// Package players reads who is online on running game servers through a
// Source, and keeps the latest reading of each in memory.
package players

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
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
	// Fresh is how long a reading counts toward Online.
	Fresh = 2 * Interval
)

// Reading is one server's players as Ludock last read them.
type Reading struct {
	// ContainerID and StartedAt are the container run it was read from; a
	// replaced or restarted container has no reading until the next one.
	ContainerID string
	StartedAt   int64
	games.Players
	// Failed: the source could not be read, or did not answer with a
	// player list.
	Failed    bool
	CheckedAt int64 // Unix ms
}

func (r *Reading) same(other *Reading) bool {
	return other != nil && r.ContainerID == other.ContainerID && r.StartedAt == other.StartedAt && r.Failed == other.Failed &&
		r.Online == other.Online && r.Max == other.Max && slices.Equal(r.Names, other.Names)
}

// A Source reads who is online on one server.
type Source interface {
	// Lists reports whether the source can read the container's players.
	Lists(container servers.Container) bool
	// Read returns a reading of the server's current container run, nil when
	// it no longer lists players, or an error when it took no reading.
	Read(ctx context.Context, server identity.Server) (*Reading, error)
}

type Service struct {
	Servers  *servers.Service
	Identity *identity.Store
	Source   Source
	// Changed is told when a server's reading changes. It must not block.
	Changed func(serverID string)

	mu       sync.Mutex
	readings map[string]*Reading
	reading  map[string]bool
	// again holds a Check that arrived during that server's reading, which
	// may have started before the change; it runs once the reading ends.
	again   map[string]identity.Server
	slots   chan struct{}
	work    sync.WaitGroup
	stopped bool
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

// Online is how many players a server's current container run has, and
// whether that is known: false until a reading succeeds, after one fails, or
// once the latest is older than Fresh.
func (s *Service) Online(serverID string, container servers.Container) (int, bool) {
	reading := s.Get(serverID, container)
	if reading == nil || reading.Failed || time.Since(time.UnixMilli(reading.CheckedAt)) > Fresh {
		return 0, false
	}
	return max(reading.Online, len(reading.Names)), true
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
			container.State != "running" || !s.Source.Lists(container) {
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
	if s.reading == nil {
		s.reading, s.readings, s.again, s.slots = map[string]bool{}, map[string]*Reading{}, map[string]identity.Server{},
			make(chan struct{}, parallel)
	}
	if s.stopped || s.reading[server.ID] {
		if !s.stopped && wait == nil {
			s.again[server.ID] = server
		}
		s.mu.Unlock()
		return
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
			next, again := s.again[server.ID]
			delete(s.again, server.ID)
			s.mu.Unlock()
			if again {
				s.start(ctx, next, nil)
			}
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
		reading, err := s.Source.Read(ctx, server)
		if err != nil {
			log.Debug("Skipped a player reading", logging.F("serverId", server.ID))
			return
		}
		if reading != nil {
			s.store(server.ID, reading)
		}
	}()
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
