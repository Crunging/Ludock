// Package authz decides what an account may do with a logical server.
// Discovery is not permission: only grants make a server visible to
// non-administrators, within their role's ceiling.
package authz

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/logging"
)

var log = logging.New("authorization")

type Capability string

const (
	ServerView      Capability = "server.view"
	ServerStart     Capability = "server.start"
	ServerStop      Capability = "server.stop"
	ServerRestart   Capability = "server.restart"
	LogsRead        Capability = "logs.read"
	ConsoleExecute  Capability = "console.execute"
	FilesRead       Capability = "files.read"
	FilesWrite      Capability = "files.write"
	BackupsCreate   Capability = "backups.create"
	SchedulesManage Capability = "schedules.manage"
	ServerUpdate    Capability = "server.update"
	ServerRecreate  Capability = "server.recreate"
	BackupsRestore  Capability = "backups.restore"
	BackupsRead     Capability = "backups.read"
	BackupsDelete   Capability = "backups.delete"
	ConsoleShell    Capability = "console.shell"
)

// All lists every capability in display order.
var All = []Capability{ServerView, ServerStart, ServerStop, ServerRestart, LogsRead, ConsoleExecute, FilesRead,
	FilesWrite, BackupsCreate, SchedulesManage, ServerUpdate, ServerRecreate, BackupsRestore, BackupsRead,
	BackupsDelete, ConsoleShell}

var readCapabilities = []Capability{ServerView, LogsRead, FilesRead}

var operatorCapabilities = append(append([]Capability(nil), readCapabilities...),
	ServerStart, ServerStop, ServerRestart, ConsoleExecute, FilesWrite, BackupsCreate, SchedulesManage)

// RoleCeiling lists the capabilities a role may be granted.
func RoleCeiling(role db.Role) []Capability {
	var allowed []Capability
	switch role {
	case db.RoleAdmin:
		return slices.Clone(All)
	case db.RoleOperator:
		allowed = operatorCapabilities
	default:
		allowed = readCapabilities
	}
	var result []Capability
	for _, capability := range All {
		if slices.Contains(allowed, capability) {
			result = append(result, capability)
		}
	}
	return result
}

type Grant struct {
	ServerID     string
	Capabilities []Capability
	UpdatedAt    int64
}

type GrantInput struct {
	ServerID     string
	Capabilities []Capability
}

type Service struct {
	DB      *db.DB
	Servers *identity.Store
}

// CurrentActor resolves the current account state for requests, streams,
// jobs, and schedules. A role carried in a queued payload or a stale session
// lookup is never an authorization authority.
func (s *Service) CurrentActor(ctx context.Context, actor *db.SessionUser) (*db.SessionUser, error) {
	if actor == nil {
		return nil, nil
	}
	user, err := s.DB.FindUserByID(ctx, actor.ID)
	if err != nil || user == nil || user.Disabled {
		return nil, err
	}
	return &db.SessionUser{ID: user.ID, Username: user.Username, Role: user.Role}, nil
}

var errNotAdmin = apperr.New("FORBIDDEN", 403, "Administrator permission required")

// AdminMutationError turns a db.AdminMutation rejection into a public error.
func AdminMutationError(err error) error {
	switch {
	case errors.Is(err, db.ErrSessionGone):
		return apperr.New("AUTHENTICATION_REQUIRED", 401, "Authentication required")
	case errors.Is(err, db.ErrNotAdmin):
		return errNotAdmin
	}
	return err
}

// AssertAdministrator requires the actor to be an enabled administrator now.
func (s *Service) AssertAdministrator(ctx context.Context, actor *db.SessionUser) (*db.SessionUser, error) {
	current, err := s.CurrentActor(ctx, actor)
	if err != nil {
		return nil, err
	}
	if current == nil || current.Role != db.RoleAdmin {
		return nil, errNotAdmin
	}
	return current, nil
}

// parseCapabilities never turns a corrupt grant into broad or default
// authorization.
func parseCapabilities(value string) []Capability {
	var raw []any
	if json.Unmarshal([]byte(value), &raw) != nil {
		return nil
	}
	var result []Capability
	for _, capability := range All {
		if slices.Contains(raw, any(string(capability))) {
			result = append(result, capability)
		}
	}
	return result
}

func encodeCapabilities(capabilities []Capability) string {
	encoded, _ := json.Marshal(capabilities)
	return string(encoded)
}

