// Package monitor tracks server availability and notifies about outages
// that outlast their grace period.
package monitor

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/servers"
)

var log = logging.New("monitor")

// Policy is a server's availability policy.
type Policy struct {
	Enabled      bool `json:"enabled"`
	Maintenance  bool `json:"maintenance"`
	GraceSeconds int  `json:"graceSeconds"`
}

// State is the persisted outage state.
type State struct {
	OutageStartedAt      int64 // 0 when none
	Notified             bool
	SuppressedUntil      int64
	IntentionallyStopped bool
	LastState            string
}

// Availability is a policy and its state.
type Availability struct {
	Policy Policy
	State  State
}

// Service persists availability.
type Service struct {
	DB     *db.DB
	Locks  *locks.Table
	Notify func(key, message string)
	Store  *identity.Store
}

// ParsePolicy validates a policy, applying defaults.
func ParsePolicy(raw []byte) (Policy, error) {
	var input struct {
		Enabled      *bool    `json:"enabled"`
		Maintenance  *bool    `json:"maintenance"`
		GraceSeconds *float64 `json:"graceSeconds"`
	}
	decoder := json.NewDecoder(bytesReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.Enabled == nil {
		return Policy{}, apperr.New("INVALID_REQUEST", 400, "Invalid request")
	}
	policy := Policy{Enabled: *input.Enabled, GraceSeconds: 120}
	if input.Maintenance != nil {
		policy.Maintenance = *input.Maintenance
	}
	if input.GraceSeconds != nil {
		grace := *input.GraceSeconds
		if grace != float64(int(grace)) || grace < 10 || grace > 86400 {
			return Policy{}, apperr.New("INVALID_REQUEST", 400, "Invalid request")
		}
		policy.GraceSeconds = int(grace)
	}
	return policy, nil
}

type row struct {
	policy                string
	outage                sql.NullInt64
	notified, intentional int
	suppressed            int64
	lastState             sql.NullString
}

func (s *Service) row(ctx context.Context, serverID string) (*row, error) {
	var r row
	err := s.DB.QueryRowContext(ctx, `SELECT policy_json,outage_started_at,notified,suppressed_until,intentionally_stopped,last_state
    FROM availability WHERE server_id=?`, serverID).Scan(&r.policy, &r.outage, &r.notified, &r.suppressed, &r.intentional, &r.lastState)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

// Get reads a server's availability.
func (s *Service) Get(ctx context.Context, serverID string) (*Availability, error) {
	r, err := s.row(ctx, serverID)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return &Availability{Policy: Policy{GraceSeconds: 120}}, nil
	}
	policy, err := ParsePolicy([]byte(r.policy))
	if err != nil {
		return nil, err
	}
	return &Availability{Policy: policy, State: State{OutageStartedAt: r.outage.Int64, Notified: r.notified == 1,
		SuppressedUntil: r.suppressed, IntentionallyStopped: r.intentional == 1, LastState: r.lastState.String}}, nil
}

// Configure saves a policy and resets any outage. The administrator's
// session and role are checked again in the transaction that saves it.
func (s *Service) Configure(ctx context.Context, adminID, tokenHash, serverID string, policy Policy) (*Availability, error) {
	encoded, _ := json.Marshal(policy)
	if err := s.DB.AdminMutation(ctx, adminID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO availability(server_id,policy_json,updated_at) VALUES(?,?,?)
      ON CONFLICT(server_id) DO UPDATE SET policy_json=excluded.policy_json,updated_at=excluded.updated_at, outage_started_at=NULL, notified=0`,
			serverID, string(encoded), db.Now())
		return err
	}); err != nil {
		return nil, err
	}
	return s.Get(ctx, serverID)
}

// ensureRow adds the default policy only if none exists, so it never
// replaces one saved concurrently.
func (s *Service) ensureRow(ctx context.Context, serverID string) error {
	encoded, _ := json.Marshal(Policy{GraceSeconds: 120})
	_, err := s.DB.ExecContext(ctx, "INSERT INTO availability(server_id,policy_json,updated_at) VALUES(?,?,?) ON CONFLICT(server_id) DO NOTHING",
		serverID, string(encoded), db.Now())
	return err
}

// SetIntentionalStop records that a stop was requested, or cleared.
func (s *Service) SetIntentionalStop(ctx context.Context, serverID string, stopped bool) error {
	if err := s.ensureRow(ctx, serverID); err != nil {
		return err
	}
	value := 0
	if stopped {
		value = 1
	}
	_, err := s.DB.ExecContext(ctx, "UPDATE availability SET intentionally_stopped=?,outage_started_at=NULL,notified=0,updated_at=? WHERE server_id=?",
		value, db.Now(), serverID)
	return err
}

// Suppress pauses outage tracking for one grace period after Ludock acted
// on the server.
func (s *Service) Suppress(ctx context.Context, serverID string) error {
	if err := s.ensureRow(ctx, serverID); err != nil {
		return err
	}
	availability, err := s.Get(ctx, serverID)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, "UPDATE availability SET suppressed_until=?,outage_started_at=NULL,notified=0 WHERE server_id=?",
		db.Now()+int64(availability.Policy.GraceSeconds)*1000, serverID)
	return err
}

// suppressed fails if active work cannot be read, rather than treating the
// server as idle.
func (s *Service) suppressed(ctx context.Context, serverID string, policy Policy, until, now int64) (bool, error) {
	if !policy.Enabled || policy.Maintenance || until > now || (s.Locks != nil && s.Locks.ServerBusy(serverID)) {
		return true, nil
	}
	var id string
	err := s.DB.QueryRowContext(ctx, "SELECT id FROM operations WHERE server_id=? AND status IN ('queued','running') LIMIT 1", serverID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Problem is a current outage.
type Problem struct {
	State           string
	OutageStartedAt int64
}

// Problem reads the persisted outage without running checks. Policy and
// operation suppression are current even between monitor ticks.
func (s *Service) Problem(ctx context.Context, serverID string, now int64) (*Problem, error) {
	availability, err := s.Get(ctx, serverID)
	if err != nil {
		return nil, err
	}
	state := availability.State
	suppressed, err := s.suppressed(ctx, serverID, availability.Policy, state.SuppressedUntil, now)
	if err != nil {
		return nil, err
	}
	if suppressed || state.IntentionallyStopped ||
		state.OutageStartedAt == 0 || now-state.OutageStartedAt < int64(availability.Policy.GraceSeconds)*1000 {
		return nil, nil
	}
	last := state.LastState
	if last == "" {
		last = "unavailable"
	}
	return &Problem{State: last, OutageStartedAt: state.OutageStartedAt}, nil
}

func (s *Service) notify(key, message string) {
	if s.Notify != nil {
		s.Notify(key, message)
	}
}

// Check runs one monitor tick. current is the discovery result of the same
// tick, or nil when Docker is unreachable: an outage of the daemon is not
// evidence that containers vanished, so bindings are kept but availability
// is reported as unverifiable. Only state transitions are written.
func (s *Service) Check(ctx context.Context, current map[string]servers.Container, now int64) error {
	unavailable := current == nil
	logical, err := s.Store.List(ctx, "")
	if err != nil {
		return err
	}
	for _, server := range logical {
		r, err := s.row(ctx, server.ID)
		if err != nil {
			return err
		}
		if r == nil {
			continue
		}
		policy, err := ParsePolicy([]byte(r.policy))
		if err != nil {
			continue
		}
		suppressed, err := s.suppressed(ctx, server.ID, policy, r.suppressed, now)
		if err != nil {
			return err
		}
		if suppressed {
			continue
		}
		container, found := current[server.ContainerID]
		running := server.Status == identity.StatusActive && found && server.ContainerID != "" && container.State == "running"
		// An owner can start the container outside Ludock. Resume monitoring
		// even if startup never passes its health check.
		if running && r.intentional == 1 {
			if _, err := s.DB.ExecContext(ctx, "UPDATE availability SET intentionally_stopped=0 WHERE server_id=?", server.ID); err != nil {
				log.Warn("Could not save a server's availability; it will be checked again")
				continue
			}
			r.intentional = 0
		}
		healthy := running && container.Health != "unhealthy" && container.Health != "starting"
		// State is saved before any alert: the saved outage start keys each
		// alert, so an unsaved state could later alert again under a new key.
		if healthy {
			if !r.outage.Valid && r.notified == 0 && r.intentional == 0 && r.lastState.String == "running" {
				continue
			}
			if _, err := s.DB.ExecContext(ctx, "UPDATE availability SET outage_started_at=NULL,notified=0,intentionally_stopped=0,last_state='running',updated_at=? WHERE server_id=?",
				now, server.ID); err != nil {
				log.Warn("Could not save a server's availability; it will be checked again")
				continue
			}
			if r.notified == 1 {
				s.notify("recovery:"+server.ID+":"+strconv.FormatInt(r.outage.Int64, 10), server.DisplayName+" recovered.")
			}
			continue
		}
		if r.intentional == 1 {
			continue
		}
		started := now
		if r.outage.Valid {
			started = r.outage.Int64
		}
		notify := now-started >= int64(policy.GraceSeconds)*1000
		notified := 0
		if notify || r.notified == 1 {
			notified = 1
		}
		lastState := string(server.Status)
		if unavailable {
			lastState = "docker_unavailable"
		} else if found && server.ContainerID != "" {
			lastState = container.State
		}
		if !(r.outage.Valid && r.outage.Int64 == started && r.notified == notified && r.lastState.String == lastState) {
			if _, err := s.DB.ExecContext(ctx, "UPDATE availability SET outage_started_at=?,notified=?,last_state=?,updated_at=? WHERE server_id=?",
				started, notified, lastState, now, server.ID); err != nil {
				log.Warn("Could not save a server's availability; it will be checked again")
				continue
			}
		}
		if notify && r.notified == 0 {
			message := server.DisplayName + " is unavailable."
			if unavailable {
				message = "Ludock cannot reach Docker to verify " + server.DisplayName + "."
			}
			s.notify("outage:"+server.ID+":"+strconv.FormatInt(started, 10), message)
		}
	}
	return nil
}

func bytesReader(data []byte) *bytes.Reader { return bytes.NewReader(data) }
