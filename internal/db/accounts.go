package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
)

// Role is an account role.
type Role string

const (
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleViewer   Role = "viewer"
)

// ValidRole reports whether value names a role.
func ValidRole(value string) bool {
	switch Role(value) {
	case RoleAdmin, RoleOperator, RoleViewer:
		return true
	}
	return false
}

// User is an account without its password verifier.
type User struct {
	ID        string
	Username  string
	Role      Role
	Disabled  bool
	CreatedAt int64
}

// UserRecord includes the password verifier.
type UserRecord struct {
	User
	PasswordHash string
}

// SessionUser is the authenticated principal of a request.
type SessionUser struct {
	ID       string
	Username string
	Role     Role
}

// Session is one sign-in listed on the account page.
type Session struct {
	ID         string
	CreatedAt  int64
	ExpiresAt  int64
	LastSeenAt int64
	IPAddress  string
	UserAgent  string
	Current    bool
}

// LoginThrottle is the state of one failure counter.
type LoginThrottle struct {
	Failures     int
	BlockedUntil int64
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// CountUsers returns the number of accounts.
func (d *DB) CountUsers(ctx context.Context) (int, error) {
	var count int
	err := d.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&count)
	return count, err
}

// CreateUser inserts an account.
func (d *DB) CreateUser(ctx context.Context, user UserRecord) error {
	_, err := d.ExecContext(ctx, `INSERT INTO users
    (id, username, password_hash, role, disabled, created_at, updated_at)
    VALUES (?, ?, ?, ?, ?, ?, ?)`,
		user.ID, user.Username, user.PasswordHash, string(user.Role), boolInt(user.Disabled), user.CreatedAt, user.CreatedAt)
	return err
}

func (d *DB) findUser(ctx context.Context, column, value string) (*UserRecord, error) {
	var user UserRecord
	var disabled int
	var role string
	err := d.QueryRowContext(ctx, `SELECT id, username, password_hash, role, disabled, created_at
    FROM users WHERE `+column+` = ?`, value).Scan(&user.ID, &user.Username, &user.PasswordHash, &role, &disabled, &user.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	user.Role, user.Disabled = Role(role), disabled == 1
	return &user, nil
}

// FindUserByUsername looks up an account case-insensitively.
func (d *DB) FindUserByUsername(ctx context.Context, username string) (*UserRecord, error) {
	return d.findUser(ctx, "username", username)
}

// FindUserByID looks up an account.
func (d *DB) FindUserByID(ctx context.Context, id string) (*UserRecord, error) {
	return d.findUser(ctx, "id", id)
}

// ListUsers lists accounts by username.
func (d *DB) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, username, role, disabled, created_at
    FROM users ORDER BY username COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var user User
		var role string
		var disabled int
		if err := rows.Scan(&user.ID, &user.Username, &role, &disabled, &user.CreatedAt); err != nil {
			return nil, err
		}
		user.Role, user.Disabled = Role(role), disabled == 1
		users = append(users, user)
	}
	return users, rows.Err()
}

// CountEnabledAdmins counts administrators who can sign in.
func (d *DB) CountEnabledAdmins(ctx context.Context) (int, error) {
	var count int
	err := d.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0").Scan(&count)
	return count, err
}

// UpdateUserAccess changes a role and disabled flag. Disabling signs the
// account out everywhere.
func (d *DB) UpdateUserAccess(ctx context.Context, id string, role Role, disabled bool) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE users SET role = ?, disabled = ?, updated_at = ? WHERE id = ?",
			string(role), boolInt(disabled), Now(), id); err != nil {
			return err
		}
		if disabled {
			_, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", id)
			return err
		}
		return nil
	})
}

// UpdateUserPassword replaces a password and revokes every session.
func (d *DB) UpdateUserPassword(ctx context.Context, id, passwordHash string) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?",
			passwordHash, Now(), id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", id)
		return err
	})
}

