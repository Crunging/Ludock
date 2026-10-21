package schedules

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/schedtime"
	"github.com/Crunging/Ludock/internal/servers"
)

var log = logging.New("schedules")

// Limits.
const (
	MaxPayloadBytes = 4 * 1024
	MaxPerServer    = 100
	MaxTotal        = 1000
	announceCutoff  = 15 * time.Second
	announceMinimum = 15 * time.Second
)

// Reasons a schedule cannot run.
const (
	OwnerMissing        = "owner_missing"
	OwnerDisabled       = "owner_disabled"
	OwnerAccessRemoved  = "owner_access_removed"
	ActionAccessRemoved = "action_access_removed"
	BindingChanged      = "binding_changed"
	BindingUnavailable  = "binding_unavailable"
	Unavailable         = "unavailable"
)

var unavailableMessages = map[string]string{
	OwnerMissing:        "Suspended: the schedule owner no longer exists",
	OwnerDisabled:       "Suspended: the schedule owner's account is disabled",
	OwnerAccessRemoved:  "Suspended: required access to manage schedules has been removed",
	ActionAccessRemoved: "Suspended: required access for this scheduled action has been removed",
	BindingChanged:      "Suspended: server configuration changed; recreate this schedule after reviewing the server",
	BindingUnavailable:  "Suspended: the server binding is unavailable or requires administrator review",
	Unavailable:         "Suspended: this schedule cannot currently run; review the server and schedule",
}

// UnavailableError explains why a schedule cannot run.
type UnavailableError struct {
	Reason string
	Public *apperr.Error
}

func unavailable(reason string) *UnavailableError {
	binding := reason == BindingChanged || reason == BindingUnavailable
	code, status := "SCHEDULE_REVOKED", 403
	if reason == BindingChanged {
		code, status = "SCHEDULE_BINDING_CHANGED", 409
	} else if binding {
		code, status = "SCHEDULE_BINDING_UNAVAILABLE", 409
	}
	return &UnavailableError{Reason: reason, Public: apperr.New(code, status, unavailableMessages[reason])}
}

func (e *UnavailableError) Error() string { return e.Public.Message }

// Unwrap exposes the public error.
func (e *UnavailableError) Unwrap() error { return e.Public }

func missed(nonexistent bool) error {
	if nonexistent {
		return apperr.New("SCHEDULE_MISSED", 409, "Missed: the scheduled local time doesn't exist in its time zone")
	}
	return apperr.New("SCHEDULE_MISSED", 409, "Missed: Ludock was not running at the scheduled time")
}

// Broadcaster sends a console command for countdown warnings. assertAccess
// runs while the command is in progress; ctx ends it.
type Broadcaster interface {
	Broadcast(ctx context.Context, server *servers.Context, command string, assertAccess func() error) error
}

// Service manages schedules.
type Service struct {
	DB       *db.DB
	Authz    *authz.Service
	Identity *identity.Store
	Runner   *ops.Runner
	Servers  *servers.Service
	Locks    *locks.Table
	Notify   func(key, message string)
	Console  Broadcaster

	mu         sync.Mutex
	announcing map[string]bool
	warnings   sync.WaitGroup
}

type row struct {
	id, serverID, ownerID, input   string
	bindingRevision, revision      int64
	lastSlot, lastResult, lastOpID sql.NullString
	lastRunAt                      sql.NullInt64
	createdAt                      int64
}

const columns = "id,server_id,owner_id,input_json,binding_revision,revision,last_slot,last_result,last_operation_id,last_run_at,created_at"

