package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/textutil"
)

const (
	// ChallengeCookie holds a sign-in that passed its password step.
	ChallengeCookie = "ludock_sign_in"
	ChallengeTTLMs  = 5 * 60 * 1000
	// challengeAttempts is how many codes one challenge accepts.
	challengeAttempts = 5
	// loginCodeAccountMax limits wrong codes per account across sources.
	// Reaching the second step needs the password, so this can block for the
	// whole window without letting strangers lock the owner out.
	loginCodeAccountMax = 10

	totpPurpose = "totp-secret"
)

// CodeRequired is Login's result for an account with two-factor sign-in:
// the password was right and Token names the challenge for CompleteLogin.
type CodeRequired struct{ Token string }

func (*CodeRequired) Error() string { return "authentication code required" }

var (
	errInvalidCode   = apperr.New("INVALID_CODE", 401, "That code didn't work. Check your authenticator app and try again.")
	errSignInExpired = apperr.New("SIGN_IN_EXPIRED", 401, "Your sign-in expired. Sign in again.")
	errTwoFactorOn   = apperr.New("TWO_FACTOR_ON", 409, "Two-factor sign-in is already on.")
	errTwoFactorOff  = apperr.New("TWO_FACTOR_OFF", 409, "Two-factor sign-in is off.")
	errSetupChanged  = apperr.New("TWO_FACTOR_CHANGED", 409, "Two-factor setup changed in another window. Start again.")
)

// ChallengeToken extracts the pending sign-in cookie.
func ChallengeToken(r *http.Request) string { return cookieValue(r, ChallengeCookie) }

func (s *Service) challenge(ctx context.Context, user *verifiedUser) error {
	token := s.newToken()
	now := s.Now()
	err := s.DB.CreateSignInChallenge(ctx, HashToken(token), user.ID, user.PasswordHash, now, now+ChallengeTTLMs)
	if errors.Is(err, db.ErrAccountChanged) {
		return errInvalidCredentials
	}
	if err != nil {
		return err
	}
	return &CodeRequired{Token: token}
}

func codeKey(username string) string {
	return throttleKey("login-code-account", strings.ToLower(username))
}

// pendingBinding ties a secret being set up to the session that started it,
// so the account's other sessions can't read or confirm it.
func pendingBinding(principal *Principal) string {
	return principal.User.ID + "\x00" + principal.TokenHash
}

// CompleteLogin finishes a two-factor sign-in with an authenticator code or
// a recovery code. Wrong codes count against the same limits as wrong
// passwords, and the session is created only once a code is accepted.
func (s *Service) CompleteLogin(ctx context.Context, token, code string, client Client) (*Principal, error) {
	now := s.Now()
	if token == "" {
		return nil, errSignInExpired
	}
	tokenHash := HashToken(token)
	challenge, err := s.DB.FindSignInChallenge(ctx, tokenHash, now)
	if err != nil {
		return nil, err
	}
	if challenge == nil {
		return nil, errSignInExpired
	}
	keys := newLoginKeys(client, truncateRunes(strings.ToLower(challenge.Username), 32))
	accountCodes := codeKey(challenge.Username)
	if blocked, err := s.tooManyAttempts(ctx, now, append(keys.blocking(), keys.account, accountCodes)...); err != nil || blocked {
		if err != nil {
			return nil, err
		}
		return nil, busy(0)
	}
	if challenge.Disabled || challenge.Secret == "" || challenge.CurrentHash != challenge.PasswordHash {
		if err := s.DB.DeleteSignInChallenge(ctx, tokenHash); err != nil {
			return nil, err
		}
		return nil, errSignInExpired
	}
	// Attempts are counted before the code is checked, in transactions that
	// serialize, so concurrent guesses can't exceed either limit.
	if reserved, err := s.DB.ReserveSignInAttempt(ctx, tokenHash, challengeAttempts, now); err != nil || !reserved {
		if err != nil {
			return nil, err
		}
		return nil, errSignInExpired
	}
	charged, err := s.DB.RecordLoginFailure(ctx, accountCodes, now, loginWindowMs, loginCodeAccountMax, 0)
	if err != nil {
		return nil, err
	}
	if charged.Failures > loginCodeAccountMax {
		return nil, busy(0)
	}
	var step int64
	recoveryHash := ""
	if code = normalizeCode(code); isTOTPCode(code) {
		secret, err := s.DB.Unseal(totpPurpose, challenge.UserID, challenge.Secret)
		if err != nil {
			return nil, err
		}
		step = matchTOTP(secret, code, now, challenge.Step)
		clear(secret)
	} else if isRecoveryCode(code) {
		recoveryHash = s.DB.RecoveryCodeHash(challenge.UserID, code)
	}
	method := "authenticator"
	if recoveryHash != "" {
		method = "recovery-code"
	}
	if step > 0 || recoveryHash != "" {
		user := db.SessionUser{ID: challenge.UserID, Username: challenge.Username, Role: challenge.Role}
		session, sessionToken := s.sessionInput(user.ID, client)
		session.PasswordHash = challenge.PasswordHash
		session.Audit = &db.AuditEntry{UserID: user.ID, Action: "auth.login", TargetType: "session",
			Details: map[string]string{"secondFactor": method}, IPAddress: client.IP}
		err = s.DB.CompleteSignInChallenge(ctx, tokenHash, step, recoveryHash, session)
		switch {
		case err == nil:
			if err := s.clearLoginKeys(ctx, keys.accountSource, keys.account, accountCodes); err != nil {
				return nil, err
			}
			return &Principal{User: user, Token: sessionToken, TokenHash: session.TokenHash}, nil
		case errors.Is(err, db.ErrChallengeGone), errors.Is(err, db.ErrAccountChanged):
			return nil, errSignInExpired
		case !errors.Is(err, db.ErrCodeUsed):
			return nil, err
		}
	}
	if err := s.recordFailure(ctx, keys, s.Now()); err != nil {
		return nil, err
	}
	if err := s.DB.WriteAudit(ctx, nil, db.AuditEntry{UserID: challenge.UserID, Action: "auth.login.code-failed", TargetType: "user",
		TargetID: challenge.UserID, IPAddress: client.IP}); err != nil {
		return nil, err
	}
	return nil, errInvalidCode
}