// UpgradeUserPasswordHash replaces a verifier with stronger parameters
// without signing the account out.
func (d *DB) UpgradeUserPasswordHash(ctx context.Context, id, passwordHash string) error {
	_, err := d.ExecContext(ctx, "UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?", passwordHash, Now(), id)
	return err
}

// DeleteUser removes an account; sessions and grants cascade.
func (d *DB) DeleteUser(ctx context.Context, id string) error {
	_, err := d.ExecContext(ctx, "DELETE FROM users WHERE id = ?", id)
	return err
}

// DeleteUserSessions signs an account out everywhere.
func (d *DB) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := d.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", userID)
	return err
}

// SessionInput describes a new session.
type SessionInput struct {
	SessionID string
	TokenHash string
	UserID    string
	CreatedAt int64
	ExpiresAt int64
	IPAddress string
	UserAgent string
	// PasswordHash, when set, is the hash the password was verified against.
	// The session is created only while the account still has it and is
	// enabled, in the same statement, so a concurrent reset or disable wins.
	PasswordHash string
}

// ErrAccountChanged means the account's password or access changed after
// its password was verified.
var ErrAccountChanged = errors.New("account changed during sign-in")

// CreateSession stores a session and drops expired ones.
func (d *DB) CreateSession(ctx context.Context, input SessionInput) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", input.CreatedAt); err != nil {
			return err
		}
		values := []any{input.TokenHash, input.SessionID, input.UserID, input.CreatedAt, input.ExpiresAt, input.CreatedAt,
			nullable(input.IPAddress), nullable(input.UserAgent)}
		insert := `INSERT INTO sessions
      (token_hash, session_id, user_id, created_at, expires_at, last_seen_at, ip_address, user_agent)
      VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
		if input.PasswordHash != "" {
			insert = `INSERT INTO sessions
      (token_hash, session_id, user_id, created_at, expires_at, last_seen_at, ip_address, user_agent)
      SELECT ?, ?, ?, ?, ?, ?, ?, ? FROM users WHERE id = ? AND password_hash = ? AND disabled = 0`
			values = append(values, input.UserID, input.PasswordHash)
		}
		result, err := tx.ExecContext(ctx, insert, values...)
		if err != nil {
			return err
		}
		if created, err := result.RowsAffected(); err != nil || created != 1 {
			if err == nil {
				err = ErrAccountChanged
			}
			return err
		}
		return nil
	})
}

// ListUserSessions lists unexpired sessions, newest activity first.
func (d *DB) ListUserSessions(ctx context.Context, userID, currentTokenHash string) ([]Session, error) {
	rows, err := d.QueryContext(ctx, `SELECT session_id, token_hash, created_at, expires_at, last_seen_at,
    ip_address, user_agent FROM sessions WHERE user_id = ? AND expires_at > ? ORDER BY last_seen_at DESC`, userID, Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		var session Session
		var tokenHash string
		var ip, agent sql.NullString
		if err := rows.Scan(&session.ID, &tokenHash, &session.CreatedAt, &session.ExpiresAt, &session.LastSeenAt, &ip, &agent); err != nil {
			return nil, err
		}
		if session.ID == "" {
			continue
		}
		session.IPAddress, session.UserAgent = ip.String, agent.String
		session.Current = tokenHash == currentTokenHash
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

// DeleteUserSessionByID revokes one of the account's sessions.
func (d *DB) DeleteUserSessionByID(ctx context.Context, userID, sessionID string) (bool, error) {
	result, err := d.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ? AND session_id = ?", userID, sessionID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}

const lastSeenWriteInterval = 60_000

// FindSessionUser resolves a session token hash to its current account.
// Expired sessions and disabled accounts are removed and rejected.
func (d *DB) FindSessionUser(ctx context.Context, tokenHash string, now int64) (*SessionUser, error) {
	var user SessionUser
	var role string
	var disabled int
	var expiresAt, lastSeenAt int64
	err := d.QueryRowContext(ctx, `SELECT users.id, users.username, users.role, users.disabled,
    sessions.expires_at, sessions.last_seen_at
    FROM sessions JOIN users ON users.id = sessions.user_id WHERE sessions.token_hash = ?`, tokenHash).
		Scan(&user.ID, &user.Username, &role, &disabled, &expiresAt, &lastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if disabled == 1 || expiresAt <= now {
		return nil, d.DeleteSession(ctx, tokenHash)
	}
	// Every authenticated request lands here, so avoid a write per request. The
	// value only drives the session list and coarse activity display.
	if now-lastSeenAt >= lastSeenWriteInterval {
		if _, err := d.ExecContext(ctx, "UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?", now, tokenHash); err != nil {
			return nil, err
		}
	}
	user.Role = Role(role)
	return &user, nil
}

// DeleteSession removes one session.
func (d *DB) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := d.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = ?", tokenHash)
	return err
}

// Throttle state is read and written inside one transaction so concurrent
// failures cannot lose counts.
func getThrottle(ctx context.Context, tx *sql.Tx, key string, now, windowMs int64) (LoginThrottle, int64, bool, error) {
	var failures int
	var started, blocked int64
	err := tx.QueryRowContext(ctx, `SELECT failures, window_started_at, blocked_until
    FROM login_attempts WHERE attempt_key = ?`, key).Scan(&failures, &started, &blocked)
	if errors.Is(err, sql.ErrNoRows) {
		return LoginThrottle{}, 0, false, nil
	}
	if err != nil {
		return LoginThrottle{}, 0, false, err
	}
	if blocked <= now && now-started >= windowMs {
		_, err := tx.ExecContext(ctx, "DELETE FROM login_attempts WHERE attempt_key = ?", key)
		return LoginThrottle{}, 0, false, err
	}
	return LoginThrottle{Failures: failures, BlockedUntil: blocked}, started, true, nil
}

// GetLoginThrottle returns the active failure count and block for a key. An
// expired window is cleared.
func (d *DB) GetLoginThrottle(ctx context.Context, key string, now, windowMs int64) (LoginThrottle, error) {
	var result LoginThrottle
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		result, _, _, err = getThrottle(ctx, tx, key, now, windowMs)
		return err
	})
	return result, err
}

const loginPruneInterval = 200

var (
	pruneMu            sync.Mutex
	failuresSincePrune int
)

// RecordLoginFailure counts a failure and blocks the key for blockMs once it
// reaches maxFailures. blockMs <= 0 means windowMs.
func (d *DB) RecordLoginFailure(ctx context.Context, key string, now, windowMs int64, maxFailures int, blockMs int64) (LoginThrottle, error) {
	if blockMs <= 0 {
		blockMs = windowMs
	}
	var result LoginThrottle
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		current, started, found, err := getThrottle(ctx, tx, key, now, windowMs)
		if err != nil {
			return err
		}
		failures := current.Failures + 1
		blocked := current.BlockedUntil
		if failures >= maxFailures {
			blocked = now + blockMs
		}
		if !found {
			started = now
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO login_attempts
      (attempt_key, failures, window_started_at, blocked_until, updated_at)
      VALUES (?, ?, ?, ?, ?)
      ON CONFLICT(attempt_key) DO UPDATE SET
        failures = excluded.failures, blocked_until = excluded.blocked_until, updated_at = excluded.updated_at`,
			key, failures, started, blocked, now)
		result = LoginThrottle{Failures: failures, BlockedUntil: blocked}
		return err
	})
	if err != nil {
		return result, err
	}
	pruneMu.Lock()
	failuresSincePrune++
	prune := failuresSincePrune >= loginPruneInterval
	if prune {
		failuresSincePrune = 0
	}
	pruneMu.Unlock()
	if prune {
		err = d.PruneLoginAttempts(ctx, now, windowMs)
	}
	return result, err
}

