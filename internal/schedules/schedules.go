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
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/schedtime"
	"github.com/Crunging/Ludock/internal/servers"
)

var log = logging.New("schedules")

const (
	MaxPayloadBytes = 4 * 1024
	MaxPerServer    = 100
	MaxTotal        = 1000
	announceCutoff  = 15 * time.Second
	announceMinimum = 15 * time.Second
)

const (
	OwnerMissing        = "owner_missing"
	OwnerDisabled       = "owner_disabled"
	OwnerAccessRemoved  = "owner_access_removed"
	ActionAccessRemoved = "action_access_removed"
	SharingPaused       = "sharing_paused"
	BindingUnavailable  = "binding_unavailable"
	Unavailable         = "unavailable"
)

var unavailableMessages = map[string]string{
	OwnerMissing:        "Suspended: the schedule owner no longer exists",
	OwnerDisabled:       "Suspended: the schedule owner's account is disabled",
	OwnerAccessRemoved:  "Suspended: the schedule owner can no longer set up schedules",
	ActionAccessRemoved: "Suspended: the schedule owner can no longer do this action",
	SharingPaused:       "Paused: the server's game or data folders changed, and an administrator needs to confirm it",
	BindingUnavailable:  "Suspended: Ludock can’t find this server in Docker",
	Unavailable:         "Suspended: this schedule can’t run right now; check the server and the schedule",
}

type UnavailableError struct {
	Reason string
	Public *apperr.Error
}

func unavailable(reason string) *UnavailableError {
	code, status := "SCHEDULE_REVOKED", 403
	if reason == SharingPaused {
		code, status = "SCHEDULE_SHARING_PAUSED", 409
	} else if reason == BindingUnavailable {
		code, status = "SCHEDULE_BINDING_UNAVAILABLE", 409
	}
	return &UnavailableError{Reason: reason, Public: apperr.New(code, status, unavailableMessages[reason])}
}

func (e *UnavailableError) Error() string { return e.Public.Message }

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

type Service struct {
	DB       *db.DB
	Authz    *authz.Service
	Identity *identity.Store
	Runner   *ops.Runner
	Servers  *servers.Service
	Locks    *locks.Table
	// Queue queues an alert inside tx (notify.Service.QueueIn), with the
	// skip that raises it; Alerted wakes delivery once tx commits. Either
	// may be nil.
	Queue   func(ctx context.Context, tx *sql.Tx, alert notify.Alert, key, message string) (bool, error)
	Alerted func()
	Console Broadcaster
	Players Players

	mu         sync.Mutex
	announcing map[string]bool
	// checked is when waiting runs last asked for each server's players,
	// latest first.
	checked  map[string][2]int64
	warnings sync.WaitGroup
	// calledOff, set by tests, observes call-offs instead of sending them.
	calledOff func(serverID, action string, binding int64)
}

type row struct {
	id, serverID, ownerID, input   string
	bindingRevision, revision      int64
	lastSlot, lastResult, lastOpID sql.NullString
	lastRunAt, changedAt           sql.NullInt64
	createdAt                      int64
}

const columns = "id,server_id,COALESCE(owner_id,''),input_json,binding_revision,revision,last_slot,last_result,last_operation_id,last_run_at,created_at,changed_at"

