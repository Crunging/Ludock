// Package monitor tracks server availability and notifies about outages
// that outlast their grace period.
package monitor

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

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

// watched reports whether outages are tracked under the policy.
func (p Policy) watched() bool { return p.Enabled && !p.Maintenance }

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
	// Queue queues an alert inside tx (notify.Service.QueueIn), so the
	// state that raises it never commits without it. Alerted wakes delivery
	// once tx commits. Either may be nil.
	Queue   func(ctx context.Context, tx *sql.Tx, alert notify.Alert, key, message string) (bool, error)
	Alerted func()
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
	// Recall tells Game and LastAnswer that a container run's game answered
	// at a time an earlier Ludock saved. It may be nil.
	Recall func(serverID string, container servers.Container, at int64)
	// MeasureDisk measures each of a server's data folders after confirming
	// its binding. An error means it was skipped. It may be nil.
	MeasureDisk func(ctx context.Context, server identity.Server) ([]DiskSpace, error)

	// since is the time of this process's first check. A silence seen before
	// it went unwatched while Ludock was down.
	since atomic.Int64
	// looked is, for each server, when the latest completed check that
	// judged it, rather than skipping it as paused or busy, began. It is
	// saved once that check's writes are done.
	lookMu sync.Mutex
	looked map[string]int64
	// announced is, for each server, the end of the latest wait for a check
	// that pages are told of.
	announced map[string]int64
}

// answerSaveInterval is how often a game that keeps answering has that
// saved, after its first answer in a container run.
const answerSaveInterval = 10 * time.Minute

// answer is when a container run's game last answered, as last saved.
type answer struct {
	Container string `json:"container"`
	Run       int64  `json:"run"`
	At        int64  `json:"at"`
}

