// Package gamequery asks a game server for its status over the game's own
// query protocol: Minecraft's Server List Ping and Valve's A2S_INFO. Every
// reply is untrusted: anything malformed, oversized, or unexpected is no
// answer, never a guess. The same code runs in the network console helper.
package gamequery

import (
	"context"
	"errors"
	"time"
)

type Protocol string

const (
	MinecraftPing Protocol = "minecraft-ping"
	ValveA2S      Protocol = "a2s"
)

// Timeout bounds one query, including retries.
const Timeout = 5 * time.Second

// maxCount bounds player counts, which no real server approaches.
const maxCount = 1_000_000

// Status is what a game said about itself.
type Status struct {
	// Counted: the reply included how many are online.
	Counted bool
	Online  int
	Max     int // 0 when the game doesn't say
	// Listed: Names is everyone online, as when nobody is, or Minecraft's
	// player sample holds every player.
	Listed bool     `json:",omitempty"`
	Names  []string `json:",omitempty"`
}

// maxListed bounds the names a reply may list.
const maxListed = 100

// Lists reports whether the status names exactly these players, in any
// order.
func (s Status) Lists(names []string) bool {
	if !s.Listed || len(s.Names) != len(names) {
		return false
	}
	count := map[string]int{}
	for _, name := range names {
		count[name]++
	}
	for _, name := range s.Names {
		if count[name]--; count[name] < 0 {
			return false
		}
	}
	return true
}

// ErrNoAnswer is a game that didn't answer, or answered with something
// that isn't a valid status reply.
var ErrNoAnswer = errors.New("the game did not answer its status query")

// Query asks the game on host and port. authorize, when set, runs before
// every write, and its error is returned as is; any other failure is
// ErrNoAnswer.
func Query(ctx context.Context, protocol Protocol, host string, port int, authorize func() error) (Status, error) {
	if authorize == nil {
		authorize = func() error { return nil }
	}
	if port < 1 || port > 65535 {
		return Status{}, errors.New("invalid status query port")
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	switch protocol {
	case MinecraftPing:
		return minecraft(ctx, host, port, authorize)
	case ValveA2S:
		return a2s(ctx, host, port, authorize)
	}
	return Status{}, errors.New("unsupported status query protocol")
}

func counts(online, capacity int64) (Status, bool) {
	if online < 0 || capacity < 0 || online > maxCount || capacity > maxCount {
		return Status{}, false
	}
	return Status{Counted: true, Online: int(online), Max: int(capacity), Listed: online == 0}, true
}
