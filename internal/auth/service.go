package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/textutil"
)

const (
	// SessionCookie names the session cookie.
	SessionCookie = "ludock_session"
	sessionTTLMs  = 7 * 24 * 60 * 60 * 1000

	loginWindowMs              = 15 * 60 * 1000
	loginAccountSourceMax      = 5
	loginSourceMax             = 20
	loginAccountCooldownMin    = 20
	loginAccountCooldownBaseMs = 250
	loginAccountCooldownMaxMs  = 5000
	setupSourceMax             = 20
)

var log = logging.New("auth")

// Client describes where a request came from.
type Client struct {
	IP        string
	UserAgent string
}

func (c Client) source() string {
	if c.IP == "" {
		return "unknown"
	}
	return c.IP
}

// Principal is an authenticated session.
type Principal struct {
	User      db.SessionUser
	Token     string
	TokenHash string
}

// Service implements accounts and sessions.
type Service struct {
	DB     *db.DB
	Authz  *authz.Service
	Setup  *SetupWindow
	Work   *PasswordWork
	Hasher Hasher
	// Now returns Unix milliseconds; tests replace it.
	Now func() int64

	setupScope string
}

// NewService wires a service with production defaults.
func NewService(d *db.DB, authorization *authz.Service, setup *SetupWindow) *Service {
	return &Service{DB: d, Authz: authorization, Setup: setup, Work: NewPasswordWork(), Hasher: Argon2{},
		Now: func() int64 { return time.Now().UnixMilli() }, setupScope: identity.NewUUID()}
}

// HashToken hashes a session token or other opaque credential.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func throttleKey(scope, value string) string { return HashToken(scope + ":" + value) }

func passwordWorkSource(client Client) string {
	return throttleKey("password-work-source", client.source())
}

// ErrBusy is the response to throttled or busy credential work.
func busy(retryAfterMs int64) error {
	return &apperr.Error{Code: "TOO_MANY_ATTEMPTS", Status: 429, Message: "Too many attempts. Try again later.", RetryAfterMs: retryAfterMs}
}

var errAuthRequired = apperr.New("AUTHENTICATION_REQUIRED", 401, "Authentication required")

func (s *Service) tooManyAttempts(ctx context.Context, now int64, keys ...string) (bool, error) {
	for _, key := range keys {
		throttle, err := s.DB.GetLoginThrottle(ctx, key, now, loginWindowMs)
		if err != nil {
			return false, err
		}
		if throttle.BlockedUntil > now {
			return true, nil
		}
	}
	return false, nil
}

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// ValidUsername trims and validates a username.
func ValidUsername(username string) (string, bool) {
	username = strings.TrimSpace(username)
	length := textutil.Length(username)
	return username, length >= 3 && length <= 32 && usernamePattern.MatchString(username)
}

// ValidPassword checks the length policy in UTF-16 units, like the form's
// minlength and maxlength.
func ValidPassword(password string) bool {
	length := textutil.Length(password)
	return length >= PasswordMin && length <= PasswordMax
}

// SetupRequired reports whether no account exists yet.
func (s *Service) SetupRequired(ctx context.Context) (bool, error) {
	count, err := s.DB.CountUsers(ctx)
	return count == 0, err
}

// SetupState reports the setup window.
func (s *Service) SetupState(ctx context.Context) (SetupState, error) {
	required, err := s.SetupRequired(ctx)
	if err != nil {
		return SetupState{}, err
	}
	return s.Setup.State(required), nil
}

// LogSetupInstructions prints a generated setup code to the console once.
// The code belongs on the local console, not in the structured logger whose
// ring buffer administrators can read in the panel.
func (s *Service) LogSetupInstructions(ctx context.Context, console func(string)) {
	if required, err := s.SetupRequired(ctx); err != nil || !required {
		return
	}
	if code := s.Setup.TakeGeneratedCode(); code != "" {
		console("Ludock initial setup code: " + code + "\nEnter this one-time code in the setup page within five minutes.\n")
	}
	log.Info("Initial administrator setup is available", logging.F("durationMinutes", SetupWindowMs/60_000),
		logging.F("closesAt", time.UnixMilli(s.Setup.ExpiresAt).UTC().Format("2006-01-02T15:04:05.000Z")))
}

