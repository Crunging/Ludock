package fakedocker

import (
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"time"
)

// fault fails (status) or stalls the next count requests whose
// "METHOD path" matches.
type fault struct {
	match  *regexp.Regexp
	status int
	stall  bool
	count  int
}

// AddFault fails the next count Engine API requests whose "METHOD path"
// matches pattern with status, or with stall leaves them unanswered until the
// client hangs up. A count below one means every match.
func (e *Engine) AddFault(pattern string, status int, stall bool, count int) error {
	match, err := regexp.Compile(pattern)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.faults = append(e.faults, &fault{match: match, status: status, stall: stall, count: count})
	return nil
}

// ClearFaults removes the delay and every fault.
func (e *Engine) ClearFaults() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Delay, e.faults = 0, nil
}

// faultLocked uses up and returns the first fault matching request, if any.
func (e *Engine) faultLocked(request string) *fault {
	for index, f := range e.faults {
		if !f.match.MatchString(request) {
			continue
		}
		if f.count > 0 {
			if f.count--; f.count == 0 {
				e.faults = slices.Delete(e.faults, index, index+1)
			}
		}
		return f
	}
	return nil
}

// Outage stops answering on the socket for duration, as a daemon that
// exits and is started again would: connections and streams close, helpers
// started with AutoRemove and every exec are gone, and new connections are refused until it
// is back. Other containers keep their state.
func (e *Engine) Outage(duration time.Duration) error {
	e.mu.Lock()
	// A restarted daemon knows none of its earlier execs.
	e.execs = map[string]*exec{}
	// Without live restore, Docker stops every container as it exits; those
	// created with AutoRemove are gone when it is back.
	for id, c := range e.containers {
		if autoRemove, _ := c.HostConfig["AutoRemove"].(bool); autoRemove {
			e.autoRemoveLocked(id)
		}
	}
	e.mu.Unlock()
	return e.Disconnect(duration)
}

// Disconnect closes every connection and stream and refuses new ones for
// duration while the daemon keeps its containers and execs, as when a proxy
// in front of the socket restarts.
func (e *Engine) Disconnect(duration time.Duration) error {
	e.mu.Lock()
	for _, events := range e.events {
		close(events)
	}
	e.events = nil
	server := e.server
	e.mu.Unlock()
	// Refuse requests before cutting streams, so a client that sees its
	// stream end can't reach the daemon in between.
	server.Close()
	os.Remove(e.Socket)
	e.mu.Lock()
	for conn := range e.streams {
		conn.Close()
	}
	e.mu.Unlock()
	time.Sleep(duration)
	listener, err := net.Listen("unix", e.Socket)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.listener = listener
	e.server = &http.Server{Handler: http.HandlerFunc(e.serve)}
	server = e.server
	e.mu.Unlock()
	go server.Serve(listener)
	return nil
}

// Refuse stops accepting connections for duration, as a daemon going down
// does just before it closes the ones it has: a request already being
// answered finishes, but its connection is not kept. It returns at once.
func (e *Engine) Refuse(duration time.Duration) {
	e.mu.Lock()
	listener, server := e.listener, e.server
	e.mu.Unlock()
	server.SetKeepAlivesEnabled(false)
	listener.Close()
	os.Remove(e.Socket)
	go func() {
		time.Sleep(duration)
		listener, err := net.Listen("unix", e.Socket)
		if err != nil {
			return
		}
		e.mu.Lock()
		e.listener = listener
		e.server = &http.Server{Handler: http.HandlerFunc(e.serve)}
		server := e.server
		e.mu.Unlock()
		server.Serve(listener)
	}()
}