// ClearLoginThrottle drops a failure counter.
func (d *DB) ClearLoginThrottle(ctx context.Context, key string) error {
	_, err := d.ExecContext(ctx, "DELETE FROM login_attempts WHERE attempt_key = ?", key)
	return err
}

// PruneLoginAttempts drops rows that no longer block and whose window has long
// since closed. Every distinct source IP creates a row, so a distributed
// attack would otherwise leave them behind indefinitely.
func (d *DB) PruneLoginAttempts(ctx context.Context, now, windowMs int64) error {
	_, err := d.ExecContext(ctx, "DELETE FROM login_attempts WHERE blocked_until <= ? AND updated_at < ?", now, now-windowMs)
	return err
}

// AuditEntry is one audit record to write.
type AuditEntry struct {
	UserID     string
	Action     string
	TargetType string
	TargetID   string
	// Details is encoded as JSON; nil stores NULL.
	Details   any
	IPAddress string
}

const auditPruneInterval = 500

var (
	auditMu               sync.Mutex
	auditWritesSincePrune int
)

// auditMaxRows is the rolling window of audit entries kept.
const auditMaxRows = 100_000

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// WriteAudit records an audit entry. Failed logins are audited for
// unauthenticated callers, so the log keeps a rolling window. Pass a
// transaction to make the entry part of the owning mutation.
func (d *DB) WriteAudit(ctx context.Context, q execer, entry AuditEntry) error {
	if q == nil {
		q = d.DB
	}
	var details any
	if entry.Details != nil {
		encoded, err := json.Marshal(entry.Details)
		if err != nil {
			return err
		}
		details = string(encoded)
	}
	userID := entry.UserID
	if _, err := q.ExecContext(ctx, `INSERT INTO audit_log
    (user_id, action, target_type, target_id, details_json, ip_address, created_at)
    VALUES (?, ?, ?, ?, ?, ?, ?)`,
		nullable(userID), entry.Action, nullable(entry.TargetType), nullable(entry.TargetID), details,
		nullable(entry.IPAddress), Now()); err != nil {
		return err
	}
	auditMu.Lock()
	auditWritesSincePrune++
	auditMu.Unlock()
	if _, ok := q.(*sql.DB); ok {
		return d.PruneAuditIfNeeded(ctx)
	}
	return nil
}