func scan(s interface{ Scan(...any) error }) (*row, error) {
	var r row
	return &r, s.Scan(&r.id, &r.serverID, &r.ownerID, &r.input, &r.bindingRevision, &r.revision, &r.lastSlot,
		&r.lastResult, &r.lastOpID, &r.lastRunAt, &r.createdAt)
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// authority confirms the owner can still run this schedule now.
func (s *Service) authority(ctx context.Context, q queryer, r *row, data Input) (*db.SessionUser, *identity.Server, error) {
	user, err := s.DB.FindUserByID(ctx, r.ownerID)
	if err != nil {
		return nil, nil, err
	}
	if user == nil {
		return nil, nil, unavailable(OwnerMissing)
	}
	if user.Disabled {
		return nil, nil, unavailable(OwnerDisabled)
	}
	// Inspect assigned grants before binding availability, so a suspended
	// binding is not misreported as a removed grant.
	if user.Role != db.RoleAdmin {
		grant, err := s.Authz.UserGrant(ctx, user.ID, r.serverID)
		if err != nil {
			return nil, nil, err
		}
		if user.Role != db.RoleOperator || grant == nil || !slices.Contains(grant.Capabilities, authz.ServerView) ||
			!slices.Contains(grant.Capabilities, authz.SchedulesManage) {
			return nil, nil, unavailable(OwnerAccessRemoved)
		}
		for _, capability := range data.Capabilities() {
			if !slices.Contains(grant.Capabilities, capability) {
				return nil, nil, unavailable(ActionAccessRemoved)
			}
		}
	}
	logical, err := s.Identity.Get(ctx, r.serverID)
	if err != nil {
		return nil, nil, err
	}
	if logical == nil {
		return nil, nil, unavailable(BindingUnavailable)
	}
	var original string
	err = q.QueryRowContext(ctx, `SELECT binding_fingerprint FROM server_bindings WHERE server_id=? AND binding_revision=? AND accepted=1
    ORDER BY id DESC LIMIT 1`, r.serverID, r.bindingRevision).Scan(&original)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	if original == "" || original != logical.BindingFingerprint {
		return nil, nil, unavailable(BindingChanged)
	}
	binding, err := s.Identity.ResolveBinding(ctx, r.serverID, -1)
	if err != nil {
		if _, ok := apperr.As(err); ok {
			return nil, nil, unavailable(BindingUnavailable)
		}
		return nil, nil, err
	}
	actor := &db.SessionUser{ID: user.ID, Username: user.Username, Role: user.Role}
	// The normal current role, grant, and binding authority is the final gate.
	for _, capability := range append([]authz.Capability{authz.SchedulesManage}, data.Capabilities()...) {
		ok, err := s.Authz.Has(ctx, actor, r.serverID, capability)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			if capability == authz.SchedulesManage {
				return nil, nil, unavailable(OwnerAccessRemoved)
			}
			return nil, nil, unavailable(ActionAccessRemoved)
		}
	}
	return actor, binding, nil
}

// Schedule is a saved schedule as shown.
type Schedule struct {
	Input
	ID                       string
	ServerID                 string
	OwnerID                  string
	LastResult               string
	LastOperation            *ops.Job
	LastRunAt                int64
	LastSlot                 string
	Revision                 int64
	NextRunAt                int64 // 0 when none
	NextRunUnavailableReason string
	CompletedAt              int64 // set once a one-time run was queued, skipped, or missed
}

func oneTimeCompleted(r *row, data Input) bool {
	return data.Kind == "once" && r.lastSlot.String == schedtime.OneTimeSlot(data.Clock())
}

// assertRunAhead requires one-time schedules to be saved or resumed with a
// time still ahead.
func assertRunAhead(data Input, now int64) error {
	if data.Kind != "once" {
		return nil
	}
	runAt, ok := schedtime.OneTimeRun(data.Clock())
	if !ok {
		return apperr.New("SCHEDULE_TIME_INVALID", 400, "That local time doesn't exist in the selected time zone. Choose another time.")
	}
	if runAt < now/60_000*60_000 {
		return apperr.New("SCHEDULE_TIME_PAST", 400, "One-time schedules need a date and time in the future.")
	}
	return nil
}

func (s *Service) latestOperation(ctx context.Context, r *row) *ops.Job {
	if !r.lastOpID.Valid {
		return nil
	}
	job, err := s.Runner.Get(ctx, r.lastOpID.String)
	// Corrupt or unrelated history never exposes an operation's private state.
	if err != nil || job == nil || job.ServerID != r.serverID || job.ActorID != r.ownerID || job.Input["scheduleId"] != r.id {
		return nil
	}
	return job
}