// TwoFactorStatus reports whether the account uses two-factor sign-in and
// how many recovery codes it has left.
func (s *Service) TwoFactorStatus(ctx context.Context, principal *Principal) (bool, int, error) {
	state, err := s.DB.TwoFactor(ctx, principal.User.ID)
	if errors.Is(err, db.ErrUserNotFound) {
		return false, 0, errAuthRequired
	}
	return state.Secret != "", state.RecoveryCodes, err
}

// BeginTwoFactor creates a new secret for the account to confirm with
// EnableTwoFactor. It replaces any earlier unconfirmed one.
func (s *Service) BeginTwoFactor(ctx context.Context, principal *Principal) (*Enrollment, error) {
	secret := make([]byte, totpSecretSize)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	defer clear(secret)
	sealed, err := s.DB.Seal(totpPurpose, pendingBinding(principal), secret)
	if err != nil {
		return nil, err
	}
	if err := s.DB.SetPendingTwoFactor(ctx, principal.User.ID, sealed); errors.Is(err, db.ErrTwoFactorChanged) {
		return nil, errTwoFactorOn
	} else if err != nil {
		return nil, err
	}
	enrollment := newEnrollment(secret, principal.User.Username)
	return &enrollment, nil
}

// PendingTwoFactor returns the unconfirmed secret again to the session that
// started setting it up, or nil.
func (s *Service) PendingTwoFactor(ctx context.Context, principal *Principal) (*Enrollment, error) {
	state, err := s.DB.TwoFactor(ctx, principal.User.ID)
	if err != nil || state.Pending == "" || state.Secret != "" {
		return nil, err
	}
	secret, err := s.DB.Unseal(totpPurpose, pendingBinding(principal), state.Pending)
	if err != nil {
		return nil, nil
	}
	defer clear(secret)
	enrollment := newEnrollment(secret, principal.User.Username)
	return &enrollment, nil
}

func validConfirmation(password string) error {
	if length := textutil.Length(password); length < 1 || length > PasswordMax {
		return apperr.New("INVALID_PASSWORD", 400, "Current password is incorrect")
	}
	return nil
}

func twoFactorChangeError(err error) error {
	switch {
	case errors.Is(err, db.ErrSessionGone):
		return errAuthRequired
	case errors.Is(err, db.ErrTwoFactorChanged):
		return errSetupChanged
	}
	return err
}