func scan(s interface{ Scan(...any) error }) (*row, error) {
	var r row
	return &r, s.Scan(&r.id, &r.serverID, &r.ownerID, &r.input, &r.bindingRevision, &r.revision, &r.lastSlot,
		&r.lastResult, &r.lastOpID, &r.lastRunAt, &r.createdAt, &r.changedAt)
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// authority confirms the owner can still run this schedule now.
func (s *Service) authority(ctx context.Context, r *row, data Input) (*db.SessionUser, *identity.Server, error) {
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
	// Inspect assigned grants before the server's state, so paused sharing
	// is not misreported as a removed grant.
	if user.Role != db.RoleAdmin {
		grant, err := s.Authz.UserGrant(ctx, user.ID, r.serverID)
		if err != nil {
			return nil, nil, err
		}
		if user.Role != db.RoleMember || grant == nil || !slices.Contains(grant.Capabilities, authz.ServerView) ||
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
	if logical.SharingPaused && user.Role != db.RoleAdmin {
		return nil, nil, unavailable(SharingPaused)
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

type Schedule struct {
	Input
	ID       string
	ServerID string
	OwnerID  string
	// OwnerName is the owner's username, or "" once the account is gone.
	OwnerName string
	// CommandHidden: the viewer can't send console commands, so another
	// owner's command is left out.
	CommandHidden            bool
	LastResult               string
	LastOperation            *ops.Job
	LastRunAt                int64
	LastSlot                 string
	Revision                 int64
	NextRunAt                int64 // 0 when none
	NextRunUnavailableReason string
	CompletedAt              int64 // set once a one-time run was queued, skipped, or missed
	// WaitingUntil is when a run waiting for players to leave stops waiting.
	WaitingUntil int64
}

// WaitingResult marks a run waiting for an empty server. The run spent its
// slot, with last_run_at its scheduled minute and no operation yet. Edits,
// pauses, and the run's own end replace it in the same write.
const WaitingResult = "Waiting for players to leave"

// stillWaitingSQL limits a write that ends a wait to a run still waiting.
const stillWaitingSQL = " AND last_result='" + WaitingResult + "' AND last_run_at IS NOT NULL AND last_operation_id IS NULL"

func waiting(r *row) bool {
	return r.lastResult.String == WaitingResult && r.lastRunAt.Valid && !r.lastOpID.Valid
}

// waitEnds is when a waiting run stops waiting.
func waitEnds(r *row, data Input) int64 {
	return r.lastRunAt.Int64 + int64(data.WaitForEmpty.Minutes)*60_000
}

func oneTimeCompleted(r *row, data Input) bool {
	return data.Kind == "once" && r.lastSlot.String == schedtime.OneTimeSlot(data.Clock()) && !waiting(r)
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
	if err != nil || job == nil || job.ServerID != r.serverID || job.Input["scheduleId"] != r.id {
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
	if waiting(r) && data.WaitForEmpty != nil {
		view.WaitingUntil = waitEnds(r, data)
	}
	if data.Enabled && !completed {
		if err := check(r, data); err != nil {
			var reason *UnavailableError
			if errors.As(err, &reason) {
				view.NextRunUnavailableReason = reason.Reason
			} else {
				view.NextRunUnavailableReason = Unavailable
			}
		} else if view.WaitingUntil != 0 && data.Kind == "once" {
			// The one run is under way.
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

// authorityState reads, through q alone, every row the authority checks depend
// on: each user's role, status, and grant on the server, and the server's
// binding. Writes read it before their checks and again inside their
// transaction, and stop if anything changed.
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
	var paused int
	var revision int64
	var fingerprint, container sql.NullString
	err := q.QueryRowContext(ctx, `SELECT status, sharing_paused, binding_revision, binding_fingerprint, container_id
    FROM logical_servers WHERE id = ?`, serverID).Scan(&status, &paused, &revision, &fingerprint, &container)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	state = append(state, []any{status, paused, revision, fingerprint, container})
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

// write runs fn in a transaction that first confirms, through tx, the
// requester's exact session and that the authority state read before the
// checks is unchanged. Authority that is being an administrator is checked as
// db.AdminMutation does.
func (s *Service) write(ctx context.Context, actor *db.SessionUser, tokenHash string, admin bool, state, serverID string, users []string,
	fn func(*sql.Tx) error) error {
	within := func(tx *sql.Tx) error {
		if err := assertUnchanged(ctx, tx, state, serverID, users...); err != nil {
			return err
		}
		return fn(tx)
	}
	if admin {
		return authz.AdminMutationError(s.DB.AdminMutation(ctx, actor.ID, tokenHash, db.Now(), within))
	}
	return authz.AdminMutationError(s.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := db.AssertSession(ctx, tx, actor.ID, tokenHash, db.Now()); err != nil {
			return err
		}
		return within(tx)
	}))
}

// List lists every schedule on a server, whoever owns it. Another owner's
// console command is shown only to someone who could send it themselves.
func (s *Service) List(ctx context.Context, actor *db.SessionUser, serverID string) ([]Schedule, error) {
	current, err := s.manager(ctx, actor, serverID, true)
	if err != nil {
		return nil, err
	}
	commands := current.Role == db.RoleAdmin
	if !commands {
		if commands, err = s.Authz.Has(ctx, current, serverID, authz.ConsoleExecute); err != nil {
			return nil, err
		}
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT "+columns+" FROM schedules WHERE server_id=? ORDER BY created_at,id LIMIT ?", serverID, MaxPerServer)
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
		_, _, err := s.authority(ctx, r, data)
		outcomes[key] = err
		return err
	}
	names := map[string]string{}
	ownerName := func(id string) string {
		name, ok := names[id]
		if !ok {
			if user, err := s.DB.FindUserByID(ctx, id); err == nil && user != nil {
				name = user.Username
			}
			names[id] = name
		}
		return name
	}
	now := db.Now()
	var result []Schedule
	for _, r := range saved {
		view, err := s.public(ctx, r, now, check)
		if err != nil {
			// A damaged saved schedule is still listed for removal.
			view = &Schedule{ID: r.id, ServerID: r.serverID, OwnerID: r.ownerID, Revision: r.revision,
				LastResult: r.lastResult.String, NextRunUnavailableReason: Unavailable}
		}
		view.OwnerName = ownerName(r.ownerID)
		if !commands {
			if view.Command != "" && r.ownerID != current.ID {
				view.Command, view.CommandHidden = "", true
			}
			// A command's reply is console output.
			if operation := view.LastOperation; operation != nil && operation.Result.Reply != "" {
				stripped := *operation
				stripped.Result.Reply = ""
				view.LastOperation = &stripped
			}
		}
		result = append(result, *view)
	}
	return result, nil
}

func (s *Service) check(ctx context.Context) func(*row, Input) error {
	return func(r *row, data Input) error {
		_, _, err := s.authority(ctx, r, data)
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

// Create saves a schedule owned by actor, signed in with tokenHash.
func (s *Service) Create(ctx context.Context, actor *db.SessionUser, tokenHash, serverID string, data Input) (*Schedule, error) {
	return s.create(ctx, actor, tokenHash, serverID, data, false)
}

// CreateAsAdmin is Create for work only an administrator may ask for.
func (s *Service) CreateAsAdmin(ctx context.Context, actor *db.SessionUser, tokenHash, serverID string, data Input) (*Schedule, error) {
	return s.create(ctx, actor, tokenHash, serverID, data, true)
}

func (s *Service) create(ctx context.Context, actor *db.SessionUser, tokenHash, serverID string, data Input, admin bool) (*Schedule, error) {
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
	if _, _, err := s.authority(ctx, &row{id: id, serverID: serverID, ownerID: actor.ID,
		bindingRevision: binding.BindingRevision}, data); err != nil {
		return nil, err
	}
	if err := s.assertListsPlayers(ctx, actor, serverID, data); err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(data)
	// Access revoked after the checks above plants nothing.
	err = s.write(ctx, actor, tokenHash, admin, state, serverID, []string{actor.ID}, func(tx *sql.Tx) error {
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
		if _, err := tx.ExecContext(ctx, "INSERT INTO schedules(id,server_id,owner_id,input_json,binding_revision,created_at,changed_at) VALUES(?,?,?,?,?,?,?)",
			id, serverID, actor.ID, string(encoded), binding.BindingRevision, db.Now(), db.Now()); err != nil {
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
	if err == nil && r == nil {
		err = errNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.public(ctx, r, db.Now(), s.check(ctx))
}

// CountByOwner counts the schedules each account set up.
func (s *Service) CountByOwner(ctx context.Context) (map[string]int, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT owner_id,COUNT(*) FROM schedules WHERE owner_id IS NOT NULL GROUP BY owner_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var owner string
		var count int
		if err := rows.Scan(&owner, &count); err != nil {
			return nil, err
		}
		counts[owner] = count
	}
	return counts, rows.Err()
}

var errNotFound = apperr.New("NOT_FOUND", 404, "Schedule not found")

func (s *Service) Delete(ctx context.Context, actor *db.SessionUser, tokenHash, serverID, id string) error {
	state, err := authorityState(ctx, s.DB, serverID, actor.ID)
	if err != nil {
		return err
	}
	current, err := s.manager(ctx, actor, serverID, true)
	if err != nil {
		return err
	}
	action, heard := "", int64(0)
	// A session, grant, or role revoked after the check above stops the
	// deletion.
	err = s.write(ctx, actor, tokenHash, current.Role == db.RoleAdmin, state, serverID, []string{actor.ID}, func(tx *sql.Tx) error {
		r, err := s.get(ctx, tx, id, serverID)
		if err != nil {
			return err
		}
		if r == nil || (current.Role != db.RoleAdmin && r.ownerID != current.ID) {
			return errNotFound
		}
		// The claims go with the schedule, so they are spent first.
		if data, err := ParseSaved(r.input); err == nil {
			slot, _, _, err := countdown(ctx, tx, r, data, db.Now())
			if err != nil {
				return err
			}
			if heard, err = callOff(ctx, tx, id, slot); err != nil {
				return err
			}
			action = data.Action
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM schedules WHERE id=?", id); err != nil {
			return err
		}
		return s.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: actor.ID, Action: "schedule.deleted", TargetType: "server",
			TargetID: serverID, Details: map[string]string{"scheduleId": id}})
	})
	if err == nil && heard != 0 {
		s.sayCalledOff(serverID, action, heard)
	}
	return err
}

func (s *Service) mutate(ctx context.Context, actor *db.SessionUser, tokenHash, serverID, id string, expected int64, update *Input,
	enabled *bool) (*Schedule, error) {
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
	// An administrator who saves or resumes a schedule whose owner was
	// deleted takes it over.
	var adopter any
	if !pausing && first != nil && first.ownerID == "" && current.Role == db.RoleAdmin {
		adopter, first.ownerID = current.ID, current.ID
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
	// Requester checks can write denial audits and read through the shared
	// pool, so they run before the transaction, which re-reads the same
	// revision and authority state.
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
		if _, _, err := s.authority(ctx, first, data); err != nil {
			return nil, err
		}
		if update != nil {
			if err := s.assertListsPlayers(ctx, actor, serverID, data); err != nil {
				return nil, err
			}
		}
	}
	var result *row
	changed, heard, action := false, int64(0), ""
	err = s.write(ctx, actor, tokenHash, current.Role == db.RoleAdmin, state, serverID, users, func(tx *sql.Tx) error {
		r, err := s.get(ctx, tx, id, serverID)
		if err != nil {
			return err
		}
		// The pinned revision keeps the owner and settings checked above.
		if err := check(r); err != nil {
			return err
		}
		if adopter != nil {
			r.ownerID = current.ID
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
		if previous.key() == data.key() && adopter == nil {
			return nil
		}
		changed = true
		// Every change ends a wait and refuses a queued operation. Otherwise
		// a countdown stands only if its run still happens as announced.
		now := db.Now()
		slot, runAt, queued, err := countdown(ctx, tx, r, previous, now)
		if err != nil {
			return err
		}
		if slot != "" {
			// The edit saves changed_at, which can end a due run's retries.
			edited := *r
			edited.changedAt = sql.NullInt64{Int64: now, Valid: true}
			nextSlot, nextRunAt, _, err := countdown(ctx, tx, &edited, data, now)
			if err != nil {
				return err
			}
			if queued || waiting(r) || !data.Enabled || data.Announcements == nil || data.Action != previous.Action ||
				nextSlot != slot || nextRunAt != runAt {
				if heard, err = callOff(ctx, tx, id, slot); err != nil {
					return err
				}
				action = previous.Action
			}
		}
		encoded, _ := json.Marshal(data)
		// Keep the binding baseline, consumed slot, and history. Every change
		// invalidates work queued from an earlier configuration.
		if _, err := tx.ExecContext(ctx, "UPDATE schedules SET input_json=?,revision=revision+1,changed_at=?,owner_id=COALESCE(owner_id,?) WHERE id=?",
			string(encoded), now, adopter, id); err != nil {
			return err
		}
		r.input, r.revision = string(encoded), r.revision+1
		if waiting(r) {
			ended := "Ended: the schedule was changed while it waited for players to leave"
			if pausing {
				ended = "Ended: the schedule was paused while it waited for players to leave"
			}
			if _, err := tx.ExecContext(ctx, "UPDATE schedules SET last_result=?,last_run_at=? WHERE id=?", ended, now, id); err != nil {
				return err
			}
			r.lastResult, r.lastRunAt = sql.NullString{String: ended, Valid: true}, sql.NullInt64{Int64: now, Valid: true}
		}
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
	if heard != 0 {
		s.sayCalledOff(serverID, action, heard)
	}
	if changed {
		if err := s.DB.PruneAuditIfNeeded(ctx); err != nil {
			log.Warn("Audit retention cleanup failed after schedule update; it will be retried")
		}
	}
	return s.public(ctx, result, db.Now(), s.check(ctx))
}

// Update replaces a schedule's settings at the expected revision.
func (s *Service) Update(ctx context.Context, actor *db.SessionUser, tokenHash, serverID, id string, revision int64, data Input) (*Schedule, error) {
	if err := payloadSize(data); err != nil {
		return nil, err
	}
	if err := data.Validate(); err != nil {
		return nil, err
	}
	return s.mutate(ctx, actor, tokenHash, serverID, id, revision, &data, nil)
}

// SetEnabled pauses or resumes a schedule at the expected revision.
func (s *Service) SetEnabled(ctx context.Context, actor *db.SessionUser, tokenHash, serverID, id string, revision int64, enabled bool) (*Schedule, error) {
	return s.mutate(ctx, actor, tokenHash, serverID, id, revision, nil, &enabled)
}

// errScheduleChanged rolls back a run for a schedule edited since it was
// read; the next run evaluates the current revision.
var errScheduleChanged = errors.New("schedule changed")

var errRetryLater = errors.New("schedule run retried later")

// lastChance is two scheduler ticks: a run still blocked this close to the
// end of its grace period is recorded as skipped instead of retried.
const lastChance = 30_000

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

// dueRun is the run a schedule's evaluation was for, and whether its saved
// configuration could be read.
type dueRun struct {
	slot, action       string
	configurationValid bool
}

func (s *Service) runOne(ctx context.Context, r *row, now int64) {
	var d dueRun
	err := s.evaluate(ctx, r, now, &d)
	// A schedule or access changed since it was read, or another operation
	// holds the server: the next check evaluates the current state.
	if err == nil || errors.Is(err, errScheduleChanged) || errors.Is(err, errAccessChanged) || errors.Is(err, errRetryLater) {
		return
	}
	s.recordSkip(ctx, r, now, d, err)
}

// evaluate decides what a schedule does now: nothing, wait, queue its run,
// or end it with err, recording in d the run that was for.
func (s *Service) evaluate(ctx context.Context, r *row, now int64, d *dueRun) error {
	data, err := ParseSaved(r.input)
	if err != nil {
		return err
	}
	d.action = data.Action
	if waiting(r) {
		d.slot, d.configurationValid = r.lastSlot.String, true
		return s.wait(ctx, r, data, now)
	}
	if !data.Enabled || oneTimeCompleted(r, data) {
		return nil
	}
	// A run blocked by another operation is retried until its grace
	// period ends, except on the last tick before then.
	retryUntil, scheduledAt := int64(0), int64(0)
	if data.Kind == "recurring" {
		d.slot, scheduledAt = schedtime.DueSlot(data.Clock(), now, r.lastSlot.String, r.changedAt.Int64)
		retryUntil = scheduledAt + schedtime.RecurringGraceMs - lastChance
	} else {
		d.slot = schedtime.Slot(data.Clock(), now)
		scheduledAt, _ = schedtime.OneTimeRun(data.Clock())
		retryUntil = scheduledAt + schedtime.OneTimeGraceMs - lastChance
	}
	d.configurationValid = true
	if data.Kind == "once" && d.slot == "" {
		runAt, ok := schedtime.OneTimeRun(data.Clock())
		if ok && now < runAt+schedtime.OneTimeGraceMs {
			return nil
		}
		// The run time passed while Ludock was stopped or the schedule could
		// not be evaluated: record it as missed instead of running late.
		d.slot = schedtime.OneTimeSlot(data.Clock())
		return missed(!ok)
	}
	if d.slot == "" || d.slot == r.lastSlot.String {
		return nil
	}
	state, err := authorityState(ctx, s.DB, r.serverID, r.ownerID)
	if err != nil {
		return err
	}
	user, binding, err := s.authority(ctx, r, data)
	if err != nil {
		return err
	}
	if err := s.Servers.InspectRefusal(ctx, r.serverID, data.Action); errors.Is(err, servers.ErrAutoRemove) {
		return Skip("Skipped: " + servers.AutoRemoveMessage)
	} else if err != nil {
		return err
	}
	if data.Action == ops.KindCommand {
		if err := s.commandReady(ctx, user, r.serverID, binding.BindingRevision); err != nil {
			return err
		}
	}
	// Who is online is decided from a reading asked for once the run is
	// due, so a running server waits for one even when it looks empty.
	if data.WaitForEmpty != nil {
		occupancy, err := s.occupancy(ctx, user, r.serverID, data, binding.BindingRevision)
		if err != nil {
			return err
		}
		if !occupancy.Stopped {
			return s.startWaiting(ctx, r, d.slot, scheduledAt, now)
		}
	}
	err = s.queue(ctx, r, data, state, user, binding, d.slot, now)
	// A stopped server busy with other work keeps the run's full wait.
	if apperr.Is(err, "OPERATION_CONFLICT") && data.WaitForEmpty != nil {
		return s.startWaiting(ctx, r, d.slot, scheduledAt, now)
	}
	// A command is sent now or not at all, never after other work.
	if apperr.Is(err, "OPERATION_CONFLICT") && data.Action == ops.KindCommand {
		return SkipBusy
	}
	if apperr.Is(err, "OPERATION_CONFLICT") && now < retryUntil {
		return errRetryLater
	}
	return err
}

// recordSkip records why a run won't happen, and alerts and calls off its
// countdown when it should.
func (s *Service) recordSkip(ctx context.Context, r *row, now int64, d dueRun, err error) {
	slot, action, configurationValid := d.slot, d.action, d.configurationValid
	reason := "Skipped: the server wasn’t ready, or other work on it was running"
	var unavailableErr *UnavailableError
	var skip Skip
	if !configurationValid {
		reason = "Suspended: saved schedule configuration is invalid; recreate this schedule"
	} else if errors.As(err, &unavailableErr) {
		reason = unavailableErr.Public.Message
	} else if errors.As(err, &skip) {
		reason = string(skip)
	} else if apperr.Is(err, "SCHEDULE_MISSED") || apperr.Is(err, "SCHEDULE_PLAYERS_UNKNOWN") {
		reason = err.Error()
	} else if _, public := apperr.As(err); !public {
		// An internal failure, such as a database error, is not a decision to
		// skip: the slot stays open and the next check retries it.
		log.Warn("Could not evaluate a schedule; it will be retried", logging.F("scheduleId", r.id))
		return
	}
	stillWaiting := ""
	if waiting(r) {
		stillWaiting = stillWaitingSQL
	}
	key := slot
	if key == "" {
		key = "invalid"
	}
	// The skip and its alert commit together. A command is expected to skip
	// while its server is stopped or busy, and alerts nobody.
	changed, alerted := false, false
	recordErr := s.DB.Tx(ctx, func(tx *sql.Tx) error {
		var result sql.Result
		var err error
		if slot != "" {
			result, err = tx.ExecContext(ctx, "UPDATE schedules SET last_slot=?,last_run_at=?,last_operation_id=NULL,last_result=? WHERE id=? AND revision=?"+stillWaiting,
				slot, now, reason, r.id, r.revision)
		} else {
			result, err = tx.ExecContext(ctx, "UPDATE schedules SET last_result=? WHERE id=? AND revision=?"+stillWaiting, reason, r.id, r.revision)
		}
		if err != nil {
			return err
		}
		if rows, err := result.RowsAffected(); err != nil || rows == 0 {
			return err
		}
		changed = true
		if skip != "" || s.Queue == nil {
			return nil
		}
		alerted, err = s.Queue(ctx, tx, notify.FailedWork, "schedule:"+r.id+":"+key, "A scheduled Ludock action could not run. Review its server and permissions.")
		return err
	})
	if recordErr != nil {
		log.Warn("Could not record or notify a skipped schedule run", logging.F("scheduleId", r.id))
		return
	}
	if !changed {
		return
	}
	if alerted && s.Alerted != nil {
		s.Alerted()
	}
	// A run that won't happen calls off its countdown, unless its time
	// passed long ago while Ludock was down.
	if configurationValid && !apperr.Is(err, "SCHEDULE_MISSED") {
		if heard, err := callOff(ctx, s.DB, r.id, slot); err == nil && heard != 0 {
			s.sayCalledOff(r.serverID, action, heard)
		}
	}
	if skip == "" && !configurationValid && r.lastResult.String != reason {
		log.Warn("Saved schedule configuration is invalid; recreate the schedule", logging.F("scheduleId", r.id))
	}
}

// queue enqueues a run's operation and records it on the schedule in one
// transaction, while the owner's access is still as read in state.
func (s *Service) queue(ctx context.Context, r *row, data Input, state string, user *db.SessionUser, binding *identity.Server, slot string, now int64) error {
	// A run that waited goes ahead only if nobody cancelled the wait.
	stillWaiting := ""
	if waiting(r) {
		stillWaiting = stillWaitingSQL
	}
	err := s.DB.Tx(ctx, func(tx *sql.Tx) error {
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
		result, err := tx.ExecContext(ctx, "UPDATE schedules SET last_slot=?,last_run_at=?,last_operation_id=?,last_result=? WHERE id=? AND revision=?"+stillWaiting,
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
}

// Skip ends a run without acting, spending its slot. Skips are expected, so
// they notify nobody.
type Skip string

func (e Skip) Error() string { return string(e) }

const (
	SkipBusy       Skip = "Skipped: the server was busy with other work"
	SkipNotRunning Skip = "Skipped: the server wasn't running"
	SkipNoConsole  Skip = "Skipped: the server's game console isn't available"
)

// commandReady skips a command run unless the server is running, idle, and
// has a game console.
func (s *Service) commandReady(ctx context.Context, user *db.SessionUser, serverID string, revision int64) error {
	if s.Locks.ServerBusy(serverID) {
		return SkipBusy
	}
	server, err := s.Servers.VerifyAuthorized(ctx, user, serverID, authz.ConsoleExecute, revision)
	if err != nil {
		return err
	}
	if server.Container.State != "running" {
		return SkipNotRunning
	}
	if server.Container.Console == nil {
		return SkipNoConsole
	}
	return nil
}

// claimAnnouncement records a warning before it is sent, so a restart
// never repeats it. Claims stay until their slot has passed, since edits can
// return to an earlier slot.
func (s *Service) claimAnnouncement(ctx context.Context, r *row, slot string, lead int, now int64) bool {
	claimed := false
	if err := s.DB.Tx(ctx, func(tx *sql.Tx) error {
		// A waiting run's countdown is claimed only while it still waits.
		query, args := `INSERT OR IGNORE INTO schedule_announcements(schedule_id,slot,lead_minutes)
      SELECT id,?,? FROM schedules WHERE id=? AND revision=?`, []any{slot, lead, r.id, r.revision}
		if waiting(r) {
			query, args = query+stillWaitingSQL+" AND last_slot=?", append(args, r.lastSlot.String)
		}
		result, err := tx.ExecContext(ctx, query, args...)
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
		// A schedule that waits for an empty server warns only when it goes
		// ahead anyway, so its warnings count down to the end of the wait.
		slot, runAt := announcedRun(r, data, now)
		lead := 0
		for _, minutes := range data.Announcements.LeadMinutes {
			if now >= runAt-int64(minutes)*60_000 && now < runAt-int64(minutes-1)*60_000 {
				lead = minutes
				break
			}
		}
		if lead == 0 || slot == "" || runAt-now < (announceCutoff+announceMinimum).Milliseconds() {
			continue
		}
		// A schedule that can't send its warning, such as one whose owner is
		// gone, leaves it for whoever takes the schedule over.
		if !s.canWarn(ctx, r, data) || !s.claimAnnouncement(ctx, r, slot, lead, now) {
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
			if err := s.announce(context.WithoutCancel(ctx), r, data, slot, runAt, lead); err != nil {
				log.Debug("Skipped a schedule countdown warning", logging.F("scheduleId", r.id))
			}
		}()
	}
}

// canWarn checks, without auditing a refusal, the authority say checks
// again as the warning goes out.
func (s *Service) canWarn(ctx context.Context, r *row, data Input) bool {
	user, _, err := s.authority(ctx, r, data)
	if err != nil {
		return false
	}
	allowed, err := s.Authz.Has(ctx, user, r.serverID, authz.ConsoleExecute)
	return err == nil && allowed
}

// announce sends one warning and records the binding it reached players
// on; only a recorded warning is called off later.
func (s *Service) announce(ctx context.Context, r *row, data Input, slot string, runAt int64, lead int) error {
	binding, err := s.say(ctx, r, data, waiting(r), time.UnixMilli(runAt).Add(-announceCutoff), WarningMessage(data.Action, lead))
	if err != nil || binding == 0 {
		return err
	}
	recorded, err := s.DB.ExecContext(ctx, "UPDATE schedule_announcements SET heard_binding=? WHERE schedule_id=? AND slot=? AND lead_minutes=?",
		binding, r.id, slot, lead)
	if err != nil {
		return err
	}
	// The run was called off while the warning went out: its claim was
	// spent unheard, so nobody told the players. This is the only side that
	// knows they heard it.
	if rows, err := recorded.RowsAffected(); err == nil && rows == 0 {
		s.sayCalledOff(r.serverID, data.Action, binding)
	}
	return nil
}

// say sends players message on the owner's authority, before deadline and
// while the schedule is unchanged; stillWaiting also requires r's waiting
// run to be waiting still, not cancelled or ended. It reports the binding
// revision it broadcast on, or 0 when the game can't message players.
func (s *Service) say(ctx context.Context, r *row, data Input, stillWaiting bool, deadline time.Time, message string) (int64, error) {
	if s.Console == nil {
		return 0, errors.New("no console")
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	assertCurrent := func() (*db.SessionUser, *identity.Server, error) {
		latest, err := s.get(ctx, s.DB, r.id, r.serverID)
		if err != nil || latest == nil || latest.revision != r.revision || ctx.Err() != nil || time.Now().After(deadline) ||
			(stillWaiting && (!waiting(latest) || latest.lastSlot != r.lastSlot)) {
			return nil, nil, errors.New("the announcement is no longer current")
		}
		user, binding, err := s.authority(ctx, latest, data)
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
		return 0, err
	}
	initial, err := s.Servers.ResolveAuthorized(ctx, user, r.serverID, authz.ConsoleExecute, binding.BindingRevision)
	if err != nil {
		return 0, err
	}
	// A warning has one chance to go out, so it cancels a player reading and
	// is not cancelled by requests or jobs, which wait for it. The locks are
	// held only until the console command's cleanup finishes, before the
	// announcement cutoff.
	release, err := s.Locks.AcquirePreempting(ctx, initial.LockKeys, nil)
	if err != nil {
		return 0, err
	}
	defer release()
	current, err := s.Servers.ResolveAuthorized(ctx, user, r.serverID, authz.ConsoleExecute, binding.BindingRevision)
	if err != nil {
		return 0, err
	}
	template := ""
	// A run that would stop a --rm server is refused, so nobody hears it's coming.
	if current.Container.Console != nil && current.Container.Console.SupportsBroadcast &&
		!current.Container.Refuses(data.Action) {
		template = broadcastTemplate(current)
	}
	if template == "" {
		return 0, nil
	}
	err = s.Console.Broadcast(ctx, current, strings.ReplaceAll(template, "{message}", message), func() error {
		if _, _, err := assertCurrent(); err != nil {
			return err
		}
		_, err := s.Identity.AssertObservedBinding(ctx, r.serverID, current.Observation, binding.BindingRevision)
		return err
	})
	if err != nil {
		return 0, err
	}
	return binding.BindingRevision, nil
}

// Wait lets warnings settle before the database closes.
func (s *Service) Wait() { s.warnings.Wait() }

func broadcastTemplate(server *servers.Context) string {
	return games.BroadcastTemplate(server.Container.GameType, server.Container.Labels)
}
