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

type Principal struct {
	User      db.SessionUser
	Token     string
	TokenHash string
}

type Service struct {
	DB     *db.DB
	Authz  *authz.Service
	Setup  *Setup
	Work   *PasswordWork
	Hasher Hasher
	// Now returns Unix milliseconds; tests replace it.
	Now func() int64

	setupScope string
}

func NewService(d *db.DB, authorization *authz.Service, setup *Setup) *Service {
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

func (s *Service) SetupRequired(ctx context.Context) (bool, error) {
	count, err := s.DB.CountUsers(ctx)
	return count == 0, err
}

func (s *Service) SetupState(ctx context.Context) (SetupState, error) {
	required, err := s.SetupRequired(ctx)
	if err != nil {
		return SetupState{}, err
	}
	return SetupState{Required: required}, nil
}

// LogSetupInstructions prints a generated setup code to the console once.
func (s *Service) LogSetupInstructions(ctx context.Context, console func(string)) {
	if required, err := s.SetupRequired(ctx); err != nil || !required {
		return
	}
	if code := s.Setup.TakeGeneratedCode(); code != "" {
		console("Ludock initial setup code: " + code + "\nEnter this one-time code on the setup page to create the first administrator.\n")
	}
	log.Info("Initial administrator setup is available")
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

// createSession signs user in with a password alone. verifiedHash is the
// password hash the caller verified; the session is refused if the account
// changed since or now has two-factor sign-in.
func (s *Service) createSession(ctx context.Context, user db.SessionUser, client Client, verifiedHash string, audit *db.AuditEntry) (*Principal, error) {
	input, token := s.sessionInput(user.ID, client)
	input.PasswordHash, input.PasswordOnly, input.Audit = verifiedHash, true, audit
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
	// Completed setup stays authoritative even for a blocked source.
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
	user, err := s.authenticate(ctx, username, password)
	if user == nil {
		return nil, err
	}
	return &user.SessionUser, err
}

// verifiedUser is an account whose password was just verified.
type verifiedUser struct {
	db.SessionUser
	// PasswordHash is the hash verified, which a new session must still match.
	PasswordHash string
	TwoFactor    bool
}

func (s *Service) authenticate(ctx context.Context, username, password string) (*verifiedUser, error) {
	record, err := s.DB.FindUserByUsername(ctx, username)
	if err != nil {
		return nil, err
	}
	if record == nil || record.Disabled || record.PasswordHash == "" {
		// Spend comparable time so unknown accounts, and invited ones that
		// have no password yet, are not distinguishable.
		_, err := s.Hasher.Hash(password)
		return nil, err
	}
	if !s.Hasher.Verify(password, record.PasswordHash) {
		return nil, nil
	}
	upgraded := ""
	if NeedsUpgrade(record.PasswordHash) {
		if upgraded, err = s.Hasher.Hash(password); err != nil {
			return nil, err
		}
	}
	current, err := s.DB.FindUserByID(ctx, record.ID)
	if err != nil || current == nil || current.Disabled || current.PasswordHash != record.PasswordHash {
		return nil, err
	}
	verified := record.PasswordHash
	if upgraded != "" {
		if err := s.DB.UpgradePasswordHashIfUnchanged(ctx, current.ID, record.PasswordHash, upgraded); err != nil {
			return nil, err
		}
		verified = upgraded
	}
	return &verifiedUser{SessionUser: db.SessionUser{ID: current.ID, Username: current.Username, Role: current.Role},
		PasswordHash: verified, TwoFactor: current.TwoFactor}, nil
}

var errInvalidCredentials = apperr.New("INVALID_CREDENTIALS", 401, "Invalid username or password")

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

// loginKeys are the throttle keys a sign-in attempt counts against.
type loginKeys struct {
	source, accountSource, account string
}

func newLoginKeys(client Client, requested string) loginKeys {
	keys := loginKeys{source: throttleKey("login-source", client.source())}
	if requested != "" {
		keys.accountSource = throttleKey("login-account-source", client.source()+"\x00"+requested)
	}
	keys.account = throttleKey("login-account-failures", requested)
	return keys
}

func (k loginKeys) blocking() []string {
	if k.accountSource == "" {
		return []string{k.source}
	}
	return []string{k.source, k.accountSource}
}

// recordFailure counts a failed password or code against the source, the
// source and account, and the account.
func (s *Service) recordFailure(ctx context.Context, keys loginKeys, failedAt int64) error {
	if _, err := s.DB.RecordLoginFailure(ctx, keys.source, failedAt, loginWindowMs, loginSourceMax, 0); err != nil {
		return err
	}
	if keys.accountSource != "" {
		if _, err := s.DB.RecordLoginFailure(ctx, keys.accountSource, failedAt, loginWindowMs, loginAccountSourceMax, 0); err != nil {
			return err
		}
	}
	// Keep the account-wide cooldown short so an attacker cannot lock the
	// owner out for long. Rejected retries do not extend it.
	current, err := s.DB.GetLoginThrottle(ctx, keys.account, failedAt, loginWindowMs)
	if err != nil {
		return err
	}
	step := min(5, max(0, (current.Failures+1-loginAccountCooldownMin)/5))
	cooldownMs := min(int64(loginAccountCooldownMaxMs), int64(loginAccountCooldownBaseMs)<<step)
	_, err = s.DB.RecordLoginFailure(ctx, keys.account, failedAt, loginWindowMs, loginAccountCooldownMin, cooldownMs)
	return err
}

func (s *Service) clearLoginKeys(ctx context.Context, keys ...string) error {
	for _, key := range keys {
		if key == "" {
			continue
		}
		if err := s.DB.ClearLoginThrottle(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

// Login checks a password. An account with two-factor sign-in gets a
// *CodeRequired error carrying the challenge for CompleteLogin instead of a
// session.
func (s *Service) Login(ctx context.Context, username, password string, client Client) (*Principal, error) {
	now := s.Now()
	requested := truncateRunes(strings.ToLower(strings.TrimSpace(username)), 32)
	keys := newLoginKeys(client, requested)
	if blocked, err := s.tooManyAttempts(ctx, now, keys.blocking()...); err != nil || blocked {
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
	cooldown, err := s.DB.GetLoginThrottle(ctx, keys.account, now, loginWindowMs)
	if err != nil {
		return nil, err
	}
	if cooldown.BlockedUntil > now {
		return nil, busy(cooldown.BlockedUntil - now)
	}
	var user *verifiedUser
	err = s.Work.Do(passwordWorkSource(client), throttleKey("password-work-login-subject", strings.ToLower(parsedUsername)), func() error {
		var err error
		user, err = s.authenticate(ctx, parsedUsername, password)
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
		if err := s.recordFailure(ctx, keys, failedAt); err != nil {
			return nil, err
		}
		if err := s.DB.WriteAudit(ctx, nil, db.AuditEntry{Action: "auth.login.failed", TargetType: "user",
			Details: map[string]string{"username": truncateRunes(parsedUsername, 32)}, IPAddress: client.IP}); err != nil {
			return nil, err
		}
		return nil, errInvalidCredentials
	}
	if user.TwoFactor {
		// Failure counts stay until the code is accepted, so a known password
		// doesn't reset the limit on guessing codes.
		return nil, s.challenge(ctx, user)
	}
	if err := s.clearLoginKeys(ctx, keys.accountSource, keys.account); err != nil {
		return nil, err
	}
	// The insert checks the verified hash again, so a reset or disable after
	// verification wins. The audit entry commits with the session.
	principal, err := s.createSession(ctx, user.SessionUser, client, user.PasswordHash,
		&db.AuditEntry{UserID: user.ID, Action: "auth.login", TargetType: "session", IPAddress: client.IP})
	if errors.Is(err, db.ErrAccountChanged) {
		return nil, errInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	return principal, nil
}

// SessionToken extracts the session cookie.
func SessionToken(r *http.Request) string { return cookieValue(r, SessionCookie) }

// cookieValue reads a cookie by name. The last valid cookie wins.
func cookieValue(r *http.Request, cookie string) string {
	value := ""
	for _, header := range r.Header.Values("Cookie") {
		for _, part := range strings.Split(header, ";") {
			part = strings.TrimSpace(part)
			separator := strings.Index(part, "=")
			if separator < 0 {
				continue
			}
			name, err := decodeURIComponent(part[:separator])
			if err != nil || name != cookie {
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

// confirmPassword checks the signed-in account's current password and
// returns the verified record and the credential's throttle key. Guesses are
// throttled per source and per credential. then runs in the same password
// work slot once the password is verified.
func (s *Service) confirmPassword(ctx context.Context, principal *Principal, password string, client Client, then func() error) (*db.UserRecord, string, error) {
	now := s.Now()
	sourceKey := throttleKey("password-change-source", client.source())
	credentialKey := throttleKey("password-change-credential", principal.TokenHash)
	if blocked, err := s.tooManyAttempts(ctx, now, sourceKey, credentialKey); err != nil || blocked {
		if err != nil {
			return nil, "", err
		}
		return nil, "", busy(0)
	}
	var record *db.UserRecord
	var verified bool
	err := s.Work.Do(passwordWorkSource(client), throttleKey("password-work-change-subject", principal.TokenHash), func() error {
		var err error
		if record, err = s.DB.FindUserByID(ctx, principal.User.ID); err != nil {
			return err
		}
		verified = record != nil && s.Hasher.Verify(password, record.PasswordHash)
		if _, err := s.Recheck(ctx, principal); err != nil {
			return err
		}
		if verified && then != nil {
			err = then()
		}
		return err
	})
	if errors.Is(err, ErrPasswordWorkBusy) {
		return nil, "", busy(0)
	}
	if err != nil {
		return nil, "", err
	}
	if !verified {
		failedAt := s.Now()
		if _, err := s.DB.RecordLoginFailure(ctx, sourceKey, failedAt, loginWindowMs, loginSourceMax, 0); err != nil {
			return nil, "", err
		}
		if _, err := s.DB.RecordLoginFailure(ctx, credentialKey, failedAt, loginWindowMs, loginAccountSourceMax, 0); err != nil {
			return nil, "", err
		}
		if err := s.DB.WriteAudit(ctx, nil, db.AuditEntry{UserID: principal.User.ID, Action: "auth.password.change-failed",
			TargetType: "user", TargetID: principal.User.ID, IPAddress: client.IP}); err != nil {
			return nil, "", err
		}
		return nil, "", apperr.New("INVALID_PASSWORD", 400, "Current password is incorrect")
	}
	return record, credentialKey, nil
}

// ChangePassword replaces the signed-in account's password and returns the
// replacement session.
func (s *Service) ChangePassword(ctx context.Context, principal *Principal, currentPassword, newPassword string, client Client) (*Principal, error) {
	if length := textutil.Length(currentPassword); length < 1 || length > PasswordMax || !ValidPassword(newPassword) {
		return nil, apperr.Newf("INVALID_REQUEST", 400, "New password must be between %d and %d characters", PasswordMin, PasswordMax)
	}
	var passwordHash string
	record, credentialKey, err := s.confirmPassword(ctx, principal, currentPassword, client, func() error {
		var err error
		passwordHash, err = s.Hasher.Hash(newPassword)
		return err
	})
	if err != nil {
		return nil, err
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
	errInsufficient = apperr.New("FORBIDDEN", 403, "You can’t do that")
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
		return apperr.New("FORBIDDEN", 403, "Only an administrator can do that")
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

// CreateUser creates an account with a password the administrator chose.
func (s *Service) CreateUser(ctx context.Context, principal *Principal, username, password string, role db.Role, client Client) (*db.User, error) {
	if !ValidPassword(password) {
		return nil, apperr.Newf("INVALID_REQUEST", 400, "Password must be between %d and %d characters", PasswordMin, PasswordMax)
	}
	return s.createUser(ctx, principal, username, password, role, client, nil)
}

// createUser creates an account; an empty password leaves it without one.
// then runs in the creating transaction.
func (s *Service) createUser(ctx context.Context, principal *Principal, username, password string, role db.Role, client Client,
	then func(tx *sql.Tx, id string) error) (*db.User, error) {
	if err := RequireAdmin(principal); err != nil {
		return nil, err
	}
	username, valid := ValidUsername(username)
	if !valid || !db.ValidRole(string(role)) {
		return nil, apperr.New("INVALID_REQUEST", 400, "Invalid user details")
	}
	hash := ""
	if password != "" {
		var err error
		if hash, err = s.adminHash(principal, client, password); err != nil {
			return nil, err
		}
	}
	id := identity.NewUUID()
	err := s.DB.AdminMutation(ctx, principal.User.ID, principal.TokenHash, s.Now(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO users
      (id, username, password_hash, role, disabled, created_at, updated_at) VALUES (?, ?, ?, ?, 0, ?, ?)`,
			id, username, hash, string(role), s.Now(), s.Now()); err != nil {
			return err
		}
		if err := s.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: principal.User.ID, Action: "user.created", TargetType: "user",
			TargetID: id, Details: map[string]string{"username": username, "role": string(role)}, IPAddress: client.IP}); err != nil {
			return err
		}
		if then != nil {
			return then(tx, id)
		}
		return nil
	})
	if db.IsUniqueViolation(err) {
		return nil, apperr.New("USERNAME_TAKEN", 409, "Username is already in use")
	}
	if err != nil {
		return nil, adminMutationError(err)
	}
	// Another administrator may have deleted the account since it committed.
	record, err := s.DB.FindUserByID(ctx, id)
	if err == nil && record == nil {
		err = errUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &record.User, nil
}

// ChangeAccess changes an account's role, its disabled flag, or both; an
// empty role or nil disabled keeps the current value, read in the same
// transaction, so a stale page changes only what it names.
func (s *Service) ChangeAccess(ctx context.Context, principal *Principal, id string, role db.Role, change *bool, client Client) (*db.User, error) {
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
	if (role != "" && !db.ValidRole(string(role))) || (role == "" && change == nil) {
		return nil, apperr.New("INVALID_REQUEST", 400, "Invalid access settings")
	}
	var disabled bool
	err = s.DB.AdminMutation(ctx, principal.User.ID, principal.TokenHash, s.Now(), func(tx *sql.Tx) error {
		var current string
		if err := tx.QueryRowContext(ctx, "SELECT role, disabled FROM users WHERE id = ?", id).Scan(&current, &disabled); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return db.ErrUserNotFound
			}
			return err
		}
		if role == "" {
			role = db.Role(current)
		}
		if change != nil {
			disabled = *change
		}
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
		// Administrators use every server, so their shares are dropped: one
		// made a member again starts with nothing shared.
		if role == db.RoleAdmin {
			if _, err := tx.ExecContext(ctx, "DELETE FROM server_grants WHERE user_id = ?", id); err != nil {
				return err
			}
		}
		if disabled {
			for _, statement := range []string{"DELETE FROM sessions WHERE user_id = ?", "DELETE FROM sign_in_challenges WHERE user_id = ?",
				"DELETE FROM account_links WHERE user_id = ?"} {
				if _, err := tx.ExecContext(ctx, statement, id); err != nil {
					return err
				}
			}
		}
		return s.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: principal.User.ID, Action: "user.access.updated", TargetType: "user",
			TargetID: id, Details: map[string]any{"role": string(role), "disabled": disabled}, IPAddress: client.IP})
	})
	if err != nil {
		return nil, adminMutationError(err)
	}
	// Another administrator may have deleted the account since it committed.
	record, err := s.DB.FindUserByID(ctx, id)
	if err == nil && record == nil {
		err = errUserNotFound
	}
	if err != nil {
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
		last, err := db.RemovesLastAdmin(ctx, tx, id, db.RoleMember, true)
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
		for _, statement := range []string{"DELETE FROM sessions WHERE user_id = ?", "DELETE FROM account_links WHERE user_id = ?"} {
			if _, err := tx.ExecContext(ctx, statement, id); err != nil {
				return err
			}
		}
		return s.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: principal.User.ID, Action: "user.password.reset", TargetType: "user",
			TargetID: id, IPAddress: client.IP})
	})
	return adminMutationError(err)
}

// RecoverPassword resets an account from the console (`ludock recover`) and
// turns off its two-factor sign-in.
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