func (s *Service) public(ctx context.Context, r *row, now int64, check func(*row, Input) error) (*Schedule, error) {
	data, err := ParseSaved(r.input)
	if err != nil {
		return nil, err
	}
	completed := oneTimeCompleted(r, data)
	view := &Schedule{Input: data, ID: r.id, ServerID: r.serverID, OwnerID: r.ownerID, LastResult: r.lastResult.String,
		LastOperation: s.latestOperation(ctx, r), LastRunAt: r.lastRunAt.Int64, LastSlot: r.lastSlot.String, Revision: r.revision}
	if completed {
		view.CompletedAt = r.lastRunAt.Int64
	}
	if data.Enabled && !completed {
		if err := check(r, data); err != nil {
			var reason *UnavailableError
			if errors.As(err, &reason) {
				view.NextRunUnavailableReason = reason.Reason
			} else {
				view.NextRunUnavailableReason = Unavailable
			}
		} else if next, ok := schedtime.NextRun(data.Clock(), now, r.lastSlot.String); ok {
			view.NextRunAt = next
		} else {
			view.NextRunUnavailableReason = Unavailable
		}
	}
	return view, nil
}

// manager checks schedule management. Administrators can inspect and remove
// saved automation while its container is missing or awaiting review;
// enabling work still needs live capabilities.
func (s *Service) manager(ctx context.Context, actor *db.SessionUser, serverID string, allowUnavailable bool) (*db.SessionUser, error) {
	current, err := s.Authz.CurrentActor(ctx, actor)
	if err != nil {
		return nil, err
	}
	capability := authz.SchedulesManage
	if allowUnavailable && current != nil && current.Role == db.RoleAdmin {
		capability = authz.ServerView
	}
	if _, err := s.Authz.Assert(ctx, actor, serverID, capability); err != nil {
		return nil, err
	}
	if current == nil {
		return nil, authz.ErrServerNotFound
	}
	return current, nil
}

// authorityState reads, on q alone, every row the authority checks depend
// on: each user's role, status, and grant on the server, and the server's
// binding. A write reads it before its checks and again inside its
// transaction, and stops if anything changed. Transactions take the write
// lock at once, so the checks themselves, which read through the shared
// pool, must not run inside one: with every connection held by writers
// queued behind it, the lock holder would wait for a free connection.
func authorityState(ctx context.Context, q queryer, serverID string, userIDs ...string) (string, error) {
	var state []any
	for _, id := range userIDs {
		var role string
		var disabled int
		var grant sql.NullString
		err := q.QueryRowContext(ctx, `SELECT users.role, users.disabled, server_grants.capabilities_json FROM users
      LEFT JOIN server_grants ON server_grants.user_id = users.id AND server_grants.server_id = ? WHERE users.id = ?`,
			serverID, id).Scan(&role, &disabled, &grant)
		if errors.Is(err, sql.ErrNoRows) {
			state = append(state, nil)
			continue
		}
		if err != nil {
			return "", err
		}
		state = append(state, []any{role, disabled, grant})
	}
	var status string
	var review int
	var revision int64
	var fingerprint, container sql.NullString
	err := q.QueryRowContext(ctx, `SELECT status, review_required, binding_revision, binding_fingerprint, container_id
    FROM logical_servers WHERE id = ?`, serverID).Scan(&status, &review, &revision, &fingerprint, &container)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	state = append(state, []any{status, review, revision, fingerprint, container})
	encoded, _ := json.Marshal(state)
	return string(encoded), nil
}

var errAccessChanged = apperr.New("SCHEDULE_ACCESS_CHANGED", 409, "Access to this server changed. Reload and try again.")

// assertUnchanged compares the authority state inside tx with before.
func assertUnchanged(ctx context.Context, tx *sql.Tx, before, serverID string, userIDs ...string) error {
	now, err := authorityState(ctx, tx, serverID, userIDs...)
	if err == nil && now != before {
		err = errAccessChanged
	}
	return err
}

