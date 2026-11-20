// Package players reads who is online on running game servers through a
// Source, and keeps the latest reading of each in memory, along with
// whether the game answers its status query.
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
	// IdleInterval is how often a monitored game's status is read while
	// nobody watches, since each reading starts a helper container.
	IdleInterval = 2 * time.Minute
	early        = 5 * time.Second
	// readLimit bounds one reading, including a console helper's setup.
	readLimit = 30 * time.Second
	// parallel readings across the fleet.
	parallel = 4
	// Fresh is how long a reading counts toward Online.
	Fresh = 2 * Interval
	// answerFresh is how long a game's answer counts, which spans an idle
	// poll and the tick it waits for.
	answerFresh = IdleInterval + Interval
)

// Reading is one server's players as Ludock last read them.
type Reading struct {
	// ContainerID and StartedAt are the container run it was read from; a
	// replaced or restarted container has no reading until the next one.
	ContainerID string
	StartedAt   int64
	games.Players
	// Failed: the source could not be read, or did not answer with a
	// player list or count.
	Failed bool
	// Via is how the players were read.
	Via       Via
	CheckedAt int64 // Unix ms
	// Answer is the latest status query outcome in this container run, from
	// AnswerAt; AnsweredAt is the last time the game answered, or 0.
	Answer     Answer
	AnswerAt   int64
	AnsweredAt int64
	// listed is the console's list as the game sent it, before redaction,
	// while the names came from the console: a status query naming the
	// same players vouches that they are still current.
	listed *[]string
	// partial: only the status query was asked, though the console lists
	// players.
	partial bool
}

type Via string

const (
	ViaConsole Via = "console"
	ViaQuery   Via = "query"
)

type Answer int8

const (
	// NotAsked: no status query ran, which says nothing about the game.
	NotAsked Answer = iota
	Answered
	Silent
)

func (r *Reading) same(other *Reading) bool {
	return other != nil && r.ContainerID == other.ContainerID && r.StartedAt == other.StartedAt && r.Failed == other.Failed &&
		r.Online == other.Online && r.Max == other.Max && slices.Equal(r.Names, other.Names) && slices.Equal(r.IDs, other.IDs) &&
		r.Via == other.Via &&
		r.Answer == other.Answer && (r.AnsweredAt == 0) == (other.AnsweredAt == 0)
}

// Game is whether a game answers its status query.
type Game int8

const (
	// NotChecked: the game has no status query.
	NotChecked Game = iota
	// Waiting: the game hasn't answered yet in this container run, such as
	// while it starts, or because its query port is wrong or off.
	Waiting
	Answering
	// NotAnswering: the game answered in this run, then stopped.
	NotAnswering
	// Unknown: the game answered in this run, but no recent query says
	// whether it still does.
	Unknown
)

// Game reports whether a server's current container run answers its status
// query, and since when it last did.
func (s *Service) Game(serverID string, container servers.Container) (Game, int64) {
	if container.Query == nil || container.State != "running" {
		return NotChecked, 0
	}
	reading := s.Get(serverID, container)
	if reading == nil || reading.AnsweredAt == 0 {
		return Waiting, 0
	}
	if reading.Answer == NotAsked || time.Since(time.UnixMilli(reading.AnswerAt)) > answerFresh {
		return Unknown, reading.AnsweredAt
	}
	if reading.Answer == Silent {
		return NotAnswering, reading.AnsweredAt
	}
	return Answering, reading.AnsweredAt
}

// A Source reads who is online on one server.
type Source interface {
	// Lists reports whether the source can read the container's players.
	Lists(container servers.Container) bool
	// Read returns a reading of the server's current container run, nil when
	// it no longer lists players, or an error when it took no reading.
	Read(ctx context.Context, server identity.Server, ask Ask) (*Reading, error)
}

// Ask is what a reading asks for.
type Ask struct {
	// Players asks the console for the list too; otherwise only the status
	// query is asked, which never reaches the game's console or its log.
	Players bool
	// Previous is the server's latest reading, from any container run, or
	// nil.
	Previous *Reading
	// Fresh asks the console even when the query names the same players,
	// as after a command that may have changed them, whose effect the
	// game's status can show a few seconds late.
	Fresh bool
}

type Service struct {
	Servers  *servers.Service
	Identity *identity.Store
	Source   Source
	// Monitored lists the servers whose availability is monitored, which
	// are read even while nobody watches. It may be nil.
	Monitored func(ctx context.Context) map[string]bool
	// Changed is told when a server's reading changes. It must not block.
	Changed func(serverID string)

	mu       sync.Mutex
	readings map[string]*Reading
	reading  map[string]bool
	// again holds a Check that arrived during that server's reading, which
	// may have started before the change; it runs once the reading ends.
	again map[string]identity.Server
	// waiters closes when the next fresh reading of its server, one that
	// starts after it was made, ends; every caller waiting meanwhile shares
	// it.
	waiters map[string]chan struct{}
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
	online, _, known := s.OnlineAt(serverID, container)
	return online, known
}

