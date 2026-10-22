// Package events turns Docker container events into per-viewer server
// change notifications. Only logical server IDs leave this package, and only
// after a fresh discovery snapshot and grant check.
package events

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/logging"
)

var log = logging.New("events")

// StateActions are the container events that change what a viewer sees.
var StateActions = map[string]bool{"create": true, "start": true, "stop": true, "die": true, "kill": true,
	"restart": true, "destroy": true, "rename": true, "pause": true, "unpause": true, "update": true, "oom": true,
	"health_status: healthy": true, "health_status: unhealthy": true, "health_status: starting": true}

// Event is a server change. Action "refresh" carries no server ID: it tells
// a former viewer to drop a server that disappeared or was suspended, and
// reveals nothing about any other server.
type Event struct {
	Action   string
	ServerID string
	Time     int64 // Unix seconds
}

// Subscriber receives events for one authenticated viewer.
type Subscriber struct {
	// Validate returns the current principal, or nil once the session is gone.
	Validate func(ctx context.Context) *db.SessionUser
	// Send must not block.
	Send func(Event)
	// Close ends the subscription after authentication expired.
	Close func(reason string)
}

// Hub streams Docker events while anyone listens.
type Hub struct {
	Docker   *docker.Client
	Identity *identity.Store
	Authz    *authz.Service
	// Refresh reconciles the fleet (servers.Service.Refresh).
	Refresh func(ctx context.Context) error

	mu          sync.Mutex
	subscribers map[*Subscriber]bool
	cancel      context.CancelFunc
	// streams counts running streams, including cancelled ones still
	// finishing a batch after their last viewer left.
	streams    sync.WaitGroup
	deliveries sync.WaitGroup
	stopped    bool
}

type dockerEvent struct {
	Action string `json:"Action"`
	Actor  struct {
		ID string `json:"ID"`
	} `json:"Actor"`
	ID   string   `json:"id"`
	Time *float64 `json:"time"`
}

type change struct {
	action, containerID string
	time                int64
}

func stateChange(event dockerEvent) (change, bool) {
	if !StateActions[event.Action] {
		return change{}, false
	}
	id := event.Actor.ID
	if id == "" {
		id = event.ID
	}
	if !docker.ContainerIDPattern.MatchString(id) {
		return change{}, false
	}
	when := time.Now().Unix()
	if event.Time != nil {
		when = int64(*event.Time)
	}
	return change{event.Action, id, when}, true
}

// Decode reads newline-delimited Docker events, skipping malformed records
// and failing on a record over 1 MiB.
func Decode(source io.Reader, deliver func(dockerEvent)) error {
	return docker.DecodeJSONLines(source, 1<<20, true, func(raw json.RawMessage) error {
		var event dockerEvent
		if json.Unmarshal(raw, &event) == nil && len(raw) > 0 && raw[0] == '{' {
			deliver(event)
		}
		return nil
	})
}

// Subscribe adds a viewer and starts the Docker stream if needed. The
// returned function removes it.
func (h *Hub) Subscribe(subscriber *Subscriber) func() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subscribers == nil {
		h.subscribers = map[*Subscriber]bool{}
	}
	h.subscribers[subscriber] = true
	if h.cancel == nil && !h.stopped {
		h.startLocked()
	}
	return func() {
		h.mu.Lock()
		delete(h.subscribers, subscriber)
		empty := len(h.subscribers) == 0
		cancel := h.cancel
		if empty {
			h.cancel = nil
		}
		h.mu.Unlock()
		if empty && cancel != nil {
			cancel()
		}
	}
}

func (h *Hub) startLocked() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.streams.Add(1)
	go func() {
		defer h.streams.Done()
		h.run(ctx)
	}()
}