// PruneAuditIfNeeded flushes deferred maintenance after the owning mutation
// has committed.
func (d *DB) PruneAuditIfNeeded(ctx context.Context) error {
	auditMu.Lock()
	due := auditWritesSincePrune >= auditPruneInterval
	if due {
		auditWritesSincePrune = 0
	}
	auditMu.Unlock()
	if !due {
		return nil
	}
	return d.PruneAudit(ctx)
}

// PruneAudit keeps the newest auditMaxRows entries.
func (d *DB) PruneAudit(ctx context.Context) error {
	_, err := d.ExecContext(ctx, "DELETE FROM audit_log WHERE id <= (SELECT MAX(id) FROM audit_log) - ?", auditMaxRows)
	return err
}

// Setting reads a JSON setting into target. It reports false when unset.
func (d *DB) Setting(ctx context.Context, key string, target any) (bool, error) {
	var value string
	err := d.QueryRowContext(ctx, "SELECT value_json FROM settings WHERE key=?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(value), target)
}

// SetSetting stores a JSON setting.
func (d *DB) SetSetting(ctx context.Context, q execer, key string, value any) error {
	if q == nil {
		q = d.DB
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO settings(key,value_json) VALUES(?,?)
    ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json`, key, string(encoded))
	return err
}

// DeleteSetting removes a setting.
func (d *DB) DeleteSetting(ctx context.Context, key string) error {
	_, err := d.ExecContext(ctx, "DELETE FROM settings WHERE key=?", key)
	return err
}

// Errors for guarded account changes.
var (
	ErrSetupComplete   = errors.New("an account already exists")
	ErrSessionGone     = errors.New("session revoked")
	ErrPasswordChanged = errors.New("password changed")
	ErrLastAdmin       = errors.New("at least one active admin is required")
	ErrUserNotFound    = errors.New("user not found")
)

// CreateFirstUser inserts the initial administrator only while no account
// exists, so concurrent setup requests create at most one.
func (d *DB) CreateFirstUser(ctx context.Context, user UserRecord, audit AuditEntry) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return ErrSetupComplete
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO users
      (id, username, password_hash, role, disabled, created_at, updated_at) VALUES (?, ?, ?, ?, 0, ?, ?)`,
			user.ID, user.Username, user.PasswordHash, string(user.Role), user.CreatedAt, user.CreatedAt); err != nil {
			return err
		}
		return d.WriteAudit(ctx, tx, audit)
	})
}

