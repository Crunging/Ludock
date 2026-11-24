package db

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
)

const sealPrefix = "aes-256-gcm:"

func (d *DB) sealAEAD(purpose string) (cipher.AEAD, error) {
	mac := hmac.New(sha256.New, d.key)
	mac.Write([]byte("ludock:seal:" + purpose + ":v1"))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts a stored credential with a key derived from the identity key,
// so a database-only disclosure doesn't reveal it. binding, such as the
// owning account's ID, must match when it is opened.
func (d *DB) Seal(purpose, binding string, plaintext []byte) (string, error) {
	aead, err := d.sealAEAD(purpose)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, plaintext, []byte(binding))
	return sealPrefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

var errUnseal = errors.New("sealed value is invalid")

func (d *DB) Unseal(purpose, binding, sealed string) ([]byte, error) {
	encoded, ok := strings.CutPrefix(sealed, sealPrefix)
	if !ok {
		return nil, errUnseal
	}
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errUnseal
	}
	aead, err := d.sealAEAD(purpose)
	if err != nil {
		return nil, err
	}
	if len(raw) < aead.NonceSize() {
		return nil, errUnseal
	}
	plaintext, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(binding))
	if err != nil {
		return nil, errUnseal
	}
	return plaintext, nil
}

// RecoveryCodeHash keys a normalized recovery code to its account.
func (d *DB) RecoveryCodeHash(userID, code string) string {
	return keyedHex(d.key, "ludock:recovery-code:v1:"+userID+":"+code)
}

var (
	// ErrTwoFactorChanged means two-factor sign-in was turned on, off, or set
	// up again since the caller read it.
	ErrTwoFactorChanged = errors.New("two-factor sign-in changed")
	// ErrChallengeGone means the sign-in challenge expired or was used.
	ErrChallengeGone = errors.New("sign-in challenge gone")
	// ErrCodeUsed means the code's time step or recovery code was already used.
	ErrCodeUsed = errors.New("code already used")
)

// TwoFactor is an account's stored two-factor state; secrets stay sealed.
type TwoFactor struct {
	Secret        string
	Pending       string
	Step          int64
	RecoveryCodes int
}

func (d *DB) TwoFactor(ctx context.Context, userID string) (TwoFactor, error) {
	var state TwoFactor
	var secret, pending sql.NullString
	err := d.QueryRowContext(ctx, `SELECT totp_secret, totp_pending, totp_step,
    (SELECT COUNT(*) FROM recovery_codes WHERE user_id = users.id) FROM users WHERE id = ?`, userID).
		Scan(&secret, &pending, &state.Step, &state.RecoveryCodes)
	if errors.Is(err, sql.ErrNoRows) {
		return state, ErrUserNotFound
	}
	state.Secret, state.Pending = secret.String, pending.String
	return state, err
}

// SetPendingTwoFactor stores a secret the account is setting up while the
// requesting session tokenHash is still valid. It has no effect on sign-in
// until EnableTwoFactor confirms it.
func (d *DB) SetPendingTwoFactor(ctx context.Context, userID, tokenHash, sealed string, now int64) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		if err := AssertSession(ctx, tx, userID, tokenHash, now); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, "UPDATE users SET totp_pending = ? WHERE id = ? AND totp_secret IS NULL", sealed, userID)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return ErrTwoFactorChanged
		}
		return nil
	})
}

// ClearPendingTwoFactor drops a secret the account was setting up, while the
// requesting session tokenHash is still valid.
func (d *DB) ClearPendingTwoFactor(ctx context.Context, userID, tokenHash string, now int64) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		if err := AssertSession(ctx, tx, userID, tokenHash, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE users SET totp_pending = NULL WHERE id = ? AND totp_secret IS NULL", userID)
		return err
	})
}

// TwoFactorChange turns two-factor sign-in on or off for the signed-in
// account whose password was just verified.
type TwoFactorChange struct {
	UserID, TokenHash, PasswordHash string
	// Pending is the stored pending secret being confirmed, and Secret the
	// same secret sealed for sign-in; both empty turn it off.
	Pending, Secret string
	// Step is the time step of the confirming code, which can't sign in again.
	Step           int64
	RecoveryHashes []string
	ThrottleKey    string
	Session        SessionInput
	Audit          AuditEntry
}

// ChangeTwoFactor applies c while the requesting session is valid and the
// password is the one verified. Like a password change, it signs out every
// session and creates the replacement in the same transaction.
func (d *DB) ChangeTwoFactor(ctx context.Context, c TwoFactorChange) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		valid, err := sessionValid(ctx, tx, c.UserID, c.TokenHash, c.Session.CreatedAt)
		if err != nil {
			return err
		}
		if !valid {
			return ErrSessionGone
		}
		var result sql.Result
		if c.Pending != "" {
			result, err = tx.ExecContext(ctx, `UPDATE users SET totp_secret = ?, totp_pending = NULL, totp_step = ?,
        updated_at = ? WHERE id = ? AND password_hash = ? AND totp_pending = ? AND totp_secret IS NULL`,
				c.Secret, c.Step, Now(), c.UserID, c.PasswordHash, c.Pending)
		} else {
			result, err = tx.ExecContext(ctx, `UPDATE users SET totp_secret = NULL, totp_pending = NULL, totp_step = 0,
        updated_at = ? WHERE id = ? AND password_hash = ? AND totp_secret IS NOT NULL`, Now(), c.UserID, c.PasswordHash)
		}
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return ErrTwoFactorChanged
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM recovery_codes WHERE user_id = ?", c.UserID); err != nil {
			return err
		}
		for _, hash := range c.RecoveryHashes {
			if _, err := tx.ExecContext(ctx, "INSERT INTO recovery_codes (user_id, code_hash) VALUES (?, ?)", c.UserID, hash); err != nil {
				return err
			}
		}
		for _, statement := range []string{"DELETE FROM sessions WHERE user_id = ?", "DELETE FROM sign_in_challenges WHERE user_id = ?"} {
			if _, err := tx.ExecContext(ctx, statement, c.UserID); err != nil {
				return err
			}
		}
		if err := d.insertSession(ctx, tx, c.Session); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM login_attempts WHERE attempt_key = ?", c.ThrottleKey); err != nil {
			return err
		}
		return d.WriteAudit(ctx, tx, c.Audit)
	})
}