// recallAnswer passes on when this container run's game last answered as
// saved, so a game that answered before Ludock restarted isn't taken for
// one still starting. It saves the first answer seen in a run, then one at
// most every answerSaveInterval.
func (s *Service) recallAnswer(ctx context.Context, serverID string, container servers.Container, r *row) {
	if s.LastAnswer == nil {
		return
	}
	var saved answer
	if decode(r.answered, &saved) != nil {
		saved = answer{}
	}
	same := saved.Container == container.ID && saved.Run == container.StateSince
	if same && s.Recall != nil {
		s.Recall(serverID, container, saved.At)
	}
	last := s.LastAnswer(serverID, container)
	if last == 0 || (same && last-saved.At < answerSaveInterval.Milliseconds()) {
		return
	}
	next := encode(answer{Container: container.ID, Run: container.StateSince, At: last})
	if _, err := s.DB.ExecContext(ctx, "UPDATE availability SET answered_json=? WHERE server_id=?", next, serverID); err != nil {
		log.Warn("Could not save when a server's game last answered; it will be saved again")
		return
	}
	r.answered = next
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

// DockerUnavailable is the last state of a server Ludock couldn't check.
const DockerUnavailable = "docker_unavailable"

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

// row is a server's monitoring as read. disk, memory, recovery, and
// answered hold their states' JSON, NULL when none.
type row struct {
	policy                           string
	outage                           sql.NullInt64
	notified, intentional            int
	suppressed                       int64
	lastState                        sql.NullString
	disk, memory, recovery, answered sql.NullString
}

const rowColumns = "policy_json,outage_started_at,notified,suppressed_until,intentionally_stopped,last_state," +
	"disk_json,memory_json,recovery_json,answered_json"

func (r *row) fields() []any {
	return []any{&r.policy, &r.outage, &r.notified, &r.suppressed, &r.intentional, &r.lastState, &r.disk, &r.memory, &r.recovery, &r.answered}
}

func (s *Service) row(ctx context.Context, serverID string) (*row, error) {
	return rowIn(ctx, s.DB, serverID)
}

func rowIn(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, serverID string) (*row, error) {
	var r row
	err := q.QueryRowContext(ctx, "SELECT "+rowColumns+" FROM availability WHERE server_id=?", serverID).Scan(r.fields()...)
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
	return availabilityOf(s.row(ctx, serverID))
}

// GetIn is Get through tx.
func (s *Service) GetIn(ctx context.Context, tx *sql.Tx, serverID string) (*Availability, error) {
	return availabilityOf(rowIn(ctx, tx, serverID))
}

func availabilityOf(r *row, err error) (*Availability, error) {
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

// Configure saves a policy, resetting any disk or memory alert whose settings
// changed and any outage if watching was or is now off or paused for
// maintenance: an outage watched on both sides of the save is the same
// outage, timed from its start and alerted once. The administrator's session
// and role are checked again in the transaction that saves it.
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
		outage := "outage_started_at=NULL,notified=0,"
		if previous.watched() && policy.watched() {
			outage = ""
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO availability(server_id,policy_json,updated_at) VALUES(?,?,?)
      ON CONFLICT(server_id) DO UPDATE SET policy_json=excluded.policy_json,updated_at=excluded.updated_at,`+outage+`
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
func ensureRow(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, serverID string) error {
	encoded, _ := json.Marshal(DefaultPolicy())
	_, err := q.ExecContext(ctx, "INSERT INTO availability(server_id,policy_json,updated_at) VALUES(?,?,?) ON CONFLICT(server_id) DO NOTHING",
		serverID, string(encoded), db.Now())
	return err
}

// keepAlerted forgets an outage only if it wasn't alerted: one that was ends
// with its recovery alert, whoever brings the server back, and never alerts
// again.
const keepAlerted = "outage_started_at=CASE WHEN notified=1 THEN outage_started_at END"

// SetIntentionalStop records that a stop was requested, which ends an
// outage not yet alerted, or cleared. An alerted one stays owed its recovery
// alert, sent once the server is back.
func (s *Service) SetIntentionalStop(ctx context.Context, serverID string, stopped bool) error {
	if err := ensureRow(ctx, s.DB, serverID); err != nil {
		return err
	}
	set := "intentionally_stopped=1," + keepAlerted
	if !stopped {
		set = "intentionally_stopped=0," + keepAlerted
	}
	_, err := s.DB.ExecContext(ctx, "UPDATE availability SET "+set+",updated_at=? WHERE server_id=?", db.Now(), serverID)
	return err
}

// Suppress pauses outage tracking for one grace period after Ludock acted
// on the server, and starts the grace of an outage not yet alerted again.
// allowed, when set, confirms through the pause's transaction that the work
// it is for may go ahead; a refusal writes nothing.
func (s *Service) Suppress(ctx context.Context, serverID string, allowed func(*sql.Tx) error) error {
	return s.suppress(ctx, serverID, keepAlerted, allowed)
}

// SuppressRecovered is Suppress after an automatic restart, which announces
// its own outcome, so it ends the outage even if alerted.
func (s *Service) SuppressRecovered(ctx context.Context, serverID string, allowed func(*sql.Tx) error) error {
	return s.suppress(ctx, serverID, "outage_started_at=NULL,notified=0", allowed)
}

func (s *Service) suppress(ctx context.Context, serverID, outage string, allowed func(*sql.Tx) error) error {
	return s.DB.Tx(ctx, func(tx *sql.Tx) error {
		if allowed != nil {
			if err := allowed(tx); err != nil {
				return err
			}
		}
		if err := ensureRow(ctx, tx, serverID); err != nil {
			return err
		}
		availability, err := s.GetIn(ctx, tx, serverID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE availability SET suppressed_until=?,"+outage+" WHERE server_id=?",
			db.Now()+int64(availability.Policy.GraceSeconds)*1000, serverID)
		return err
	})
}

func (s *Service) suppressed(serverID string, policy Policy, until, now int64, active bool) bool {
	return !policy.watched() || until > now || active || (s.Locks != nil && s.Locks.ServerBusy(serverID))
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
		if policy, err := ParsePolicy([]byte(r.policy)); err == nil && policy.watched() {
			monitored[serverID] = true
		}
	}
	return monitored, nil
}

type Problem struct {
	State           string
	OutageStartedAt int64
}

// unchecked is how long after a pause ends an outage kept across it stays
// hidden while no check has begun, in case checks have stopped.
const unchecked = time.Minute

// Problems reads the persisted outages of every server without running
// checks. Policy and operation suppression are current even between monitor
// ticks.
func (s *Service) Problems(ctx context.Context, now int64) (map[string]*Problem, error) {
	s.lookMu.Lock()
	looked := maps.Clone(s.looked)
	s.lookMu.Unlock()
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
		// An outage kept across a pause may have ended in it unseen: it shows
		// again once a check has looked at the server since the pause ended.
		waiting := r.suppressed > looked[serverID] && now-r.suppressed < unchecked.Milliseconds()
		if waiting && r.notified == 1 {
			s.announceAt(serverID, r.suppressed+unchecked.Milliseconds())
		}
		// An alerted outage shows even if its grace was raised since.
		if s.suppressed(serverID, policy, r.suppressed, now, active[serverID]) || r.intentional == 1 ||
			!r.outage.Valid || r.outage.Int64 == 0 || (r.notified == 0 && now-r.outage.Int64 < int64(policy.GraceSeconds)*1000) {
			continue
		}
		if waiting {
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

// afterFunc runs f after d; tests replace it.
var afterFunc = func(d time.Duration, f func()) { time.AfterFunc(d, f) }

// announceAt tells pages once, at deadline, that an outage Problems hides,
// during its pause or after, shows again, since no check may judge the
// server first and nothing else would: startup recovery can still be
// retrying.
func (s *Service) announceAt(serverID string, deadline int64) {
	s.lookMu.Lock()
	if s.announced == nil {
		s.announced = map[string]int64{}
	}
	due := s.announced[serverID] != deadline
	s.announced[serverID] = deadline
	s.lookMu.Unlock()
	if due {
		afterFunc(time.Duration(deadline-db.Now())*time.Millisecond, func() { s.changed(serverID) })
	}
}

// alert is a server problem to queue with the state that raises it.
type alert struct{ key, message string }

// apply runs an UPDATE of availability and, only if it changed exactly one
// row, queues alerts in the same transaction. It reports whether it changed.
func (s *Service) apply(ctx context.Context, alerts []alert, query string, args ...any) (bool, error) {
	changed := false
	err := s.DB.Tx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if changed = rows == 1; err != nil || !changed {
			return err
		}
		return s.queueIn(ctx, tx, alerts)
	})
	if err == nil && changed {
		s.alerted(alerts)
	}
	return changed && err == nil, err
}

func (s *Service) queueIn(ctx context.Context, tx *sql.Tx, alerts []alert) error {
	for _, a := range alerts {
		if s.Queue == nil {
			return nil
		}
		if _, err := s.Queue(ctx, tx, notify.ServerProblems, a.key, a.message); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) alerted(alerts []alert) {
	if len(alerts) > 0 && s.Alerted != nil {
		s.Alerted()
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
	s.since.CompareAndSwap(0, now)
	since := s.since.Load()
	s.lookMu.Lock()
	previous := maps.Clone(s.looked)
	s.lookMu.Unlock()
	// judged are the servers this check doesn't skip; shown, those with an
	// outage kept across a pause, which pages show again once it is saved.
	var judged, shown []string
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
		container, found := current[server.ContainerID]
		running := server.Status == identity.StatusActive && found && server.ContainerID != "" && container.State == "running"
		if running {
			s.recallAnswer(ctx, server.ID, container, r)
		}
		if s.suppressed(server.ID, policy, r.suppressed, now, active[server.ID]) {
			// An alerted outage that ends, by the test below, in the pause
			// after Ludock acted on the server says so at once. While the work
			// still runs it may be about to stop the server, and an automatic
			// restart announces its own outcome.
			up := r.notified == 1 && !s.suppressed(server.ID, policy, 0, now, active[server.ID]) && running &&
				container.Health != "unhealthy" && container.Health != "starting"
			game := GameNotChecked
			if up && s.Game != nil {
				game = s.Game(server.ID, container)
				up = game != GameNotAnswering && !((game == GameUnknown || game == GameWaiting) && r.lastState.String == NotAnswering)
			}
			up = up && !(r.lastState.String == "restarting" && earlyRun(container, s.answered(server.ID, container), policy.GraceSeconds, now))
			if up {
				if changed, err := s.recovered(ctx, server, r, now); err != nil {
					log.Warn("Could not save a server's availability; it will be checked again")
				} else if changed {
					s.changed(server.ID)
				}
			}
			continue
		}
		judged = append(judged, server.ID)
		if r.notified == 1 && r.suppressed > previous[server.ID] {
			shown = append(shown, server.ID)
		}
		// Ludock vouches only for silence it watched: a silent game's outage
		// from before it restarted, not yet alerted, waits its grace again
		// from this process's first check.
		if r.lastState.String == NotAnswering && r.notified == 0 && r.outage.Valid && r.outage.Int64 < since {
			changed, err := s.transition(ctx, server.ID, r, nil, "outage_started_at=?", since)
			if err != nil {
				log.Warn("Could not save a server's availability; it will be checked again")
				continue
			}
			if !changed {
				continue
			}
			r.outage.Int64 = since
		}
		// An owner can start the container outside Ludock. Resume monitoring
		// even if startup never passes its health check.
		if testHook != nil {
			testHook("read")
		}
		// The flag is cleared only if no stop was recorded since the row was
		// read; a newer stop is kept and the server is checked next tick. A
		// server Docker is restarting was started since, however briefly.
		restarting := server.Status == identity.StatusActive && found && server.ContainerID != "" && container.State == "restarting"
		if (running || restarting) && r.intentional == 1 {
			changed, err := s.transition(ctx, server.ID, r, nil, "intentionally_stopped=0")
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
		// A run after restarts, whoever started it, keeps their outage, which
		// still alerts.
		healthy = healthy && !(r.lastState.String == "restarting" && earlyRun(container, s.answered(server.ID, container), policy.GraceSeconds, now))
		// State is saved before any alert: the saved outage start keys each
		// alert, so an unsaved state could later alert again under a new key.
		if healthy {
			if !r.outage.Valid && r.notified == 0 && r.intentional == 0 && r.lastState.String == "running" {
				continue
			}
			changed, err := s.recovered(ctx, server, r, now)
			if err != nil {
				log.Warn("Could not save a server's availability; it will be checked again")
				continue
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
		// A silent game, or one that kept restarting, keeps its state while
		// Docker is unreachable, since only the game answering, or a run
		// that lasts, ends that outage.
		if unavailable && r.lastState.String != NotAnswering && r.lastState.String != "restarting" {
			lastState = DockerUnavailable
		} else if unavailable || silent {
			lastState = NotAnswering
		} else if found && server.ContainerID != "" && (container.State != "running" || r.lastState.String != "restarting") {
			// A run between restarts, still starting, stays part of them.
			lastState = container.State
		} else if found && server.ContainerID != "" {
			lastState = r.lastState.String
		}
		// A recovery restart, or giving up on one, is announced instead of
		// the outage. It saves only the recovery state, which the outage
		// transition below doesn't compare.
		recovering := false
		if silent && pastGrace && policy.Recover && container.Health != "starting" {
			recovering = s.recover(ctx, server, container, r, policy, now)
		}
		// Raising an alert always changes notified, and is queued with that
		// change, saved against the state read above.
		raise := pastGrace && r.notified == 0 && !recovering
		var down []alert
		if raise {
			message := server.DisplayName + ": the server is down."
			if unavailable {
				message = server.DisplayName + ": Ludock can’t reach Docker to check it."
			} else if silent {
				message = server.DisplayName + ": running, but the game stopped answering."
			}
			down = []alert{{"outage:" + server.ID + ":" + strconv.FormatInt(started, 10), message}}
		}
		changed := false
		if !(r.outage.Valid && r.outage.Int64 == started && r.notified == notified && r.lastState.String == lastState) {
			if changed, err = s.transition(ctx, server.ID, r, down, "outage_started_at=?,notified=?,last_state=?,updated_at=?",
				started, notified, lastState, now); err != nil {
				log.Warn("Could not save a server's availability; it will be checked again")
				continue
			}
		}
		if changed && raise {
			s.changed(server.ID)
		} else if changed && notified == 1 && r.lastState.String != lastState {
			// Pages show the reason of an outage past its grace or alerted.
			s.changed(server.ID)
		}
	}
	s.lookMu.Lock()
	if s.looked == nil {
		s.looked = map[string]int64{}
	}
	for _, serverID := range judged {
		s.looked[serverID] = now
	}
	s.lookMu.Unlock()
	for _, serverID := range shown {
		s.changed(serverID)
	}
	return nil
}

// earlyRun reports a run too short and quiet to show a server that kept
// restarting is back: the game hasn't answered in it, and it hasn't stayed
// up for the grace period.
func earlyRun(container servers.Container, answered bool, graceSeconds int, now int64) bool {
	return container.State == "running" && !answered && now-container.StateSince < int64(graceSeconds)*1000
}

// Looping reports whether a run of a server seen restarting is still one of
// the loop's, as Check judges the loop's outage.
func (s *Service) Looping(ctx context.Context, serverID string, container servers.Container) bool {
	policy := DefaultPolicy()
	if r, err := s.row(ctx, serverID); err == nil && r != nil {
		if saved, err := ParsePolicy([]byte(r.policy)); err == nil {
			policy = saved
		}
	}
	return earlyRun(container, s.answered(serverID, container), policy.GraceSeconds, db.Now())
}

// answered reports whether the game answered in this container run.
func (s *Service) answered(serverID string, container servers.Container) bool {
	if s.LastAnswer != nil && s.LastAnswer(serverID, container) > 0 {
		return true
	}
	if s.Game == nil {
		return false
	}
	game := s.Game(serverID, container)
	return game == GameAnswering || game == GameNotAnswering
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
// alerts are queued with the change.
func (s *Service) transition(ctx context.Context, serverID string, r *row, alerts []alert, set string, args ...any) (bool, error) {
	return s.transitionIf(ctx, serverID, r, alerts, "", set, args...)
}

// transitionIf is transition that also needs the condition where.
func (s *Service) transitionIf(ctx context.Context, serverID string, r *row, alerts []alert, where, set string, args ...any) (bool, error) {
	return s.apply(ctx, alerts, "UPDATE availability SET "+set+" WHERE server_id=? AND policy_json=? AND "+asRead+where,
		append(append(args, serverID, r.policy), r.asRead()...)...)
}

// whileIdle holds a transition while any operation is queued or running for
// the server, including one queued after Check read them: it may be about to
// stop the server, so the outage is judged again once it ends.
const whileIdle = " AND NOT EXISTS(SELECT 1 FROM operations WHERE operations.server_id=availability.server_id AND operations.status IN ('queued','running'))"

// recovered ends an outage, with its recovery alert if it was alerted.
func (s *Service) recovered(ctx context.Context, server identity.Server, r *row, now int64) (bool, error) {
	var back []alert
	if r.notified == 1 {
		back = []alert{{"recovery:" + server.ID + ":" + strconv.FormatInt(r.outage.Int64, 10), server.DisplayName + ": the server is back up."}}
	}
	return s.transitionIf(ctx, server.ID, r, back, whileIdle, "outage_started_at=NULL,notified=0,intentionally_stopped=0,last_state='running',updated_at=?", now)
}

// asRead matches a row whose outage, alert, pause and stop are still as r
// read them.
const asRead = "outage_started_at IS ? AND notified=? AND suppressed_until=? AND intentionally_stopped=?"

func (r *row) asRead() []any { return []any{r.outage, r.notified, r.suppressed, r.intentional} }

// testHook, set only by tests, runs at named points in Check so a test can
// force an interleaving.
var testHook func(point string)

func bytesReader(data []byte) *bytes.Reader { return bytes.NewReader(data) }
