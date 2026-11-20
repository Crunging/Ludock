package games

import (
	"strconv"
	"strings"

	"github.com/Crunging/Ludock/internal/gamequery"
)

const (
	// LabelQuery picks the status query protocol (minecraft-ping or a2s), or
	// turns it off with disabled or none.
	LabelQuery = "ludock.query"
	// LabelQueryPort is the status query port inside the container.
	LabelQueryPort = "ludock.query.port"
)

// StatusQuery is how Ludock asks a game whether it answers.
type StatusQuery struct {
	Protocol gamequery.Protocol
	// Port is the default port inside the container.
	Port int
	// PortEnv names environment variables the image documents for that
	// port; the first one set wins over Port.
	PortEnv []string
	// OffEnv is a variable whose OffValues, in any case, keep the game from
	// answering, and OffReason says so in plain words.
	OffEnv    string
	OffValues []string
	OffReason string
}

// Off is why the container's environment keeps the game from answering the
// query, or "".
func (q *StatusQuery) Off(env map[string]string) string {
	value := strings.TrimSpace(env[q.OffEnv])
	for _, off := range q.OffValues {
		if q.OffEnv != "" && strings.EqualFold(value, off) {
			return q.OffReason
		}
	}
	return ""
}

// QueryDisabled reports whether a label turns the status check off.
func QueryDisabled(labels map[string]string) bool {
	configured := strings.ToLower(strings.TrimSpace(labels[LabelQuery]))
	return configured == "disabled" || configured == "none"
}

var queryNames = map[gamequery.Protocol]string{gamequery.MinecraftPing: "Server List Ping", gamequery.ValveA2S: "Steam server query"}

// Name is the protocol's name as pages show it.
func (q *StatusQuery) Name() string { return queryNames[q.Protocol] }

// ResolveQuery is the status query for a game type and its approved labels,
// or nil when there is none or a label turns it off or is invalid. A label
// naming a protocol enables it for any game, which then needs a port.
func ResolveQuery(gameType string, labels map[string]string) *StatusQuery {
	if QueryDisabled(labels) {
		return nil
	}
	configured := strings.ToLower(strings.TrimSpace(labels[LabelQuery]))
	var query StatusQuery
	if integration := Lookup(gameType); integration != nil && integration.Query != nil {
		query = *integration.Query
	}
	if configured != "" {
		protocol := gamequery.Protocol(configured)
		if queryNames[protocol] == "" {
			return nil
		}
		if protocol != query.Protocol {
			query = StatusQuery{Protocol: protocol}
		}
	}
	if raw, present := labels[LabelQueryPort]; present {
		port, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || port < 1 || port > 65535 {
			return nil
		}
		query.Port, query.PortEnv = port, nil
	}
	if query.Protocol == "" || (query.Port == 0 && len(query.PortEnv) == 0) {
		return nil
	}
	return &query
}

// QueryPort is the port to query given the container's environment, or 0.
func (q *StatusQuery) QueryPort(env map[string]string) int {
	for _, name := range q.PortEnv {
		if raw := strings.TrimSpace(env[name]); raw != "" {
			if port, err := strconv.Atoi(raw); err == nil && port >= 1 && port <= 65535 {
				return port
			}
			return 0
		}
	}
	return q.Port
}