// List lists a server's schedules: every schedule for administrators, the
// actor's own for others.
func (s *Service) List(ctx context.Context, actor *db.SessionUser, serverID string) ([]Schedule, error) {
	current, err := s.manager(ctx, actor, serverID, true)
	if err != nil {
		return nil, err
	}
	query := "SELECT " + columns + " FROM schedules WHERE server_id=? ORDER BY created_at,id LIMIT ?"
	args := []any{serverID, MaxPerServer}
	if current.Role != db.RoleAdmin {
		query = "SELECT " + columns + " FROM schedules WHERE server_id=? AND owner_id=? ORDER BY created_at,id LIMIT ?"
		args = []any{serverID, current.ID, MaxPerServer}
	}
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var saved []*row
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		saved = append(saved, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reuse authority outcomes within one listing; runs and edits always
	// check afresh.
	outcomes := map[string]error{}
	check := func(r *row, data Input) error {
		key := fmt.Sprint(r.ownerID, r.serverID, r.bindingRevision, data.Capabilities())
		if err, ok := outcomes[key]; ok {
			return err
		}
		_, _, err := s.authority(ctx, s.DB, r, data)
		outcomes[key] = err
		return err
	}
	now := db.Now()
	var result []Schedule
	for _, r := range saved {
		view, err := s.public(ctx, r, now, check)
		if err != nil {
			// A damaged saved schedule is still listed for removal.
			result = append(result, Schedule{ID: r.id, ServerID: r.serverID, OwnerID: r.ownerID, Revision: r.revision,
				LastResult: r.lastResult.String, NextRunUnavailableReason: Unavailable})
			continue
		}
		result = append(result, *view)
	}
	return result, nil
}

func (s *Service) check(ctx context.Context) func(*row, Input) error {
	return func(r *row, data Input) error {
		_, _, err := s.authority(ctx, s.DB, r, data)
		return err
	}
}

func (s *Service) get(ctx context.Context, q queryer, id, serverID string) (*row, error) {
	r, err := scan(q.QueryRowContext(ctx, "SELECT "+columns+" FROM schedules WHERE id=? AND server_id=?", id, serverID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func payloadSize(input any) error {
	encoded, _ := json.Marshal(input)
	if len(encoded) > MaxPayloadBytes {
		return apperr.Newf("SCHEDULE_PAYLOAD_TOO_LARGE", 413, "Schedule payloads cannot exceed %d bytes", MaxPayloadBytes)
	}
	return nil
}

// Create saves a schedule owned by the actor.
func (s *Service) Create(ctx context.Context, actor *db.SessionUser, serverID string, data Input) (*Schedule, error) {
	if err := payloadSize(data); err != nil {
		return nil, err
	}
	if err := data.Validate(); err != nil {
		return nil, err
	}
	state, err := authorityState(ctx, s.DB, serverID, actor.ID)
	if err != nil {
		return nil, err
	}
	for _, capability := range append([]authz.Capability{authz.SchedulesManage}, data.EditCapabilities()...) {
		if _, err := s.Authz.Assert(ctx, actor, serverID, capability); err != nil {
			return nil, err
		}
	}
	if err := assertRunAhead(data, db.Now()); err != nil {
		return nil, err
	}
	binding, err := s.Identity.ResolveBinding(ctx, serverID, -1)
	if err != nil {
		return nil, err
	}
	id := identity.NewUUID()
	if _, _, err := s.authority(ctx, s.DB, &row{id: id, serverID: serverID, ownerID: actor.ID,
		bindingRevision: binding.BindingRevision}, data); err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(data)
	err = s.DB.Tx(ctx, func(tx *sql.Tx) error {
		// Access revoked after the checks above plants nothing.
		if err := assertUnchanged(ctx, tx, state, serverID, actor.ID); err != nil {
			return err
		}
		// Check the global boundary first; this stops after a bounded number
		// of rows even on a database already over it.
		var present int
		err := tx.QueryRowContext(ctx, "SELECT 1 FROM schedules ORDER BY rowid LIMIT 1 OFFSET ?", MaxTotal-1).Scan(&present)
		if err == nil {
			return apperr.Newf("SCHEDULE_LIMIT_REACHED", 409, "Ludock supports up to %d schedules", MaxTotal)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM schedules WHERE server_id=?", serverID).Scan(&count); err != nil {
			return err
		}
		if count >= MaxPerServer {
			return apperr.Newf("SCHEDULE_LIMIT_REACHED", 409, "A server can have up to %d schedules", MaxPerServer)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schedules(id,server_id,owner_id,input_json,binding_revision,created_at) VALUES(?,?,?,?,?,?)",
			id, serverID, actor.ID, string(encoded), binding.BindingRevision, db.Now()); err != nil {
			return err
		}
		return s.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: actor.ID, Action: "schedule.created", TargetType: "server",
			TargetID: serverID, Details: map[string]string{"scheduleId": id, "action": data.Action}})
	})
	if err != nil {
		return nil, err
	}
	if err := s.DB.PruneAuditIfNeeded(ctx); err != nil {
		log.Warn("Audit retention cleanup failed after schedule creation; it will be retried")
	}
	r, err := s.get(ctx, s.DB, id, serverID)
	if err != nil {
		return nil, err
	}
	return s.public(ctx, r, db.Now(), s.check(ctx))
}

var errNotFound = apperr.New("NOT_FOUND", 404, "Schedule not found")

// Delete removes a schedule.
func (s *Service) Delete(ctx context.Context, actor *db.SessionUser, serverID, id string) error {
	state, err := authorityState(ctx, s.DB, serverID, actor.ID)
	if err != nil {
		return err
	}
	current, err := s.manager(ctx, actor, serverID, true)
	if err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx *sql.Tx) error {
		// A grant or role revoked after the check above stops the deletion.
		if err := assertUnchanged(ctx, tx, state, serverID, actor.ID); err != nil {
			return err
		}
		r, err := s.get(ctx, tx, id, serverID)
		if err != nil {
			return err
		}
		if r == nil || (current.Role != db.RoleAdmin && r.ownerID != current.ID) {
			return errNotFound
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM schedules WHERE id=?", id); err != nil {
			return err
		}
		return s.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: actor.ID, Action: "schedule.deleted", TargetType: "server",
			TargetID: serverID, Details: map[string]string{"scheduleId": id}})
	})
}

