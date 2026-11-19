package db

import (
	"context"
	"database/sql"
	"errors"
)

// LinkPurpose says what a one-time account link is for.
type LinkPurpose string

const (
	// LinkInvite sets the first password of an account that has none.
	LinkInvite LinkPurpose = "invite"
	// LinkReset replaces an account's password.
	LinkReset LinkPurpose = "reset"
)

// AccountLink is an account's outstanding link, without its token.
type AccountLink struct {
	Purpose   LinkPurpose
	ExpiresAt int64
}

// LinkAccount is the account an unexpired, unused link names.
type LinkAccount struct {
	UserID, Username string
	Role             Role
	Purpose          LinkPurpose
	ExpiresAt        int64
	TwoFactor        bool
}

var (
	// ErrLinkGone means the link was used, revoked, replaced, or expired, or
	// its account changed.
	ErrLinkGone = errors.New("account link gone")
	// ErrUserDisabled means the account is disabled.
	ErrUserDisabled = errors.New("account disabled")
)

// liveLink selects links that still work: unexpired, for an enabled
// account whose password hasn't changed since the link was made.
const liveLink = `account_links JOIN users ON users.id = account_links.user_id
    WHERE account_links.expires_at > ? AND users.disabled = 0 AND users.password_hash = account_links.password_hash`

// PutAccountLink replaces the account's link inside tx and reports its
// purpose: an invite when the account has no password, a reset otherwise.
func PutAccountLink(ctx context.Context, tx *sql.Tx, userID, tokenHash string, now, expiresAt int64) (LinkPurpose, error) {
	var disabled int
	var hasPassword bool
	err := tx.QueryRowContext(ctx, "SELECT disabled, password_hash != '' FROM users WHERE id = ?", userID).Scan(&disabled, &hasPassword)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUserNotFound
	}
	if err != nil {
		return "", err
	}
	if disabled == 1 {
		return "", ErrUserDisabled
	}
	purpose := LinkInvite
	if hasPassword {
		purpose = LinkReset
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM account_links WHERE user_id = ? OR expires_at <= ?", userID, now); err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO account_links (token_hash, user_id, purpose, password_hash, created_at, expires_at)
    SELECT ?, id, ?, password_hash, ?, ? FROM users WHERE id = ?`, tokenHash, string(purpose), now, expiresAt, userID)
	return purpose, err
}

// DeleteAccountLink removes the account's link inside tx and reports whether
// a working one was removed.
func DeleteAccountLink(ctx context.Context, tx *sql.Tx, userID string, now int64) (bool, error) {
	var live int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+liveLink+" AND account_links.user_id = ?", now, userID).Scan(&live); err != nil {
		return false, err
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM account_links WHERE user_id = ?", userID)
	return live == 1, err
}

// FindAccountLink returns the account a working link names, or nil.
func (d *DB) FindAccountLink(ctx context.Context, tokenHash string, now int64) (*LinkAccount, error) {
	var link LinkAccount
	var role, purpose string
	err := d.QueryRowContext(ctx, `SELECT users.id, users.username, users.role, account_links.purpose, account_links.expires_at,
    users.totp_secret IS NOT NULL FROM `+liveLink+` AND account_links.token_hash = ?`, now, tokenHash).
		Scan(&link.UserID, &link.Username, &role, &purpose, &link.ExpiresAt, &link.TwoFactor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	link.Role, link.Purpose = Role(role), LinkPurpose(purpose)
	return &link, nil
}

// LinkUse spends a link to set an account's password.
type LinkUse struct {
	TokenHash string
	UserID    string
	// PasswordHash is the new password's hash.
	PasswordHash string
	// Session signs in an account without two-factor sign-in; Challenge, the
	// token hash of a sign-in challenge expiring at ChallengeExpiresAt, asks
	// one with it for a code instead.
	Session            SessionInput
	Challenge          string
	ChallengeExpiresAt int64
	Audit              AuditEntry
}

// UseAccountLink spends the link, sets the password, signs the account out
// everywhere, and starts its new sign-in, all in one transaction. It reports
// whether the account has two-factor sign-in, in which case the challenge
// was created instead of the session.
func (d *DB) UseAccountLink(ctx context.Context, use LinkUse) (bool, error) {
	var twoFactor bool
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		var userID, bound string
		err := tx.QueryRowContext(ctx, `SELECT users.id, account_links.password_hash, users.totp_secret IS NOT NULL
      FROM `+liveLink+` AND account_links.token_hash = ?`, use.Session.CreatedAt, use.TokenHash).Scan(&userID, &bound, &twoFactor)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && userID != use.UserID) {
			return ErrLinkGone
		}
		if err != nil {
			return err
		}
		for _, statement := range []string{"DELETE FROM account_links WHERE user_id = ?", "DELETE FROM sessions WHERE user_id = ?",
			"DELETE FROM sign_in_challenges WHERE user_id = ?"} {
			if _, err := tx.ExecContext(ctx, statement, userID); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, "UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ? AND password_hash = ?",
			use.PasswordHash, use.Session.CreatedAt, userID, bound)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return ErrLinkGone
		}
		if twoFactor {
			_, err = tx.ExecContext(ctx, `INSERT INTO sign_in_challenges (token_hash, user_id, password_hash, expires_at)
        VALUES (?, ?, ?, ?)`, use.Challenge, userID, use.PasswordHash, use.ChallengeExpiresAt)
		} else {
			session := use.Session
			session.PasswordHash, session.PasswordOnly = use.PasswordHash, true
			err = d.insertSession(ctx, tx, session)
		}
		if err != nil {
			return err
		}
		return d.WriteAudit(ctx, tx, use.Audit)
	})
	return twoFactor, err
}