// UpgradePasswordHashIfUnchanged swaps in a stronger verifier unless the
// password was changed meanwhile.
func (d *DB) UpgradePasswordHashIfUnchanged(ctx context.Context, id, oldHash, newHash string) error {
	_, err := d.ExecContext(ctx, `UPDATE users SET password_hash = ?, updated_at = ?
    WHERE id = ? AND password_hash = ? AND disabled = 0`, newHash, Now(), id, oldHash)
	return err
}

func sessionValid(ctx context.Context, tx *sql.Tx, userID, tokenHash string, now int64) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions JOIN users ON users.id = sessions.user_id
    WHERE sessions.token_hash = ? AND users.id = ? AND users.disabled = 0 AND sessions.expires_at > ?`,
		tokenHash, userID, now).Scan(&count)
	return count == 1, err
}

// ChangePassword replaces the password only while the requesting session is
// still valid and the verifier is the one that was checked. Every session is
// revoked, the replacement session is created, and the throttleKey failure
// counter is cleared in the same transaction.
func (d *DB) ChangePassword(ctx context.Context, userID, tokenHash, oldHash, newHash, throttleKey string, session SessionInput, audit AuditEntry) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		valid, err := sessionValid(ctx, tx, userID, tokenHash, session.CreatedAt)
		if err != nil {
			return err
		}
		if !valid {
			return ErrSessionGone
		}
		result, err := tx.ExecContext(ctx, "UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ? AND password_hash = ?",
			newHash, Now(), userID, oldHash)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return ErrPasswordChanged
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions
      (token_hash, session_id, user_id, created_at, expires_at, last_seen_at, ip_address, user_agent)
      VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			session.TokenHash, session.SessionID, userID, session.CreatedAt, session.ExpiresAt, session.CreatedAt,
			nullable(session.IPAddress), nullable(session.UserAgent)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM login_attempts WHERE attempt_key = ?", throttleKey); err != nil {
			return err
		}
		return d.WriteAudit(ctx, tx, audit)
	})
}

// AdminMutation runs fn in a transaction after confirming that the
// requesting session still belongs to an enabled administrator.
func (d *DB) AdminMutation(ctx context.Context, adminID, tokenHash string, now int64, fn func(tx *sql.Tx) error) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		valid, err := sessionValid(ctx, tx, adminID, tokenHash, now)
		if err != nil {
			return err
		}
		if !valid {
			return ErrSessionGone
		}
		var role string
		if err := tx.QueryRowContext(ctx, "SELECT role FROM users WHERE id = ?", adminID).Scan(&role); err != nil {
			return err
		}
		if Role(role) != RoleAdmin {
			return ErrNotAdmin
		}
		return fn(tx)
	})
}

// ErrNotAdmin rejects a principal that is no longer an administrator.
var ErrNotAdmin = errors.New("administrator permission required")

// RemovesLastAdmin reports whether changing target would leave no enabled
// administrator.
func RemovesLastAdmin(ctx context.Context, tx *sql.Tx, targetID string, role Role, disabled bool) (bool, error) {
	var currentRole string
	var currentDisabled int
	if err := tx.QueryRowContext(ctx, "SELECT role, disabled FROM users WHERE id = ?", targetID).Scan(&currentRole, &currentDisabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrUserNotFound
		}
		return false, err
	}
	if Role(currentRole) != RoleAdmin || currentDisabled == 1 || (role == RoleAdmin && !disabled) {
		return false, nil
	}
	var count int
	err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0").Scan(&count)
	return count <= 1, err
}
