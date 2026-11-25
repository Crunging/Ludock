// Package history pages through operations and the audit log. Every page
// rechecks the reader's access; cursors are bound to their filters.
package history

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/ops"
)

type Actor struct {
	ID   string
	Name string
}

// PublicActor is the actor of a history entry, or nil when none is recorded.
func PublicActor(id, name string) *Actor {
	if id == "" && name == "" {
		return nil
	}
	return &Actor{ID: id, Name: name}
}

// Query filters a history page. Zero values mean unset.
type Query struct {
	Limit       int
	Cursor      string
	ServerID    string
	Actor       string
	Action      string
	Status      string
	OperationID string // audit only
	// ActionKeys are audit actions whose displayed names match Action.
	// "<action>#<kind>" matches only entries whose details name that kind,
	// such as one player action.
	ActionKeys []string
	From, To   int64 // inclusive Unix ms; -1 when unset
}

var Statuses = []string{ops.Queued, ops.Running, ops.Succeeded, ops.AlreadyCurrent, ops.Skipped, ops.Failed, ops.Interrupted, ops.Cancelled}

// Normalize validates a query, applying the default limit of 50.
func (q *Query) Normalize() error {
	invalid := apperr.New("INVALID_REQUEST", 400, "Invalid request")
	if q.Limit == 0 {
		q.Limit = 50
	}
	q.Actor, q.Action = strings.TrimSpace(q.Actor), strings.TrimSpace(q.Action)
	if q.Limit < 1 || q.Limit > 250 || len(q.Cursor) > 2048 || len(q.Actor) > 200 || len(q.Action) > 200 ||
		(q.ServerID != "" && !identity.ValidID(q.ServerID)) ||
		(q.Status != "" && !contains(Statuses, q.Status)) ||
		(q.From >= 0 && q.To >= 0 && q.From > q.To) {
		return invalid
	}
	return nil
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

type cursor struct {
	Scope     string `json:"scope"`
	CreatedAt int64  `json:"createdAt"`
	ID        any    `json:"id"`
}

func scope(kind string, q Query) string {
	encoded, _ := json.Marshal([]any{kind, q.ServerID, q.Actor, q.Action, q.Status, q.OperationID, q.From, q.To, q.ActionKeys})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

var errCursor = apperr.New("INVALID_HISTORY_CURSOR", 400, "This history cursor is invalid for the selected filters")

func page(kind string, q Query, alias string) ([]string, []any, string, error) {
	var conditions []string
	var values []any
	scopeKey := scope(kind, q)
	if q.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		var c cursor
		if err != nil || json.Unmarshal(raw, &c) != nil || c.Scope != scopeKey || c.CreatedAt < 0 {
			return nil, nil, "", errCursor
		}
		switch id := c.ID.(type) {
		case string:
			if kind != "operations" || !identity.ValidID(id) {
				return nil, nil, "", errCursor
			}
		case float64:
			if kind != "audit" || id < 1 || id != float64(int64(id)) {
				return nil, nil, "", errCursor
			}
			c.ID = int64(id)
		default:
			return nil, nil, "", errCursor
		}
		conditions = append(conditions, "("+alias+".created_at < ? OR ("+alias+".created_at = ? AND "+alias+".id < ?))")
		values = append(values, c.CreatedAt, c.CreatedAt, c.ID)
	}
	if q.From >= 0 {
		conditions = append(conditions, alias+".created_at >= ?")
		values = append(values, q.From)
	}
	if q.To >= 0 {
		conditions = append(conditions, alias+".created_at <= ?")
		values = append(values, q.To)
	}
	return conditions, values, scopeKey, nil
}

func nextCursor(scopeKey string, createdAt int64, id any) string {
	encoded, _ := json.Marshal(cursor{Scope: scopeKey, CreatedAt: createdAt, ID: id})
	return base64.RawURLEncoding.EncodeToString(encoded)
}

type Operation struct {
	ID, ServerID, Kind, Status, Phase string
	Actor                             *Actor
	// CancelledBy is who cancelled it, for cancelled work.
	CancelledBy          *Actor
	CreatedAt, UpdatedAt int64
	Error                string
	Result               ops.Result
}

// testHook, set only by tests, runs once access is decided.
var testHook func(point string)

type Service struct {
	DB       *db.DB
	Authz    *authz.Service
	Identity *identity.Store
}

// Operations lists operations on servers the actor can view now.
func (s *Service) Operations(ctx context.Context, actor *db.SessionUser, q Query) ([]Operation, string, error) {
	if err := q.Normalize(); err != nil {
		return nil, "", err
	}
	conditions, values, scopeKey, err := page("operations", q, "o")
	if err != nil {
		return nil, "", err
	}
	var candidates []string
	if q.ServerID != "" {
		candidates = []string{q.ServerID}
	} else {
		servers, err := s.Identity.List(ctx, "")
		if err != nil {
			return nil, "", err
		}
		for _, server := range servers {
			candidates = append(candidates, server.ID)
		}
	}
	// Access and the rows are read from one snapshot, so a pause or a moved
	// server committing between them can't show rows the reader may not see.
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	var visible []string
	for _, id := range candidates {
		if ok, err := s.Authz.HasIn(ctx, tx, actor, id, authz.ServerView); err != nil {
			return nil, "", err
		} else if ok {
			visible = append(visible, id)
		}
	}
	if testHook != nil {
		testHook("checked")
	}
	if len(visible) == 0 {
		return nil, "", nil
	}
	conditions = append(conditions, "o.server_id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(visible)), ",")+")")
	for _, id := range visible {
		values = append(values, id)
	}
	if q.Action != "" {
		conditions = append(conditions, "instr(lower(o.kind), lower(?)) > 0")
		values = append(values, q.Action)
	}
	if q.Status != "" {
		conditions = append(conditions, "o.status = ?")
		values = append(values, q.Status)
	}
	if q.Actor != "" {
		conditions = append(conditions, "(o.actor_id = ? COLLATE NOCASE OR instr(lower(u.username), lower(?)) > 0)")
		values = append(values, q.Actor, q.Actor)
	}
	values = append(values, q.Limit+1)
	rows, err := tx.QueryContext(ctx, `SELECT o.id, o.server_id, o.actor_id, u.username,
    o.kind, o.status, o.phase, o.created_at, o.updated_at, o.error, o.result_json, c.username
    FROM operations o LEFT JOIN users u ON u.id = o.actor_id
    LEFT JOIN users c ON o.status = 'cancelled' AND json_valid(o.result_json) AND c.id = json_extract(o.result_json, '`+ops.PathCancelledBy+`')
    WHERE `+strings.Join(conditions, " AND ")+` ORDER BY o.created_at DESC, o.id DESC LIMIT ?`, values...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var result []Operation
	for rows.Next() {
		var operation Operation
		var actorID string
		var username, errorText, resultJSON, canceller sql.NullString
		if err := rows.Scan(&operation.ID, &operation.ServerID, &actorID, &username, &operation.Kind, &operation.Status,
			&operation.Phase, &operation.CreatedAt, &operation.UpdatedAt, &errorText, &resultJSON, &canceller); err != nil {
			return nil, "", err
		}
		operation.Actor = PublicActor(actorID, username.String)
		operation.Error = errorText.String
		if resultJSON.Valid {
			json.Unmarshal([]byte(resultJSON.String), &operation.Result)
		}
		if operation.Status == ops.Cancelled {
			operation.CancelledBy = PublicActor(operation.Result.CancelledBy, canceller.String)
		}
		result = append(result, operation)
	}
	next := ""
	if len(result) > q.Limit {
		last := result[q.Limit-1]
		next = nextCursor(scopeKey, last.CreatedAt, last.ID)
		result = result[:q.Limit]
	}
	return result, next, rows.Err()
}

type AuditEntry struct {
	ID          int64
	Username    string
	Actor       *Actor
	Action      string
	Status      string // outcome recorded in the action, not the operation's current state
	OperationID string
	TargetType  string
	TargetID    string
	Details     any
	IPAddress   string
	CreatedAt   int64
}

var auditOperationIDSQL = db.AuditOperationID("a.")

const auditDetailsActorSQL = "CASE WHEN json_valid(a.details_json) THEN CASE WHEN json_type(a.details_json, '$.actorId') = 'text' THEN json_extract(a.details_json, '$.actorId') END END"

func auditStatusSQL() string {
	var cases []string
	for _, status := range Statuses {
		cases = append(cases, "WHEN substr(a.action, -"+itoa(len(status)+1)+") = '."+status+"' THEN '"+status+"'")
	}
	return "CASE " + strings.Join(cases, " ") + " END"
}

func itoa(value int) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// Audit lists audit entries for administrators.
func (s *Service) Audit(ctx context.Context, q Query) ([]AuditEntry, string, error) {
	if err := q.Normalize(); err != nil {
		return nil, "", err
	}
	if q.OperationID != "" && !identity.ValidID(q.OperationID) {
		return nil, "", apperr.New("INVALID_REQUEST", 400, "Invalid request")
	}
	conditions, values, scopeKey, err := page("audit", q, "a")
	if err != nil {
		return nil, "", err
	}
	actorSQL := "COALESCE(a.user_id, " + auditDetailsActorSQL + ", o.actor_id)"
	statusSQL := auditStatusSQL()
	if q.ServerID != "" {
		conditions = append(conditions, "a.target_type = 'server' AND a.target_id = ?")
		values = append(values, q.ServerID)
	}
	if q.OperationID != "" {
		conditions = append(conditions, auditOperationIDSQL+" = ?")
		values = append(values, q.OperationID)
	}
	if q.Action != "" {
		match := "instr(lower(a.action), lower(?)) > 0"
		values = append(values, q.Action)
		for _, key := range q.ActionKeys {
			if action, kind, ok := strings.Cut(key, "#"); ok {
				match += " OR (a.action LIKE ? ESCAPE '\\' AND json_valid(a.details_json) AND json_extract(a.details_json, '$.action') = ?)"
				values = append(values, strings.NewReplacer("%", "\\%", "_", "\\_").Replace(action)+".%", kind)
				continue
			}
			match += " OR a.action = ?"
			values = append(values, key)
			// Only server work is recorded with its result as a suffix.
			if strings.HasPrefix(key, "server.") {
				match += " OR a.action LIKE ? ESCAPE '\\'"
				values = append(values, strings.NewReplacer("%", "\\%", "_", "\\_").Replace(key)+".%")
			}
		}
		conditions = append(conditions, "("+match+")")
	}
	if q.Status != "" {
		conditions = append(conditions, statusSQL+" = ?")
		values = append(values, q.Status)
	}
	if q.Actor != "" {
		conditions = append(conditions, "("+actorSQL+" = ? COLLATE NOCASE OR instr(lower(COALESCE(u.username, actor_user.username)), lower(?)) > 0)")
		values = append(values, q.Actor, q.Actor)
	}
	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}
	values = append(values, q.Limit+1)
	rows, err := s.DB.QueryContext(ctx, `SELECT a.id, `+actorSQL+`, u.username, COALESCE(u.username, actor_user.username),
    a.action, `+statusSQL+`, `+auditOperationIDSQL+`, a.target_type, a.target_id, a.details_json, a.ip_address, a.created_at
    FROM audit_log a LEFT JOIN users u ON u.id = a.user_id
    LEFT JOIN operations o ON o.id = `+auditOperationIDSQL+` AND a.target_type = 'server' AND o.server_id = a.target_id
    LEFT JOIN users actor_user ON actor_user.id = COALESCE(`+auditDetailsActorSQL+`, o.actor_id)
    `+where+` ORDER BY a.created_at DESC, a.id DESC LIMIT ?`, values...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var result []AuditEntry
	for rows.Next() {
		var entry AuditEntry
		var actorID, username, actorName, status, operationID, targetType, targetID, details, ip sql.NullString
		if err := rows.Scan(&entry.ID, &actorID, &username, &actorName, &entry.Action, &status, &operationID, &targetType,
			&targetID, &details, &ip, &entry.CreatedAt); err != nil {
			return nil, "", err
		}
		entry.Username, entry.Status, entry.TargetType, entry.TargetID, entry.IPAddress =
			username.String, status.String, targetType.String, targetID.String, ip.String
		entry.Actor = PublicActor(actorID.String, actorName.String)
		if identity.ValidID(operationID.String) {
			entry.OperationID = operationID.String
		}
		if details.Valid {
			var parsed any
			// Unreadable historical events stay listed without details.
			if json.Unmarshal([]byte(details.String), &parsed) == nil {
				entry.Details = parsed
			}
		}
		result = append(result, entry)
	}
	next := ""
	if len(result) > q.Limit {
		last := result[q.Limit-1]
		next = nextCursor(scopeKey, last.CreatedAt, last.ID)
		result = result[:q.Limit]
	}
	return result, next, rows.Err()
}
