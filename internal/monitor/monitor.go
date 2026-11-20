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
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
)

var log = logging.New("monitor")

type Policy struct {
	Enabled      bool `json:"enabled"`
	Maintenance  bool `json:"maintenance"`
	GraceSeconds int  `json:"graceSeconds"`
	// A data folder's disk alerts at DiskPercent full.
	DiskAlert   bool `json:"diskAlert"`
	DiskPercent int  `json:"diskPercent"`
	// Memory alerts at MemoryPercent of the container's limit, held for
	// MemoryMinutes.
	MemoryAlert   bool `json:"memoryAlert"`
	MemoryPercent int  `json:"memoryPercent"`
	MemoryMinutes int  `json:"memoryMinutes"`
	// Recover restarts a running server whose game stopped answering.
	Recover bool `json:"recover"`
}

// DefaultPolicy is a server's policy until an administrator saves one:
// every server is watched unless an administrator turns that off.
func DefaultPolicy() Policy {
	return Policy{Enabled: true, GraceSeconds: 120, DiskAlert: true, DiskPercent: 90, MemoryAlert: true, MemoryPercent: 90, MemoryMinutes: 5}
}

type State struct {
	OutageStartedAt      int64 // 0 when none
	Notified             bool
	SuppressedUntil      int64
	IntentionallyStopped bool
	LastState            string
}

type Availability struct {
	Policy Policy
	State  State
}

type Service struct {
	DB    *db.DB
	Locks *locks.Table
	// Runner queues recovery restarts. It may be nil.
	Runner *ops.Runner
	Notify func(alert notify.Alert, key, message string)
	// Changed is told when an alert is raised or cleared, which no Docker
	// event announces to open pages. It must not block.
	Changed func(serverID string)
	Store   *identity.Store
	// Game reports whether a running game answers its status query. It may
	// be nil.
	Game func(serverID string, container servers.Container) GameAnswer
	// LastAnswer is when a running game last answered its status query in
	// this container run, or 0. It may be nil.
	LastAnswer func(serverID string, container servers.Container) int64
	// MeasureDisk measures each of a server's data folders after confirming
	// its binding. An error means it was skipped. It may be nil.
	MeasureDisk func(ctx context.Context, server identity.Server) ([]DiskSpace, error)
}

type GameAnswer int8

const (
	GameNotChecked GameAnswer = iota
	GameAnswering
	// GameNotAnswering: the game answered in this container run, then
	// stopped.
	GameNotAnswering
	// GameUnknown: no recent query says.
	GameUnknown
	// GameWaiting: the game was asked in this container run and hasn't
	// answered yet.
	GameWaiting
)

// NotAnswering is the last state of a running container whose game stopped
// answering its status query.
const NotAnswering = "not_answering"