// OnlineAt is Online with when that reading was taken, in Unix milliseconds.
func (s *Service) OnlineAt(serverID string, container servers.Container) (int, int64, bool) {
	reading := s.Get(serverID, container)
	if reading == nil || reading.Failed || time.Since(time.UnixMilli(reading.CheckedAt)) > Fresh {
		return 0, 0, false
	}
	return reading.Online, reading.CheckedAt, true
}

// ListedAt is OnlineAt without a query-only count the console could have
// listed, for a schedule waiting for an empty server, which goes by its own
// console checks.
func (s *Service) ListedAt(serverID string, container servers.Container) (int, int64, bool) {
	reading := s.Get(serverID, container)
	if reading == nil || reading.Failed || reading.partial || time.Since(time.UnixMilli(reading.CheckedAt)) > Fresh {
		return 0, 0, false
	}
	return reading.Online, reading.CheckedAt, true
}

// Poll starts a reading for every running server that is due: those that
// list players while an open page shows them (watched, which may be nil for
// none), and monitored ones whose game answers a status query, less often
// and with the query alone otherwise. It forgets servers that stopped, and
// waits for the readings.
func (s *Service) Poll(ctx context.Context, watched func(serverID string) bool) {
	var monitored map[string]bool
	if s.Monitored != nil {
		monitored = s.Monitored(ctx)
	}
	if watched == nil && len(monitored) == 0 {
		return
	}
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
		if server.Status != identity.StatusActive || server.ContainerID == "" || !found ||
			container.State != "running" || !s.Source.Lists(container) {
			continue
		}
		listed[server.ID] = true
		watched := watched != nil && watched(server.ID)
		due := Interval
		if !watched {
			due = IdleInterval
		}
		// A game whose health check is still starting isn't taking players.
		if container.Health == "starting" || !(watched || (monitored[server.ID] && container.Query != nil)) {
			continue
		}
		// A query-only reading doesn't stand in for the list a page shows.
		if reading := s.Get(server.ID, container); reading != nil && now.Sub(time.UnixMilli(reading.CheckedAt)) < due-early &&
			!(watched && reading.partial) {
			continue
		}
		s.start(ctx, server, watched, false, &wait)
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
	s.start(ctx, *server, true, true, nil)
}

// CheckAndWait is Check, returning once a reading that started after the
// call ends, when ctx ends, or after limit.
func (s *Service) CheckAndWait(ctx context.Context, serverID string, limit time.Duration) {
	server, err := s.Identity.ResolveBinding(ctx, serverID, -1)
	if err != nil {
		return
	}
	s.mu.Lock()
	if s.waiters == nil {
		s.waiters = map[string]chan struct{}{}
	}
	done := s.waiters[serverID]
	if done == nil {
		done = make(chan struct{})
		s.waiters[serverID] = done
	}
	s.mu.Unlock()
	s.start(context.WithoutCancel(ctx), *server, true, true, nil)
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-done:
	case <-ctx.Done():
	case <-timer.C:
	}
}

// Stop waits for readings to finish; none start after it.
func (s *Service) Stop() {
	s.mu.Lock()
	s.stopped = true
	for id, waiter := range s.waiters {
		close(waiter)
		delete(s.waiters, id)
	}
	s.mu.Unlock()
	s.work.Wait()
}

func (s *Service) start(ctx context.Context, server identity.Server, players, fresh bool, wait *sync.WaitGroup) {
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
	ask := Ask{Players: players, Previous: s.readings[server.ID], Fresh: fresh}
	var waiter chan struct{}
	if players && fresh {
		waiter = s.waiters[server.ID]
		delete(s.waiters, server.ID)
	}
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
			if waiter != nil {
				close(waiter)
			}
			if again {
				s.start(ctx, next, true, true, nil)
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
		reading, err := s.Source.Read(ctx, server, ask)
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
	// A reading without a query outcome keeps the last one of the same run.
	if previous != nil && previous.ContainerID == reading.ContainerID && previous.StartedAt == reading.StartedAt {
		if reading.Answer == NotAsked {
			reading.Answer, reading.AnswerAt = previous.Answer, previous.AnswerAt
		}
		reading.AnsweredAt = max(reading.AnsweredAt, previous.AnsweredAt)
	}
	s.readings[serverID] = reading
	s.mu.Unlock()
	if !reading.same(previous) && s.Changed != nil {
		s.Changed(serverID)
	}
}