func (s *Service) mutate(ctx context.Context, actor *db.SessionUser, serverID, id string, expected int64, update *Input, enabled *bool) (*Schedule, error) {
	pausing := enabled != nil && !*enabled
	// The authority state is read before any check, so the transaction can
	// tell whether the requester's or owner's access changed since.
	first, err := s.get(ctx, s.DB, id, serverID)
	if err != nil {
		return nil, err
	}
	users := []string{actor.ID}
	if first != nil {
		users = append(users, first.ownerID)
	}
	state, err := authorityState(ctx, s.DB, serverID, users...)
	if err != nil {
		return nil, err
	}
	current, err := s.manager(ctx, actor, serverID, pausing)
	if err != nil {
		return nil, err
	}
	next := func(r *row) (Input, Input, error) {
		previous, parseErr := ParseSaved(r.input)
		if parseErr != nil && update == nil {
			return Input{}, Input{}, apperr.New("INVALID_SCHEDULE", 409, "Saved schedule configuration is invalid; recreate this schedule")
		}
		if update != nil {
			return previous, *update, nil
		}
		data := previous
		data.Enabled = *enabled
		return previous, data, nil
	}
	check := func(r *row) error {
		if r == nil || (current.Role != db.RoleAdmin && r.ownerID != current.ID) {
			return errNotFound
		}
		if r.revision != expected {
			return apperr.New("SCHEDULE_CHANGED", 409, "This schedule changed. Reload it before trying again.")
		}
		return nil
	}
	// Requester checks can write denial audits, and every check reads through
	// the shared pool, so they run before the transaction; it re-reads the
	// same revision and authority state.
	if err := check(first); err != nil {
		return nil, err
	}
	if !pausing {
		_, data, err := next(first)
		if err != nil {
			return nil, err
		}
		for _, capability := range data.EditCapabilities() {
			if _, err := s.Authz.Assert(ctx, actor, serverID, capability); err != nil {
				return nil, err
			}
		}
		// Pausing stays possible after the action grant or original binding
		// was revoked. Editing or resuming needs requester and owner access.
		if _, _, err := s.authority(ctx, s.DB, first, data); err != nil {
			return nil, err
		}
	}
	var result *row
	changed := false
	err = s.DB.Tx(ctx, func(tx *sql.Tx) error {
		r, err := s.get(ctx, tx, id, serverID)
		if err != nil {
			return err
		}
		// The pinned revision keeps the owner and settings checked above.
		if err := check(r); err != nil {
			return err
		}
		if err := assertUnchanged(ctx, tx, state, serverID, users...); err != nil {
			return err
		}
		previous, data, err := next(r)
		if err != nil {
			return err
		}
		// A consumed or past one-time run is rescheduled by editing, never
		// replayed.
		if !pausing {
			if err := assertRunAhead(data, db.Now()); err != nil {
				return err
			}
		}
		result = r
		if previous.key() == data.key() {
			return nil
		}
		changed = true
		encoded, _ := json.Marshal(data)
		// Keep the binding baseline, consumed slot, and history. Every change
		// invalidates work queued from an earlier configuration.
		if _, err := tx.ExecContext(ctx, "UPDATE schedules SET input_json=?,revision=revision+1 WHERE id=?", string(encoded), id); err != nil {
			return err
		}
		r.input, r.revision = string(encoded), r.revision+1
		action := "schedule.updated"
		if update == nil {
			action = "schedule.paused"
			if *enabled {
				action = "schedule.resumed"
			}
		}
		return s.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: current.ID, Action: action, TargetType: "server", TargetID: serverID,
			Details: map[string]any{"scheduleId": id, "action": data.Action, "revision": r.revision}})
	})
	if err != nil {
		return nil, err
	}
	if changed {
		if err := s.DB.PruneAuditIfNeeded(ctx); err != nil {
			log.Warn("Audit retention cleanup failed after schedule update; it will be retried")
		}
	}
	return s.public(ctx, result, db.Now(), s.check(ctx))
}