// run follows the Docker event stream, reconnecting after failures. Bursts
// are coalesced: only state actions are queued, at most one per container,
// and each drained batch shares one discovery refresh.
func (h *Hub) run(ctx context.Context) {
	for ctx.Err() == nil {
		stream, err := h.Docker.Events(ctx, docker.Filters{"type": {"container"}})
		delay := 5 * time.Second
		if err == nil {
			pending := map[string]change{}
			var order []string
			var mu sync.Mutex
			wake := make(chan struct{}, 1)
			drained := make(chan struct{})
			go func() {
				defer close(drained)
				for {
					select {
					case <-ctx.Done():
						return
					case _, ok := <-wake:
						if !ok {
							return
						}
					}
					for {
						mu.Lock()
						batch := make([]change, 0, len(order))
						for _, id := range order {
							batch = append(batch, pending[id])
						}
						pending, order = map[string]change{}, nil
						mu.Unlock()
						if len(batch) == 0 {
							break
						}
						if err := h.dispatch(ctx, batch); err != nil && ctx.Err() == nil {
							log.Warn("Unable to refresh Docker event state")
						}
					}
				}
			}()
			decodeErr := Decode(stream, func(event dockerEvent) {
				c, ok := stateChange(event)
				if !ok {
					return
				}
				mu.Lock()
				if _, exists := pending[c.containerID]; !exists {
					if len(order) >= 1024 {
						mu.Unlock()
						return
					}
					order = append(order, c.containerID)
				}
				pending[c.containerID] = c
				mu.Unlock()
				select {
				case wake <- struct{}{}:
				default:
				}
			})
			stream.Close()
			close(wake)
			<-drained
			if decodeErr == nil {
				delay = 2 * time.Second
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// dispatch delivers a batch of changes after one refresh.
func (h *Hub) dispatch(ctx context.Context, changes []change) error {
	h.deliveries.Add(1)
	defer h.deliveries.Done()
	before, err := h.Identity.List(ctx, "")
	if err != nil {
		return err
	}
	h.mu.Lock()
	subscribers := make([]*Subscriber, 0, len(h.subscribers))
	for subscriber := range h.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	h.mu.Unlock()
	previouslyVisible := make([]map[*Subscriber]bool, len(changes))
	for index, c := range changes {
		previouslyVisible[index] = map[*Subscriber]bool{}
		previous := findByContainer(before, c.containerID)
		if previous == nil {
			continue
		}
		for _, subscriber := range subscribers {
			if visible, _ := h.Authz.Has(ctx, subscriber.Validate(ctx), previous.ID, authz.ServerView); visible {
				previouslyVisible[index][subscriber] = true
			}
		}
	}
	if err := h.Refresh(ctx); err != nil {
		return err
	}
	after, err := h.Identity.List(ctx, "")
	if err != nil {
		return err
	}
	for index, c := range changes {
		current := findByContainer(after, c.containerID)
		for _, subscriber := range subscribers {
			user, err := h.Authz.CurrentActor(ctx, subscriber.Validate(ctx))
			if err != nil {
				continue
			}
			if user == nil {
				subscriber.Close("Authentication expired")
				continue
			}
			visible := false
			if current != nil {
				visible, _ = h.Authz.Has(ctx, user, current.ID, authz.ServerView)
			}
			if !visible && !previouslyVisible[index][subscriber] {
				continue
			}
			if visible {
				subscriber.Send(Event{Action: c.action, ServerID: current.ID, Time: c.time})
			} else {
				subscriber.Send(Event{Action: "refresh", Time: c.time})
			}
		}
	}
	return nil
}

func findByContainer(servers []identity.Server, containerID string) *identity.Server {
	for index := range servers {
		if servers[index].ContainerID == containerID {
			return &servers[index]
		}
	}
	return nil
}

// Stop ends the stream and waits for in-flight deliveries, which still use
// the database for authorization.
func (h *Hub) Stop() {
	h.mu.Lock()
	h.stopped = true
	cancel := h.cancel
	h.cancel = nil
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// No stream starts once stopped; every dispatch runs inside one.
	h.streams.Wait()
	h.deliveries.Wait()
}