// ParsePolicy validates a policy, applying defaults.
func ParsePolicy(raw []byte) (Policy, error) {
	var input struct {
		Enabled       *bool    `json:"enabled"`
		Maintenance   *bool    `json:"maintenance"`
		GraceSeconds  *float64 `json:"graceSeconds"`
		DiskAlert     *bool    `json:"diskAlert"`
		DiskPercent   *float64 `json:"diskPercent"`
		MemoryAlert   *bool    `json:"memoryAlert"`
		MemoryPercent *float64 `json:"memoryPercent"`
		MemoryMinutes *float64 `json:"memoryMinutes"`
		Recover       *bool    `json:"recover"`
	}
	decoder := json.NewDecoder(bytesReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.Enabled == nil {
		return Policy{}, apperr.New("INVALID_REQUEST", 400, "Invalid request")
	}
	policy := DefaultPolicy()
	policy.Enabled = *input.Enabled
	for _, flag := range []struct {
		input  *bool
		output *bool
	}{{input.Maintenance, &policy.Maintenance}, {input.DiskAlert, &policy.DiskAlert}, {input.MemoryAlert, &policy.MemoryAlert},
		{input.Recover, &policy.Recover}} {
		if flag.input != nil {
			*flag.output = *flag.input
		}
	}
	for _, number := range []struct {
		input    *float64
		output   *int
		min, max float64
	}{{input.GraceSeconds, &policy.GraceSeconds, 10, 86400}, {input.DiskPercent, &policy.DiskPercent, 50, 99},
		{input.MemoryPercent, &policy.MemoryPercent, 50, 99}, {input.MemoryMinutes, &policy.MemoryMinutes, 1, 60}} {
		if number.input == nil {
			continue
		}
		value := *number.input
		if value != float64(int(value)) || value < number.min || value > number.max {
			return Policy{}, apperr.New("INVALID_REQUEST", 400, "Invalid request")
		}
		*number.output = int(value)
	}
	return policy, nil
}

// row is a server's monitoring as read. disk, memory, and recovery hold
// their states' JSON, NULL when none.
type row struct {
	policy                 string
	outage                 sql.NullInt64
	notified, intentional  int
	suppressed             int64
	lastState              sql.NullString
	disk, memory, recovery sql.NullString
}

const rowColumns = "policy_json,outage_started_at,notified,suppressed_until,intentionally_stopped,last_state,disk_json,memory_json,recovery_json"

func (r *row) fields() []any {
	return []any{&r.policy, &r.outage, &r.notified, &r.suppressed, &r.intentional, &r.lastState, &r.disk, &r.memory, &r.recovery}
}

func (s *Service) row(ctx context.Context, serverID string) (*row, error) {
	var r row
	err := s.DB.QueryRowContext(ctx, "SELECT "+rowColumns+" FROM availability WHERE server_id=?", serverID).Scan(r.fields()...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

func (s *Service) rows(ctx context.Context) (map[string]*row, error) {
	result, err := s.DB.QueryContext(ctx, "SELECT server_id,"+rowColumns+" FROM availability")
	if err != nil {
		return nil, err
	}
	defer result.Close()
	rows := map[string]*row{}
	for result.Next() {
		var serverID string
		var r row
		if err := result.Scan(append([]any{&serverID}, r.fields()...)...); err != nil {
			return nil, err
		}
		rows[serverID] = &r
	}
	return rows, result.Err()
}

// active reads the servers with queued or running operations.
func (s *Service) active(ctx context.Context) (map[string]bool, error) {
	result, err := s.DB.QueryContext(ctx, "SELECT DISTINCT server_id FROM operations WHERE status IN ('queued','running')")
	if err != nil {
		return nil, err
	}
	defer result.Close()
	active := map[string]bool{}
	for result.Next() {
		var serverID string
		if err := result.Scan(&serverID); err != nil {
			return nil, err
		}
		active[serverID] = true
	}
	return active, result.Err()
}

func (s *Service) Get(ctx context.Context, serverID string) (*Availability, error) {
	r, err := s.row(ctx, serverID)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return &Availability{Policy: DefaultPolicy()}, nil
	}
	policy, err := ParsePolicy([]byte(r.policy))
	if err != nil {
		return nil, err
	}
	return &Availability{Policy: policy, State: State{OutageStartedAt: r.outage.Int64, Notified: r.notified == 1,
		SuppressedUntil: r.suppressed, IntentionallyStopped: r.intentional == 1, LastState: r.lastState.String}}, nil
}

// Configure saves a policy and resets any outage, and any disk or memory
// alert whose settings changed. The administrator's
// session and role are checked again in the transaction that saves it.
func (s *Service) Configure(ctx context.Context, adminID, tokenHash, serverID string, policy Policy) (*Availability, error) {
	encoded, _ := json.Marshal(policy)
	if err := s.DB.AdminMutation(ctx, adminID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		var r row
		if err := tx.QueryRowContext(ctx, "SELECT "+rowColumns+" FROM availability WHERE server_id=?", serverID).Scan(r.fields()...); err != nil &&
			!errors.Is(err, sql.ErrNoRows) {
			return err
		}
		previous, _ := ParsePolicy([]byte(r.policy))
		disk, memory := resetAlerts(r.disk, r.memory, previous, policy)
		_, err := tx.ExecContext(ctx, `INSERT INTO availability(server_id,policy_json,updated_at) VALUES(?,?,?)
      ON CONFLICT(server_id) DO UPDATE SET policy_json=excluded.policy_json,updated_at=excluded.updated_at, outage_started_at=NULL, notified=0,
        disk_json=?,memory_json=?,recovery_json=?`,
			serverID, string(encoded), db.Now(), disk, memory, resetRecovery(r.recovery, previous, policy))
		return err
	}); err != nil {
		return nil, err
	}
	return s.Get(ctx, serverID)
}

// ensureRow adds the default policy only if none exists, so it never
// replaces one saved concurrently.
func (s *Service) ensureRow(ctx context.Context, serverID string) error {
	encoded, _ := json.Marshal(DefaultPolicy())
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

func (s *Service) suppressed(serverID string, policy Policy, until, now int64, active bool) bool {
	return !policy.Enabled || policy.Maintenance || until > now || active || (s.Locks != nil && s.Locks.ServerBusy(serverID))
}

// Monitored lists the servers whose monitoring is on and not paused for
// maintenance.
func (s *Service) Monitored(ctx context.Context) (map[string]bool, error) {
	rows, err := s.rows(ctx)
	if err != nil {
		return nil, err
	}
	monitored := map[string]bool{}
	for serverID, r := range rows {
		if policy, err := ParsePolicy([]byte(r.policy)); err == nil && policy.Enabled && !policy.Maintenance {
			monitored[serverID] = true
		}
	}
	return monitored, nil
}

type Problem struct {
	State           string
	OutageStartedAt int64
}

// Problems reads the persisted outages of every server without running
// checks. Policy and operation suppression are current even between monitor
// ticks.
func (s *Service) Problems(ctx context.Context, now int64) (map[string]*Problem, error) {
	rows, err := s.rows(ctx)
	if err != nil {
		return nil, err
	}
	active, err := s.active(ctx)
	if err != nil {
		return nil, err
	}
	problems := map[string]*Problem{}
	for serverID, r := range rows {
		policy, err := ParsePolicy([]byte(r.policy))
		if err != nil {
			return nil, err
		}
		if s.suppressed(serverID, policy, r.suppressed, now, active[serverID]) || r.intentional == 1 ||
			!r.outage.Valid || r.outage.Int64 == 0 || now-r.outage.Int64 < int64(policy.GraceSeconds)*1000 {
			continue
		}
		last := r.lastState.String
		if last == "" {
			last = "unavailable"
		}
		problems[serverID] = &Problem{State: last, OutageStartedAt: r.outage.Int64}
	}
	return problems, nil
}

func (s *Service) notify(alert notify.Alert, key, message string) {
	if s.Notify != nil {
		s.Notify(alert, key, message)
	}
}

func (s *Service) changed(serverID string) {
	if s.Changed != nil {
		s.Changed(serverID)
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
	// Each transition below applies only if its row is unchanged since
	// this read.
	rows, err := s.rows(ctx)
	if err != nil {
		return err
	}
	active, err := s.active(ctx)
	if err != nil {
		return err
	}
	if testHook != nil {
		testHook("active")
	}
	if added, err := s.adopt(ctx, logical, rows, current); err != nil {
		return err
	} else if added {
		if rows, err = s.rows(ctx); err != nil {
			return err
		}
	}
	for _, server := range logical {
		r := rows[server.ID]
		if r == nil {
			continue
		}
		policy, err := ParsePolicy([]byte(r.policy))
		if err != nil {
			continue
		}
		if s.suppressed(server.ID, policy, r.suppressed, now, active[server.ID]) {
			continue
		}
		container, found := current[server.ContainerID]
		running := server.Status == identity.StatusActive && found && server.ContainerID != "" && container.State == "running"
		// An owner can start the container outside Ludock. Resume monitoring
		// even if startup never passes its health check.
		if testHook != nil {
			testHook("read")
		}
		// The flag is cleared only if no stop was recorded since the row was
		// read; a newer stop is kept and the server is checked next tick.
		if running && r.intentional == 1 {
			changed, err := s.transition(ctx, server.ID, r, "intentionally_stopped=0")
			if err != nil {
				log.Warn("Could not save a server's availability; it will be checked again")
				continue
			}
			if !changed {
				continue
			}
			r.intentional = 0
		}
		healthy := running && container.Health != "unhealthy" && container.Health != "starting"
		// Silence names the outage even when a health check that asks the
		// game, as Minecraft's does, also fails.
		silent := false
		game := GameNotChecked
		if running && s.Game != nil {
			game = s.Game(server.ID, container)
		}
		// While Docker is unreachable nothing is known about the restart.
		if policy.Recover && !unavailable {
			s.trackRecovery(ctx, server, container, running, game, r, policy, now)
		}
		if running && s.Game != nil {
			// A game that stopped answering recovers only by answering, even
			// after Ludock restarts and whatever the health check says.
			if (game == GameUnknown || game == GameWaiting) && r.lastState.String == NotAnswering {
				continue
			}
			silent = game == GameNotAnswering
			healthy = healthy && !silent
		}
		// State is saved before any alert: the saved outage start keys each
		// alert, so an unsaved state could later alert again under a new key.
		if healthy {
			if !r.outage.Valid && r.notified == 0 && r.intentional == 0 && r.lastState.String == "running" {
				continue
			}
			changed, err := s.transition(ctx, server.ID, r, "outage_started_at=NULL,notified=0,intentionally_stopped=0,last_state='running',updated_at=?", now)
			if err != nil {
				log.Warn("Could not save a server's availability; it will be checked again")
				continue
			}
			if changed && r.notified == 1 {
				s.notify(notify.ServerProblems, "recovery:"+server.ID+":"+strconv.FormatInt(r.outage.Int64, 10), server.DisplayName+": the server is back up.")
			}
			// Pages show an outage once its grace ends, even before its alert.
			if changed && r.outage.Valid {
				s.changed(server.ID)
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
		pastGrace := now-started >= int64(policy.GraceSeconds)*1000
		notified := 0
		if pastGrace || r.notified == 1 {
			notified = 1
		}
		lastState := string(server.Status)
		// A silent game keeps its state while Docker is unreachable, since
		// only the game answering ends that outage.
		if unavailable && r.lastState.String != NotAnswering {
			lastState = "docker_unavailable"
		} else if unavailable || silent {
			lastState = NotAnswering
		} else if found && server.ContainerID != "" {
			lastState = container.State
		}
		changed := false
		if !(r.outage.Valid && r.outage.Int64 == started && r.notified == notified && r.lastState.String == lastState) {
			if changed, err = s.transition(ctx, server.ID, r, "outage_started_at=?,notified=?,last_state=?,updated_at=?",
				started, notified, lastState, now); err != nil {
				log.Warn("Could not save a server's availability; it will be checked again")
				continue
			}
		}
		// A recovery restart, or giving up on one, is announced instead of
		// the outage.
		recovering := false
		if silent && pastGrace && policy.Recover && container.Health != "starting" {
			recovering = s.recover(ctx, server, container, r, policy, now)
		}
		// Raising an alert always changes notified, so it is sent only when
		// that change was saved against the state read above.
		if changed && pastGrace && r.notified == 0 && !recovering {
			message := server.DisplayName + ": the server is down."
			if unavailable {
				message = server.DisplayName + ": Ludock can’t reach Docker to check it."
			} else if silent {
				message = server.DisplayName + ": running, but the game stopped answering."
			}
			s.notify(notify.ServerProblems, "outage:"+server.ID+":"+strconv.FormatInt(started, 10), message)
			s.changed(server.ID)
		} else if changed && pastGrace && r.lastState.String != lastState {
			// Pages show the reason of an outage past its grace.
			s.changed(server.ID)
		}
	}
	return nil
}

// adopt starts watching servers seen for the first time with the default
// policy. A container already stopped counts as stopped on purpose, so a
// server someone left off doesn't alert; it is watched once it runs. Nothing is
// adopted while Docker is unreachable, since its state is unknown.
func (s *Service) adopt(ctx context.Context, logical []identity.Server, rows map[string]*row, current map[string]servers.Container) (bool, error) {
	if current == nil {
		return false, nil
	}
	encoded, _ := json.Marshal(DefaultPolicy())
	added := false
	for _, server := range logical {
		if rows[server.ID] != nil {
			continue
		}
		stopped := 0
		if state := current[server.ContainerID].State; bound(server, current) && (state == "exited" || state == "created") {
			stopped = 1
		}
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO availability(server_id,policy_json,intentionally_stopped,updated_at) VALUES(?,?,?,?)
      ON CONFLICT(server_id) DO NOTHING`, server.ID, string(encoded), stopped, db.Now()); err != nil {
			return false, err
		}
		added = true
	}
	return added, nil
}

// transition applies set only if the row still has the policy and state
// that were read, so a policy saved, a stop recorded, or a suppression added
// meanwhile is never overwritten and raises no alert from the old state.
func (s *Service) transition(ctx context.Context, serverID string, r *row, set string, args ...any) (bool, error) {
	result, err := s.DB.ExecContext(ctx, "UPDATE availability SET "+set+` WHERE server_id=? AND policy_json=? AND
    outage_started_at IS ? AND notified=? AND suppressed_until=? AND intentionally_stopped=?`,
		append(args, serverID, r.policy, r.outage, r.notified, r.suppressed, r.intentional)...)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

// testHook, set only by tests, runs at named points in Check so a test can
// force an interleaving.
var testHook func(point string)

func bytesReader(data []byte) *bytes.Reader { return bytes.NewReader(data) }