// Update replaces a schedule's settings at the expected revision.
func (s *Service) Update(ctx context.Context, actor *db.SessionUser, serverID, id string, revision int64, data Input) (*Schedule, error) {
	if err := payloadSize(data); err != nil {
		return nil, err
	}
	if err := data.Validate(); err != nil {
		return nil, err
	}
	return s.mutate(ctx, actor, serverID, id, revision, &data, nil)
}

// SetEnabled pauses or resumes a schedule at the expected revision.
func (s *Service) SetEnabled(ctx context.Context, actor *db.SessionUser, serverID, id string, revision int64, enabled bool) (*Schedule, error) {
	return s.mutate(ctx, actor, serverID, id, revision, nil, &enabled)
}

func (s *Service) notify(key, message string) {
	if s.Notify != nil {
		s.Notify(key, message)
	}
}

// errScheduleChanged rolls back a run for a schedule edited since it was
// read; the next run evaluates the current revision.
var errScheduleChanged = errors.New("schedule changed")

// Run queues every due schedule once per slot. Missed one-time runs are
// recorded, never replayed.
func (s *Service) Run(ctx context.Context, now int64) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+columns+" FROM schedules ORDER BY rowid LIMIT ?", MaxTotal+1)
	if err != nil {
		return
	}
	var saved []*row
	for rows.Next() {
		if r, err := scan(rows); err == nil {
			saved = append(saved, r)
		}
	}
	rows.Close()
	// A partial list would miss schedules due now; the next tick reads again.
	if err := rows.Err(); err != nil {
		log.Warn("Could not read schedules; they will be evaluated at the next check")
		return
	}
	if len(saved) > MaxTotal {
		log.Warn("Schedule limit exceeded; schedules beyond the limit are not evaluated", logging.F("limit", MaxTotal))
		saved = saved[:MaxTotal]
	}
	for _, r := range saved {
		s.runOne(ctx, r, now)
	}
}