// ClearTwoFactor turns off an account's two-factor sign-in inside tx and
// reports whether it was on. Callers sign the account out.
func ClearTwoFactor(ctx context.Context, tx *sql.Tx, userID string) (bool, error) {
	var on bool
	err := tx.QueryRowContext(ctx, "SELECT totp_secret IS NOT NULL FROM users WHERE id = ?", userID).Scan(&on)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrUserNotFound
	}
	if err != nil {
		return false, err
	}
	for _, statement := range []string{
		"UPDATE users SET totp_secret = NULL, totp_pending = NULL, totp_step = 0 WHERE id = ?",
		"DELETE FROM recovery_codes WHERE user_id = ?",
		"DELETE FROM sign_in_challenges WHERE user_id = ?",
	} {
		if _, err := tx.ExecContext(ctx, statement, userID); err != nil {
			return false, err
		}
	}
	return on, nil
}

// SignInChallenge is a pending two-factor sign-in with its account's
// current state.
type SignInChallenge struct {
	UserID, Username string
	Role             Role
	// PasswordHash is the hash the password step verified; the account must
	// still have it.
	PasswordHash, CurrentHash string
	Disabled                  bool
	Secret                    string
	Step                      int64
}

// CreateSignInChallenge stores a challenge only while the account is
// enabled, has two-factor sign-in, and still has the verified password.
func (d *DB) CreateSignInChallenge(ctx context.Context, tokenHash, userID, passwordHash string, now, expiresAt int64) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM sign_in_challenges WHERE expires_at <= ?", now); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO sign_in_challenges (token_hash, user_id, password_hash, expires_at)
      SELECT ?, id, password_hash, ? FROM users
      WHERE id = ? AND password_hash = ? AND disabled = 0 AND totp_secret IS NOT NULL`, tokenHash, expiresAt, userID, passwordHash)
		if err != nil {
			return err
		}
		if created, _ := result.RowsAffected(); created != 1 {
			return ErrAccountChanged
		}
		return nil
	})
}

// FindSignInChallenge returns an unexpired challenge with attempts left, or
// nil.
func (d *DB) FindSignInChallenge(ctx context.Context, tokenHash string, maxAttempts int, now int64) (*SignInChallenge, error) {
	var c SignInChallenge
	var role string
	var disabled int
	var secret sql.NullString
	err := d.QueryRowContext(ctx, `SELECT users.id, users.username, users.role, sign_in_challenges.password_hash,
    users.password_hash, users.disabled, users.totp_secret, users.totp_step
    FROM sign_in_challenges JOIN users ON users.id = sign_in_challenges.user_id
    WHERE sign_in_challenges.token_hash = ? AND sign_in_challenges.attempts < ? AND sign_in_challenges.expires_at > ?`,
		tokenHash, maxAttempts, now).
		Scan(&c.UserID, &c.Username, &role, &c.PasswordHash, &c.CurrentHash, &disabled, &secret, &c.Step)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Role, c.Disabled, c.Secret = Role(role), disabled == 1, secret.String
	return &c, nil
}

// ReserveSignInAttempt counts a code attempt before it is checked, so
// concurrent attempts can't exceed maxAttempts. It reports false when the
// challenge is used up, expired, or gone.
func (d *DB) ReserveSignInAttempt(ctx context.Context, tokenHash string, maxAttempts int, now int64) (bool, error) {
	result, err := d.ExecContext(ctx, `UPDATE sign_in_challenges SET attempts = attempts + 1
    WHERE token_hash = ? AND attempts < ? AND expires_at > ?`, tokenHash, maxAttempts, now)
	if err != nil {
		return false, err
	}
	reserved, err := result.RowsAffected()
	return reserved == 1, err
}

func (d *DB) DeleteSignInChallenge(ctx context.Context, tokenHash string) error {
	_, err := d.ExecContext(ctx, "DELETE FROM sign_in_challenges WHERE token_hash = ?", tokenHash)
	return err
}

// CompleteSignInChallenge spends the challenge and a code, then creates the
// session, all in one transaction. A step above zero records an
// authenticator code's time step, which must be later than any used before;
// otherwise recoveryHash names the recovery code to spend. The session must
// carry the challenge's verified password hash.
func (d *DB) CompleteSignInChallenge(ctx context.Context, tokenHash string, step int64, recoveryHash string, session SessionInput) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM sign_in_challenges
      WHERE token_hash = ? AND user_id = ? AND password_hash = ? AND expires_at > ?`,
			tokenHash, session.UserID, session.PasswordHash, session.CreatedAt)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return ErrChallengeGone
		}
		if step > 0 {
			result, err = tx.ExecContext(ctx, `UPDATE users SET totp_step = ?
        WHERE id = ? AND totp_secret IS NOT NULL AND totp_step < ?`, step, session.UserID, step)
		} else {
			result, err = tx.ExecContext(ctx, "DELETE FROM recovery_codes WHERE user_id = ? AND code_hash = ?", session.UserID, recoveryHash)
		}
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return ErrCodeUsed
		}
		return d.insertSession(ctx, tx, session)
	})
}