func (s *Service) UserGrants(ctx context.Context, userID string) ([]Grant, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT server_id, capabilities_json, updated_at
    FROM server_grants WHERE user_id = ? ORDER BY server_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []Grant
	for rows.Next() {
		var grant Grant
		var capabilities string
		if err := rows.Scan(&grant.ServerID, &capabilities, &grant.UpdatedAt); err != nil {
			return nil, err
		}
		grant.Capabilities = parseCapabilities(capabilities)
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// UserGrant reads one assignment without treating binding availability as
// revocation.
func (s *Service) UserGrant(ctx context.Context, userID, serverID string) (*Grant, error) {
	var capabilities string
	grant := Grant{ServerID: serverID}
	err := s.DB.QueryRowContext(ctx, "SELECT capabilities_json, updated_at FROM server_grants WHERE user_id=? AND server_id=?",
		userID, serverID).Scan(&capabilities, &grant.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	grant.Capabilities = parseCapabilities(capabilities)
	return &grant, nil
}

func (s *Service) Effective(ctx context.Context, actor *db.SessionUser, serverID string) ([]Capability, error) {
	capabilities, _, err := s.Access(ctx, actor, serverID)
	return capabilities, err
}

var accessQuery = `SELECT u.role, u.disabled, g.capabilities_json, ` + identity.ServerColumns("s") +
	` FROM logical_servers s LEFT JOIN users u ON u.id = ?
    LEFT JOIN server_grants g ON g.user_id = u.id AND g.server_id = s.id WHERE s.id = ?`

// Access returns the actor's current capabilities on a server and the
// server they apply to, read together in one query. The server is nil when
// it does not exist.
func (s *Service) Access(ctx context.Context, actor *db.SessionUser, serverID string) ([]Capability, *identity.Server, error) {
	if actor == nil {
		return nil, nil, nil
	}
	var role, grant sql.NullString
	var disabled sql.NullInt64
	server, err := identity.ScanServer(s.DB.QueryRowContext(ctx, accessQuery, actor.ID, serverID), &role, &disabled, &grant)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !role.Valid || disabled.Int64 != 0 {
		return nil, server, nil
	}
	user := &db.SessionUser{ID: actor.ID, Role: db.Role(role.String)}
	var assigned []Capability
	if grant.Valid {
		assigned = parseCapabilities(grant.String)
	}
	return roleCapabilities(user, server, assigned), server, nil
}

// EffectiveAll is Effective for many servers, reading the account and its
// grants once. It returns the current account, or nil once it is gone.
func (s *Service) EffectiveAll(ctx context.Context, actor *db.SessionUser, servers []identity.Server) (*db.SessionUser, map[string][]Capability, error) {
	user, err := s.CurrentActor(ctx, actor)
	if err != nil || user == nil {
		return nil, nil, err
	}
	assigned := map[string][]Capability{}
	if user.Role != db.RoleAdmin {
		rows, err := s.DB.QueryContext(ctx, "SELECT server_id, capabilities_json FROM server_grants WHERE user_id = ?", user.ID)
		if err != nil {
			return nil, nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var serverID, raw string
			if err := rows.Scan(&serverID, &raw); err != nil {
				return nil, nil, err
			}
			assigned[serverID] = parseCapabilities(raw)
		}
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
	}
	result := make(map[string][]Capability, len(servers))
	for index := range servers {
		result[servers[index].ID] = roleCapabilities(user, &servers[index], assigned[servers[index].ID])
	}
	return user, result, nil
}

func usable(server *identity.Server) bool {
	return server.Status == identity.StatusActive && !server.ReviewRequired
}

// roleCapabilities applies the account's role to its grant on a server.
func roleCapabilities(user *db.SessionUser, server *identity.Server, assigned []Capability) []Capability {
	if user.Role == db.RoleAdmin {
		// Administrators can inspect status and review metadata even when acting
		// on the container is forbidden. This never authorizes Docker reads or
		// mutation.
		if usable(server) {
			return slices.Clone(All)
		}
		return []Capability{ServerView}
	}
	if !usable(server) || !slices.Contains(assigned, ServerView) {
		return nil
	}
	var result []Capability
	for _, capability := range RoleCeiling(user.Role) {
		if !slices.Contains(assigned, capability) {
			continue
		}
		if capability == FilesWrite && !slices.Contains(assigned, FilesRead) {
			continue
		}
		result = append(result, capability)
	}
	return result
}

func (s *Service) Has(ctx context.Context, actor *db.SessionUser, serverID string, capability Capability) (bool, error) {
	capabilities, err := s.Effective(ctx, actor, serverID)
	return slices.Contains(capabilities, capability), err
}

// ErrServerNotFound covers a nonexistent server, a suspended identity, and
// an unassigned server alike, without disclosing another user's resources.
var ErrServerNotFound = apperr.New("SERVER_NOT_FOUND", 404, "Server not found")

var errInsufficient = apperr.New("FORBIDDEN", 403, "Insufficient server permissions")

// Assert requires a capability and returns the server.
func (s *Service) Assert(ctx context.Context, actor *db.SessionUser, serverID string, capability Capability) (*identity.Server, error) {
	capabilities, server, err := s.Access(ctx, actor, serverID)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(capabilities, ServerView) {
		s.auditDenial(ctx, actor, serverID, capability)
		return nil, ErrServerNotFound
	}
	if !slices.Contains(capabilities, capability) {
		s.auditDenial(ctx, actor, serverID, capability)
		return nil, errInsufficient
	}
	return server, nil
}

func (s *Service) auditDenial(ctx context.Context, actor *db.SessionUser, serverID string, capability Capability) {
	if slices.Contains(readCapabilities, capability) {
		return
	}
	entry := db.AuditEntry{Action: "authorization.denied", TargetType: "server", TargetID: serverID,
		Details: map[string]string{"capability": string(capability)}}
	if current, _ := s.CurrentActor(ctx, actor); current != nil {
		entry.UserID = current.ID
	}
	if err := s.DB.WriteAudit(ctx, nil, entry); err != nil {
		log.Error("Could not write an audit entry", logging.F("action", entry.Action))
	}
}

func invalidGrant(message string) error { return apperr.New("INVALID_GRANT", 400, message) }

func (s *Service) validateGrant(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, role db.Role, input GrantInput) ([]Capability, error) {
	var exists int
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM logical_servers WHERE id = ?", input.ServerID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, ErrServerNotFound
	}
	ceiling := RoleCeiling(role)
	for _, capability := range input.Capabilities {
		if !slices.Contains(ceiling, capability) {
			return nil, invalidGrant("A capability exceeds this user's role")
		}
	}
	var capabilities []Capability
	for _, capability := range All {
		if slices.Contains(input.Capabilities, capability) {
			capabilities = append(capabilities, capability)
		}
	}
	if len(capabilities) > 0 && !slices.Contains(capabilities, ServerView) {
		return nil, invalidGrant("Server access is required for every server capability")
	}
	if slices.Contains(capabilities, FilesWrite) && !slices.Contains(capabilities, FilesRead) {
		return nil, invalidGrant("File writing also requires file reading")
	}
	return capabilities, nil
}

// SetUserGrants atomically replaces a user's assignments; empty grants revoke
// access. The actor's session and admin role are rechecked inside the
// transaction.
func (s *Service) SetUserGrants(ctx context.Context, userID string, inputs []GrantInput, actor *db.SessionUser, tokenHash string) ([]Grant, error) {
	administrator, err := s.AssertAdministrator(ctx, actor)
	if err != nil {
		return nil, err
	}
	if len(inputs) > 1000 {
		return nil, invalidGrant("Too many server assignments")
	}
	seen := map[string]bool{}
	for _, input := range inputs {
		if seen[input.ServerID] {
			return nil, invalidGrant("Duplicate server assignment")
		}
		seen[input.ServerID] = true
	}
	err = s.DB.AdminMutation(ctx, administrator.ID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		var role string
		var disabled int
		err := tx.QueryRowContext(ctx, "SELECT role, disabled FROM users WHERE id = ?", userID).Scan(&role, &disabled)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New("USER_NOT_FOUND", 404, "User not found")
		}
		if err != nil {
			return err
		}
		if db.Role(role) == db.RoleAdmin {
			return invalidGrant("Administrators already have access to all eligible servers")
		}
		type validated struct {
			ServerID     string       `json:"serverId"`
			Capabilities []Capability `json:"capabilities"`
		}
		var grants []validated
		for _, input := range inputs {
			capabilities, err := s.validateGrant(ctx, tx, db.Role(role), input)
			if err != nil {
				return err
			}
			grants = append(grants, validated{input.ServerID, capabilities})
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM server_grants WHERE user_id = ?", userID); err != nil {
			return err
		}
		now := db.Now()
		for _, grant := range grants {
			if len(grant.Capabilities) == 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO server_grants (user_id, server_id, capabilities_json, updated_at)
        VALUES (?, ?, ?, ?)`, userID, grant.ServerID, encodeCapabilities(grant.Capabilities), now); err != nil {
				return err
			}
		}
		if grants == nil {
			grants = []validated{}
		}
		for index := range grants {
			if grants[index].Capabilities == nil {
				grants[index].Capabilities = []Capability{}
			}
		}
		return s.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: administrator.ID, Action: "users.server_grants.updated",
			TargetType: "user", TargetID: userID, Details: map[string]any{"grants": grants}})
	})
	if err != nil {
		return nil, AdminMutationError(err)
	}
	s.DB.PruneAuditIfNeeded(ctx)
	return s.UserGrants(ctx, userID)
}