func (s *Service) newToken() string {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

func (s *Service) sessionInput(userID string, client Client) (db.SessionInput, string) {
	token := s.newToken()
	now := s.Now()
	return db.SessionInput{SessionID: identity.NewUUID(), TokenHash: HashToken(token), UserID: userID,
		CreatedAt: now, ExpiresAt: now + sessionTTLMs, IPAddress: client.IP, UserAgent: client.UserAgent}, token
}

// createSession signs user in. A non-empty verifiedHash is the password hash
// the caller verified; the session is refused if the account changed since.
func (s *Service) createSession(ctx context.Context, user db.SessionUser, client Client, verifiedHash string, audit *db.AuditEntry) (*Principal, error) {
	input, token := s.sessionInput(user.ID, client)
	input.PasswordHash, input.Audit = verifiedHash, audit
	if err := s.DB.CreateSession(ctx, input); err != nil {
		return nil, err
	}
	return &Principal{User: user, Token: token, TokenHash: input.TokenHash}, nil
}

// CompleteSetup creates the first administrator. Code authorization runs
// before validation or password hashing; missing and incorrect codes get the
// same response and count against the source.
func (s *Service) CompleteSetup(ctx context.Context, username, password, code string, client Client) (*Principal, error) {
	required, err := s.SetupRequired(ctx)
	if err != nil {
		return nil, err
	}
	sourceKey := throttleKey("setup-source", s.setupScope+":"+client.source())
	// Completed or expired setup stays authoritative even for a blocked
	// source.
	if err := s.Setup.assertOpen(required); err != nil {
		return nil, err
	}
	if blocked, err := s.tooManyAttempts(ctx, s.Now(), sourceKey); err != nil || blocked {
		if err != nil {
			return nil, err
		}
		return nil, busy(0)
	}
	if err := s.Setup.assertAuthorized(required, code); err != nil {
		if apperr.Is(err, "SETUP_AUTHORIZATION_REQUIRED") {
			if _, recordErr := s.DB.RecordLoginFailure(ctx, sourceKey, s.Now(), loginWindowMs, setupSourceMax, 0); recordErr != nil {
				return nil, recordErr
			}
		}
		return nil, err
	}
	if err := s.DB.ClearLoginThrottle(ctx, sourceKey); err != nil {
		return nil, err
	}
	username, validUsername := ValidUsername(username)
	if !validUsername || !ValidPassword(password) || textutil.Length(code) < SetupCodeMin || textutil.Length(code) > SetupCodeMax {
		return nil, apperr.New("INVALID_REQUEST", 400,
			"Username must be 3-32 letters, numbers, dots, underscores, or hyphens; password must be at least 15 characters.")
	}
	var passwordHash string
	err = s.Work.Do(HashToken("setup-source:"+client.source()), "initial-setup", func() error {
		var err error
		passwordHash, err = s.Hasher.Hash(password)
		return err
	})
	if errors.Is(err, ErrPasswordWorkBusy) {
		return nil, busy(0)
	}
	if err != nil {
		return nil, err
	}
	if required, err = s.SetupRequired(ctx); err != nil {
		return nil, err
	}
	if err := s.Setup.assertAuthorized(required, code); err != nil {
		if apperr.Is(err, "SETUP_AUTHORIZATION_REQUIRED") {
			return nil, errSetupComplete
		}
		return nil, err
	}
	user := db.SessionUser{ID: identity.NewUUID(), Username: username, Role: db.RoleAdmin}
	session, token := s.sessionInput(user.ID, client)
	err = s.DB.CreateFirstUser(ctx, db.UserRecord{User: db.User{ID: user.ID, Username: user.Username, Role: user.Role,
		CreatedAt: s.Now()}, PasswordHash: passwordHash},
		db.AuditEntry{UserID: user.ID, Action: "auth.setup.completed", TargetType: "user", TargetID: user.ID, IPAddress: client.IP},
		session)
	if errors.Is(err, db.ErrSetupComplete) {
		return nil, errSetupComplete
	}
	if err != nil {
		return nil, err
	}
	s.Setup.markCompleted()
	return &Principal{User: user, Token: token, TokenHash: session.TokenHash}, nil
}

// Authenticate verifies credentials against the current account state.
// Password work yields to other requests, so a reset or disabled account
// invalidates the credentials that were just checked.
func (s *Service) Authenticate(ctx context.Context, username, password string) (*db.SessionUser, error) {
	user, _, err := s.authenticate(ctx, username, password)
	return user, err
}

// authenticate also returns the password hash it verified, which a new
// session must still match.
func (s *Service) authenticate(ctx context.Context, username, password string) (*db.SessionUser, string, error) {
	record, err := s.DB.FindUserByUsername(ctx, username)
	if err != nil {
		return nil, "", err
	}
	if record == nil || record.Disabled {
		// Spend comparable time so unknown accounts are not distinguishable.
		_, err := s.Hasher.Hash(password)
		return nil, "", err
	}
	if !s.Hasher.Verify(password, record.PasswordHash) {
		return nil, "", nil
	}
	upgraded := ""
	if NeedsUpgrade(record.PasswordHash) {
		if upgraded, err = s.Hasher.Hash(password); err != nil {
			return nil, "", err
		}
	}
	current, err := s.DB.FindUserByID(ctx, record.ID)
	if err != nil || current == nil || current.Disabled || current.PasswordHash != record.PasswordHash {
		return nil, "", err
	}
	verified := record.PasswordHash
	if upgraded != "" {
		if err := s.DB.UpgradePasswordHashIfUnchanged(ctx, current.ID, record.PasswordHash, upgraded); err != nil {
			return nil, "", err
		}
		verified = upgraded
	}
	return &db.SessionUser{ID: current.ID, Username: current.Username, Role: current.Role}, verified, nil
}

var errInvalidCredentials = apperr.New("INVALID_CREDENTIALS", 401, "Invalid username or password")

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

// Login signs in and creates a session.
func (s *Service) Login(ctx context.Context, username, password string, client Client) (*Principal, error) {
	now := s.Now()
	requested := truncateRunes(strings.ToLower(strings.TrimSpace(username)), 32)
	sourceKey := throttleKey("login-source", client.source())
	keys := []string{sourceKey}
	accountSourceKey := ""
	if requested != "" {
		accountSourceKey = throttleKey("login-account-source", client.source()+"\x00"+requested)
		keys = append(keys, accountSourceKey)
	}
	if blocked, err := s.tooManyAttempts(ctx, now, keys...); err != nil || blocked {
		if err != nil {
			return nil, err
		}
		return nil, busy(0)
	}
	parsedUsername, validUsername := ValidUsername(username)
	required, err := s.SetupRequired(ctx)
	if err != nil {
		return nil, err
	}
	if !validUsername || !ValidPassword(password) || required {
		return nil, errInvalidCredentials
	}
	accountKey := throttleKey("login-account-failures", requested)
	cooldown, err := s.DB.GetLoginThrottle(ctx, accountKey, now, loginWindowMs)
	if err != nil {
		return nil, err
	}
	if cooldown.BlockedUntil > now {
		return nil, busy(cooldown.BlockedUntil - now)
	}
	var user *db.SessionUser
	var verifiedHash string
	err = s.Work.Do(passwordWorkSource(client), throttleKey("password-work-login-subject", strings.ToLower(parsedUsername)), func() error {
		var err error
		user, verifiedHash, err = s.authenticate(ctx, parsedUsername, password)
		return err
	})
	if errors.Is(err, ErrPasswordWorkBusy) {
		return nil, busy(0)
	}
	if err != nil {
		return nil, err
	}
	if user == nil {
		failedAt := s.Now()
		if _, err := s.DB.RecordLoginFailure(ctx, sourceKey, failedAt, loginWindowMs, loginSourceMax, 0); err != nil {
			return nil, err
		}
		if accountSourceKey != "" {
			if _, err := s.DB.RecordLoginFailure(ctx, accountSourceKey, failedAt, loginWindowMs, loginAccountSourceMax, 0); err != nil {
				return nil, err
			}
		}
		// A rotating source cannot avoid this account-wide slowdown. Keep the
		// cooldown short so an attacker cannot lock the owner out for long.
		// Rejected retries do not extend it, and a successful login clears it.
		current, err := s.DB.GetLoginThrottle(ctx, accountKey, failedAt, loginWindowMs)
		if err != nil {
			return nil, err
		}
		step := min(5, max(0, (current.Failures+1-loginAccountCooldownMin)/5))
		cooldownMs := min(int64(loginAccountCooldownMaxMs), int64(loginAccountCooldownBaseMs)<<step)
		if _, err := s.DB.RecordLoginFailure(ctx, accountKey, failedAt, loginWindowMs, loginAccountCooldownMin, cooldownMs); err != nil {
			return nil, err
		}
		if err := s.DB.WriteAudit(ctx, nil, db.AuditEntry{Action: "auth.login.failed", TargetType: "user",
			Details: map[string]string{"username": truncateRunes(parsedUsername, 32)}, IPAddress: client.IP}); err != nil {
			return nil, err
		}
		return nil, errInvalidCredentials
	}
	if accountSourceKey != "" {
		if err := s.DB.ClearLoginThrottle(ctx, accountSourceKey); err != nil {
			return nil, err
		}
	}
	if err := s.DB.ClearLoginThrottle(ctx, accountKey); err != nil {
		return nil, err
	}
	// A reset or disable after verification revokes sessions; the insert
	// checks the verified hash again so this login cannot outlive it. The
	// audit entry commits with the session, so a failed login leaves none.
	principal, err := s.createSession(ctx, *user, client, verifiedHash,
		&db.AuditEntry{UserID: user.ID, Action: "auth.login", TargetType: "session", IPAddress: client.IP})
	if errors.Is(err, db.ErrAccountChanged) {
		return nil, errInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	return principal, nil
}

// SessionToken extracts the session cookie. The last valid cookie wins.
func SessionToken(r *http.Request) string {
	value := ""
	for _, header := range r.Header.Values("Cookie") {
		for _, part := range strings.Split(header, ";") {
			part = strings.TrimSpace(part)
			separator := strings.Index(part, "=")
			if separator < 0 {
				continue
			}
			name, err := decodeURIComponent(part[:separator])
			if err != nil || name != SessionCookie {
				continue
			}
			if value, err = decodeURIComponent(part[separator+1:]); err != nil {
				value = ""
			}
		}
	}
	return value
}

func decodeURIComponent(value string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '%' {
			b.WriteByte(value[i])
			continue
		}
		if i+2 >= len(value) {
			return "", errors.New("malformed URI")
		}
		decoded, err := hex.DecodeString(value[i+1 : i+3])
		if err != nil {
			return "", errors.New("malformed URI")
		}
		b.WriteByte(decoded[0])
		i += 2
	}
	result := b.String()
	if !utf8Valid(result) {
		return "", errors.New("malformed URI")
	}
	return result, nil
}

// Session resolves a token to its current principal, or nil.
func (s *Service) Session(ctx context.Context, token string) (*Principal, error) {
	if token == "" {
		return nil, nil
	}
	tokenHash := HashToken(token)
	user, err := s.DB.FindSessionUser(ctx, tokenHash, s.Now())
	if err != nil || user == nil {
		return nil, err
	}
	return &Principal{User: *user, Token: token, TokenHash: tokenHash}, nil
}

// Recheck confirms the exact principal again after slow work and right
// before acting. Account state alone cannot detect a revoked session or a
// password reset.
func (s *Service) Recheck(ctx context.Context, principal *Principal) (*Principal, error) {
	current, err := s.Session(ctx, principal.Token)
	if err != nil {
		return nil, err
	}
	if current == nil || current.User.ID != principal.User.ID {
		return nil, errAuthRequired
	}
	return current, nil
}

// Logout ends the session and audits it; both commit or neither does.
func (s *Service) Logout(ctx context.Context, token string, client Client) error {
	principal, err := s.Session(ctx, token)
	if err != nil || principal == nil {
		return err
	}
	return s.DB.Logout(ctx, principal.TokenHash,
		db.AuditEntry{UserID: principal.User.ID, Action: "auth.logout", TargetType: "session", IPAddress: client.IP})
}

// ChangePassword replaces the signed-in account's password and returns the
// replacement session. Current-password guesses are throttled per source and
// per credential, so a stolen session cannot be brute forced into a takeover
// and one stolen session cannot lock every session out.
func (s *Service) ChangePassword(ctx context.Context, principal *Principal, currentPassword, newPassword string, client Client) (*Principal, error) {
	if length := textutil.Length(currentPassword); length < 1 || length > PasswordMax || !ValidPassword(newPassword) {
		return nil, apperr.Newf("INVALID_REQUEST", 400, "New password must be between %d and %d characters", PasswordMin, PasswordMax)
	}
	now := s.Now()
	sourceKey := throttleKey("password-change-source", client.source())
	credentialKey := throttleKey("password-change-credential", principal.TokenHash)
	if blocked, err := s.tooManyAttempts(ctx, now, sourceKey, credentialKey); err != nil || blocked {
		if err != nil {
			return nil, err
		}
		return nil, busy(0)
	}
	var record *db.UserRecord
	var verified bool
	var passwordHash string
	err := s.Work.Do(passwordWorkSource(client), throttleKey("password-work-change-subject", principal.TokenHash), func() error {
		var err error
		if record, err = s.DB.FindUserByID(ctx, principal.User.ID); err != nil {
			return err
		}
		verified = record != nil && s.Hasher.Verify(currentPassword, record.PasswordHash)
		if _, err := s.Recheck(ctx, principal); err != nil {
			return err
		}
		if verified {
			passwordHash, err = s.Hasher.Hash(newPassword)
		}
		return err
	})
	if errors.Is(err, ErrPasswordWorkBusy) {
		return nil, busy(0)
	}
	if err != nil {
		return nil, err
	}
	if !verified {
		failedAt := s.Now()
		if _, err := s.DB.RecordLoginFailure(ctx, sourceKey, failedAt, loginWindowMs, loginSourceMax, 0); err != nil {
			return nil, err
		}
		if _, err := s.DB.RecordLoginFailure(ctx, credentialKey, failedAt, loginWindowMs, loginAccountSourceMax, 0); err != nil {
			return nil, err
		}
		if err := s.DB.WriteAudit(ctx, nil, db.AuditEntry{UserID: principal.User.ID, Action: "auth.password.change-failed",
			TargetType: "user", TargetID: principal.User.ID, IPAddress: client.IP}); err != nil {
			return nil, err
		}
		return nil, apperr.New("INVALID_PASSWORD", 400, "Current password is incorrect")
	}
	current, err := s.Recheck(ctx, principal)
	if err != nil {
		return nil, err
	}
	session, token := s.sessionInput(principal.User.ID, client)
	err = s.DB.ChangePassword(ctx, principal.User.ID, principal.TokenHash, record.PasswordHash, passwordHash, credentialKey, session,
		db.AuditEntry{UserID: principal.User.ID, Action: "auth.password.changed", TargetType: "user", TargetID: principal.User.ID, IPAddress: client.IP})
	switch {
	case errors.Is(err, db.ErrSessionGone):
		return nil, errAuthRequired
	case errors.Is(err, db.ErrPasswordChanged):
		return nil, apperr.New("PASSWORD_CHANGED", 409, "Password changed; sign in again")
	case err != nil:
		return nil, err
	}
	return &Principal{User: current.User, Token: token, TokenHash: session.TokenHash}, nil
}

// Sessions lists the account's sessions.
func (s *Service) Sessions(ctx context.Context, principal *Principal) ([]db.Session, error) {
	return s.DB.ListUserSessions(ctx, principal.User.ID, principal.TokenHash)
}

// RevokeSession ends one of the account's sessions; the revocation and its
// audit entry commit together.
func (s *Service) RevokeSession(ctx context.Context, principal *Principal, sessionID string, client Client) error {
	removed, err := s.DB.DeleteUserSessionByID(ctx, principal.User.ID, sessionID, db.AuditEntry{UserID: principal.User.ID,
		Action: "auth.session.revoked", TargetType: "session", TargetID: sessionID, IPAddress: client.IP})
	if err != nil {
		return err
	}
	if !removed {
		return apperr.New("SESSION_NOT_FOUND", 404, "Session not found")
	}
	return nil
}

var (
	errInsufficient = apperr.New("FORBIDDEN", 403, "Insufficient permissions")
	errUserNotFound = apperr.New("USER_NOT_FOUND", 404, "User not found")
	errLastAdmin    = apperr.New("LAST_ADMIN", 409, "At least one active admin is required")
)

// RequireAdmin rejects a principal whose session role is not admin.
func RequireAdmin(principal *Principal) error {
	if principal == nil {
		return errAuthRequired
	}
	if principal.User.Role != db.RoleAdmin {
		return errInsufficient
	}
	return nil
}

func adminMutationError(err error) error {
	switch {
	case errors.Is(err, db.ErrSessionGone):
		return errAuthRequired
	case errors.Is(err, db.ErrNotAdmin):
		return apperr.New("FORBIDDEN", 403, "Administrator permission required")
	case errors.Is(err, db.ErrUserNotFound):
		return errUserNotFound
	case errors.Is(err, db.ErrLastAdmin):
		return errLastAdmin
	}
	return err
}

func (s *Service) adminHash(principal *Principal, client Client, password string) (string, error) {
	var hash string
	err := s.Work.Do(passwordWorkSource(client), throttleKey("password-work-admin-subject", principal.TokenHash), func() error {
		var err error
		hash, err = s.Hasher.Hash(password)
		return err
	})
	if errors.Is(err, ErrPasswordWorkBusy) {
		return "", busy(0)
	}
	return hash, err
}

// CreateUser adds an account.
func (s *Service) CreateUser(ctx context.Context, principal *Principal, username, password string, role db.Role, client Client) (*db.User, error) {
	if err := RequireAdmin(principal); err != nil {
		return nil, err
	}
	username, valid := ValidUsername(username)
	if !valid || !ValidPassword(password) || !db.ValidRole(string(role)) {
		return nil, apperr.New("INVALID_REQUEST", 400, "Invalid user details")
	}
	hash, err := s.adminHash(principal, client, password)
	if err != nil {
		return nil, err
	}
	id := identity.NewUUID()
	err = s.DB.AdminMutation(ctx, principal.User.ID, principal.TokenHash, s.Now(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO users
      (id, username, password_hash, role, disabled, created_at, updated_at) VALUES (?, ?, ?, ?, 0, ?, ?)`,
			id, username, hash, string(role), s.Now(), s.Now()); err != nil {
			return err
		}
		return s.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: principal.User.ID, Action: "user.created", TargetType: "user",
			TargetID: id, Details: map[string]string{"username": username, "role": string(role)}, IPAddress: client.IP})
	})
	if db.IsUniqueViolation(err) {
		return nil, apperr.New("USERNAME_TAKEN", 409, "Username is already in use")
	}
	if err != nil {
		return nil, adminMutationError(err)
	}
	record, err := s.DB.FindUserByID(ctx, id)
	if err != nil || record == nil {
		return nil, err
	}
	return &record.User, nil
}

// UpdateAccess changes an account's role and disabled flag.
func (s *Service) UpdateAccess(ctx context.Context, principal *Principal, id string, role db.Role, disabled bool, client Client) (*db.User, error) {
	if err := RequireAdmin(principal); err != nil {
		return nil, err
	}
	target, err := s.DB.FindUserByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, errUserNotFound
	}
	if !db.ValidRole(string(role)) {
		return nil, apperr.New("INVALID_REQUEST", 400, "Invalid access settings")
	}
	err = s.DB.AdminMutation(ctx, principal.User.ID, principal.TokenHash, s.Now(), func(tx *sql.Tx) error {
		last, err := db.RemovesLastAdmin(ctx, tx, id, role, disabled)
		if err != nil {
			return err
		}
		if last {
			return db.ErrLastAdmin
		}
		if _, err := tx.ExecContext(ctx, "UPDATE users SET role = ?, disabled = ?, updated_at = ? WHERE id = ?",
			string(role), boolInt(disabled), s.Now(), id); err != nil {
			return err
		}
		if disabled {
			if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", id); err != nil {
				return err
			}
		}
		return s.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: principal.User.ID, Action: "user.access.updated", TargetType: "user",
			TargetID: id, Details: map[string]any{"role": string(role), "disabled": disabled}, IPAddress: client.IP})
	})
	if err != nil {
		return nil, adminMutationError(err)
	}
	record, err := s.DB.FindUserByID(ctx, id)
	if err != nil || record == nil {
		return nil, err
	}
	return &record.User, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// DeleteUser removes an account.
func (s *Service) DeleteUser(ctx context.Context, principal *Principal, id string, client Client) error {
	if err := RequireAdmin(principal); err != nil {
		return err
	}
	target, err := s.DB.FindUserByID(ctx, id)
	if err != nil {
		return err
	}
	if target == nil {
		return errUserNotFound
	}
	if target.ID == principal.User.ID {
		return apperr.New("SELF_DELETE", 409, "You cannot delete your own account")
	}
	err = s.DB.AdminMutation(ctx, principal.User.ID, principal.TokenHash, s.Now(), func(tx *sql.Tx) error {
		last, err := db.RemovesLastAdmin(ctx, tx, id, db.RoleViewer, true)
		if err != nil {
			return err
		}
		if last {
			return db.ErrLastAdmin
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM users WHERE id = ?", id); err != nil {
			return err
		}
		return s.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: principal.User.ID, Action: "user.deleted", TargetType: "user",
			TargetID: id, Details: map[string]string{"username": target.Username, "role": string(target.Role)}, IPAddress: client.IP})
	})
	return adminMutationError(err)
}

// ResetPassword sets another account's password and signs it out.
func (s *Service) ResetPassword(ctx context.Context, principal *Principal, id, password string, client Client) error {
	if err := RequireAdmin(principal); err != nil {
		return err
	}
	target, err := s.DB.FindUserByID(ctx, id)
	if err != nil {
		return err
	}
	if target == nil {
		return errUserNotFound
	}
	if !ValidPassword(password) {
		return apperr.Newf("INVALID_REQUEST", 400, "Password must be at least %d characters", PasswordMin)
	}
	hash, err := s.adminHash(principal, client, password)
	if err != nil {
		return err
	}
	err = s.DB.AdminMutation(ctx, principal.User.ID, principal.TokenHash, s.Now(), func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?", hash, s.Now(), id)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return db.ErrUserNotFound
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", id); err != nil {
			return err
		}
		return s.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: principal.User.ID, Action: "user.password.reset", TargetType: "user",
			TargetID: id, IPAddress: client.IP})
	})
	return adminMutationError(err)
}

// RecoverPassword resets an account from the console (`ludock recover`).
func RecoverPassword(ctx context.Context, d *db.DB, username, password string) (*db.UserRecord, error) {
	user, err := d.FindUserByUsername(ctx, username)
	if err != nil || user == nil {
		return user, err
	}
	hash, err := Argon2{}.Hash(password)
	if err != nil {
		return nil, err
	}
	// The reset and its audit entry commit together, so a reported failure
	// leaves the old password and sessions in place.
	if err := d.UpdateUserPassword(ctx, user.ID, hash, db.AuditEntry{Action: "user.password.recovered", TargetType: "user",
		TargetID: user.ID, Details: map[string]string{"username": user.Username}}); err != nil {
		return nil, err
	}
	return user, nil
}

func utf8Valid(value string) bool { return utf8.ValidString(value) }