// EnableTwoFactor turns on two-factor sign-in once code confirms the
// pending secret. It signs out the account's other sessions and returns the
// replacement session and the recovery codes, which are never shown again.
func (s *Service) EnableTwoFactor(ctx context.Context, principal *Principal, password, code string, client Client) (*Principal, []string, error) {
	if err := validConfirmation(password); err != nil {
		return nil, nil, err
	}
	state, err := s.DB.TwoFactor(ctx, principal.User.ID)
	if err != nil {
		return nil, nil, twoFactorChangeError(err)
	}
	if state.Secret != "" {
		return nil, nil, errTwoFactorOn
	}
	if state.Pending == "" {
		return nil, nil, errSetupChanged
	}
	secret, err := s.DB.Unseal(totpPurpose, pendingBinding(principal), state.Pending)
	if err != nil {
		return nil, nil, errSetupChanged
	}
	defer clear(secret)
	record, credentialKey, err := s.confirmPassword(ctx, principal, password, client, nil)
	if err != nil {
		return nil, nil, err
	}
	sealed, err := s.DB.Seal(totpPurpose, principal.User.ID, secret)
	if err != nil {
		return nil, nil, err
	}
	step := matchTOTP(secret, normalizeCode(code), s.Now(), 0)
	if step == 0 {
		return nil, nil, apperr.New("INVALID_CODE", 400, "That code didn't work. Check the time on your phone and enter the newest code.")
	}
	codes := newRecoveryCodes()
	hashes := make([]string, len(codes))
	for index, recovery := range codes {
		hashes[index] = s.DB.RecoveryCodeHash(principal.User.ID, normalizeCode(recovery))
	}
	session, token := s.sessionInput(principal.User.ID, client)
	err = s.DB.ChangeTwoFactor(ctx, db.TwoFactorChange{UserID: principal.User.ID, TokenHash: principal.TokenHash,
		PasswordHash: record.PasswordHash, Pending: state.Pending, Secret: sealed, Step: step, RecoveryHashes: hashes, ThrottleKey: credentialKey,
		Session: session, Audit: db.AuditEntry{UserID: principal.User.ID, Action: "auth.two-factor.enabled", TargetType: "user",
			TargetID: principal.User.ID, IPAddress: client.IP}})
	if err != nil {
		return nil, nil, twoFactorChangeError(err)
	}
	return &Principal{User: principal.User, Token: token, TokenHash: session.TokenHash}, codes, nil
}

// DisableTwoFactor turns off two-factor sign-in and, like a password
// change, signs out the account's other sessions.
func (s *Service) DisableTwoFactor(ctx context.Context, principal *Principal, password string, client Client) (*Principal, error) {
	if err := validConfirmation(password); err != nil {
		return nil, err
	}
	record, credentialKey, err := s.confirmPassword(ctx, principal, password, client, nil)
	if err != nil {
		return nil, err
	}
	if !record.TwoFactor {
		return nil, errTwoFactorOff
	}
	session, token := s.sessionInput(principal.User.ID, client)
	err = s.DB.ChangeTwoFactor(ctx, db.TwoFactorChange{UserID: principal.User.ID, TokenHash: principal.TokenHash,
		PasswordHash: record.PasswordHash, ThrottleKey: credentialKey, Session: session,
		Audit: db.AuditEntry{UserID: principal.User.ID, Action: "auth.two-factor.disabled", TargetType: "user",
			TargetID: principal.User.ID, IPAddress: client.IP}})
	if err != nil {
		return nil, twoFactorChangeError(err)
	}
	return &Principal{User: principal.User, Token: token, TokenHash: session.TokenHash}, nil
}

// ResetTwoFactor turns off another account's two-factor sign-in for a lost
// phone and signs that account out everywhere.
func (s *Service) ResetTwoFactor(ctx context.Context, principal *Principal, id string, client Client) error {
	if err := RequireAdmin(principal); err != nil {
		return err
	}
	if id == principal.User.ID {
		return apperr.New("SELF_TWO_FACTOR_RESET", 409, "Turn off your own two-factor sign-in in Account")
	}
	err := s.DB.AdminMutation(ctx, principal.User.ID, principal.TokenHash, s.Now(), func(tx *sql.Tx) error {
		on, err := db.ClearTwoFactor(ctx, tx, id)
		if err != nil {
			return err
		}
		if !on {
			return errTwoFactorOff
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", id); err != nil {
			return err
		}
		return s.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: principal.User.ID, Action: "user.two-factor.reset", TargetType: "user",
			TargetID: id, IPAddress: client.IP})
	})
	return adminMutationError(err)
}