func (s *Service) runOne(ctx context.Context, r *row, now int64) {
	slot := ""
	configurationValid := false
	err := func() error {
		data, err := ParseSaved(r.input)
		if err != nil {
			return err
		}
		if !data.Enabled || oneTimeCompleted(r, data) {
			return nil
		}
		slot = schedtime.Slot(data.Clock(), now)
		configurationValid = true
		if data.Kind == "once" && slot == "" {
			runAt, ok := schedtime.OneTimeRun(data.Clock())
			if ok && now < runAt+schedtime.OneTimeGraceMs {
				return nil
			}
			// The run time passed while Ludock was stopped or the schedule could
			// not be evaluated: record it as missed instead of running late.
			slot = schedtime.OneTimeSlot(data.Clock())
			return missed(!ok)
		}
		if slot == "" || slot == r.lastSlot.String {
			return nil
		}
		state, err := authorityState(ctx, s.DB, r.serverID, r.ownerID)
		if err != nil {
			return err
		}
		user, binding, err := s.authority(ctx, s.DB, r, data)
		if err != nil {
			return err
		}
		err = s.DB.Tx(ctx, func(tx *sql.Tx) error {
			// The owner's access is as checked above, read on this connection.
			if err := assertUnchanged(ctx, tx, state, r.serverID, r.ownerID); err != nil {
				return err
			}
			input := map[string]any{"scheduleId": r.id, "scheduleRevision": r.revision}
			if data.CreateBackup {
				input["createBackup"] = true
			}
			job, err := s.Runner.Enqueue(ctx, tx, ops.EnqueueOptions{ServerID: r.serverID, ActorID: user.ID, Kind: data.Action,
				BindingRevision: binding.BindingRevision, Input: input, IdempotencyKey: "schedule:" + r.id + ":" + slot})
			if err != nil {
				return err
			}
			// Queue and associate the same attempt atomically.
			result, err := tx.ExecContext(ctx, "UPDATE schedules SET last_slot=?,last_run_at=?,last_operation_id=?,last_result=? WHERE id=? AND revision=?",
				slot, now, job.ID, "Queued operation "+job.ID, r.id, r.revision)
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			if err == nil && changed == 0 {
				err = errScheduleChanged
			}
			return err
		})
		if err == nil {
			s.Runner.Wake()
			if err := s.DB.PruneAuditIfNeeded(ctx); err != nil {
				log.Warn("Audit retention cleanup failed after scheduling; it will be retried")
			}
		}
		return err
	}()
	// A schedule or access changed since it was read: the next run
	// evaluates the current state.
	if err == nil || errors.Is(err, errScheduleChanged) || errors.Is(err, errAccessChanged) {
		return
	}
	reason := "Skipped: server state or a conflicting operation prevented this run"
	var unavailableErr *UnavailableError
	if !configurationValid {
		reason = "Suspended: saved schedule configuration is invalid; recreate this schedule"
	} else if errors.As(err, &unavailableErr) {
		reason = unavailableErr.Public.Message
	} else if apperr.Is(err, "SCHEDULE_MISSED") {
		reason = err.Error()
	}
	var result sql.Result
	var recordErr error
	if slot != "" {
		result, recordErr = s.DB.ExecContext(ctx, "UPDATE schedules SET last_slot=?,last_run_at=?,last_operation_id=NULL,last_result=? WHERE id=? AND revision=?",
			slot, now, reason, r.id, r.revision)
	} else {
		result, recordErr = s.DB.ExecContext(ctx, "UPDATE schedules SET last_result=? WHERE id=? AND revision=?", reason, r.id, r.revision)
	}
	if recordErr != nil {
		log.Warn("Could not record or notify a skipped schedule run", logging.F("scheduleId", r.id))
		return
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 0 {
		return
	}
	if !configurationValid && r.lastResult.String != reason {
		log.Warn("Saved schedule configuration is invalid; recreate the schedule", logging.F("scheduleId", r.id))
	}
	key := slot
	if key == "" {
		key = "invalid"
	}
	s.notify("schedule:"+r.id+":"+key, "A scheduled Ludock action could not run. Review its server and permissions.")
}

// claimAnnouncement records a warning before it is sent, so a restart
// never repeats it. Claims stay until their slot has passed, since edits can
// return to an earlier slot.
func (s *Service) claimAnnouncement(ctx context.Context, r *row, slot string, lead int, now int64) bool {
	claimed := false
	if err := s.DB.Tx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schedule_announcements(schedule_id,slot,lead_minutes)
      SELECT id,?,? FROM schedules WHERE id=? AND revision=?`, slot, lead, r.id, r.revision)
		if err != nil {
			return err
		}
		changes, _ := result.RowsAffected()
		if changes == 0 {
			return nil
		}
		claimed = true
		// Slots start with their local date, which is within a day of UTC.
		spent := time.UnixMilli(now - 2*86_400_000).UTC().Format("2006-01-02")
		_, err = tx.ExecContext(ctx, "DELETE FROM schedule_announcements WHERE schedule_id=? AND substr(slot,1,10)<?", r.id, spent)
		return err
	}); err != nil {
		// The insert rolled back with the transaction: no durable claim, so
		// no warning. A later tick may claim it again.
		log.Warn("Could not record a schedule warning; it was not sent", logging.F("scheduleId", r.id))
		return false
	}
	return claimed
}

// Announce sends due countdown warnings, best effort: each lead time of a
// run is tried at most once, during its minute and before the cutoff, with
// at most one warning per server at a time.
func (s *Service) Announce(ctx context.Context, now int64) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+columns+" FROM schedules ORDER BY rowid LIMIT ?", MaxTotal)
	if err != nil {
		return
	}
	var saved []*row
	for rows.Next() {
		if r, err := scan(rows); err == nil {
			saved = append(saved, r)
		}
	}
	rows.Close()
	// Warnings are best effort; a partial list waits for the next check.
	if rows.Err() != nil {
		return
	}
	for _, r := range saved {
		s.mu.Lock()
		busy := s.announcing[r.serverID]
		s.mu.Unlock()
		if busy {
			continue
		}
		data, err := ParseSaved(r.input)
		if err != nil || !data.Enabled || data.Announcements == nil || oneTimeCompleted(r, data) {
			continue
		}
		runAt, ok := schedtime.NextRun(data.Clock(), now, r.lastSlot.String)
		if !ok {
			continue
		}
		lead := 0
		for _, minutes := range data.Announcements.LeadMinutes {
			if now >= runAt-int64(minutes)*60_000 && now < runAt-int64(minutes-1)*60_000 {
				lead = minutes
				break
			}
		}
		slot := schedtime.Slot(data.Clock(), runAt)
		if lead == 0 || slot == "" || runAt-now < (announceCutoff+announceMinimum).Milliseconds() {
			continue
		}
		if !s.claimAnnouncement(ctx, r, slot, lead, now) {
			continue
		}
		s.mu.Lock()
		if s.announcing == nil {
			s.announcing = map[string]bool{}
		}
		s.announcing[r.serverID] = true
		s.mu.Unlock()
		s.warnings.Add(1)
		go func() {
			defer s.warnings.Done()
			defer func() {
				s.mu.Lock()
				delete(s.announcing, r.serverID)
				s.mu.Unlock()
			}()
			// Console output and errors may contain secrets; a skipped warning
			// never affects the scheduled action.
			if err := s.announce(context.WithoutCancel(ctx), r, data, runAt, lead); err != nil {
				log.Debug("Skipped a restart countdown warning", logging.F("scheduleId", r.id))
			}
		}()
	}
}

func (s *Service) announce(ctx context.Context, r *row, data Input, runAt int64, lead int) error {
	if s.Console == nil {
		return errors.New("no console")
	}
	deadline := time.UnixMilli(runAt).Add(-announceCutoff)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	assertCurrent := func() (*db.SessionUser, *identity.Server, error) {
		latest, err := s.get(ctx, s.DB, r.id, r.serverID)
		if err != nil || latest == nil || latest.revision != r.revision || ctx.Err() != nil || time.Now().After(deadline) {
			return nil, nil, errors.New("the announcement is no longer current")
		}
		user, binding, err := s.authority(ctx, s.DB, latest, data)
		if err != nil {
			return nil, nil, err
		}
		if _, err := s.Authz.Assert(ctx, user, r.serverID, authz.ConsoleExecute); err != nil {
			return nil, nil, err
		}
		return user, binding, nil
	}
	user, binding, err := assertCurrent()
	if err != nil {
		return err
	}
	initial, err := s.Servers.ResolveAuthorized(ctx, user, r.serverID, authz.ConsoleExecute, binding.BindingRevision)
	if err != nil {
		return err
	}
	// A job that needs the server cancels the warning; the locks are held only
	// until the console command's cleanup finishes.
	release, err := s.Locks.Acquire(initial.LockKeys, cancel)
	if err != nil {
		return err
	}
	defer release()
	current, err := s.Servers.ResolveAuthorized(ctx, user, r.serverID, authz.ConsoleExecute, binding.BindingRevision)
	if err != nil {
		return err
	}
	template := ""
	if current.Container.Console != nil && current.Container.Console.SupportsBroadcast {
		template = broadcastTemplate(current)
	}
	if template == "" {
		return nil
	}
	plural := "s"
	if lead == 1 {
		plural = ""
	}
	message := fmt.Sprintf("Server restart in %d minute%s.", lead, plural)
	return s.Console.Broadcast(ctx, current, strings.ReplaceAll(template, "{message}", message), func() error {
		if _, _, err := assertCurrent(); err != nil {
			return err
		}
		_, err := s.Identity.AssertObservedBinding(ctx, r.serverID, current.Observation, binding.BindingRevision)
		return err
	})
}

// Wait lets warnings settle before the database closes.
func (s *Service) Wait() { s.warnings.Wait() }

func broadcastTemplate(server *servers.Context) string {
	return games.BroadcastTemplate(server.Container.GameType, server.Container.Labels)
}
