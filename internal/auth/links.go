package auth

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
)

const (
	// LinkCookie carries an opened invite or reset link to its form, so the
	// token leaves the address bar after the first request.
	LinkCookie = "ludock_link"
	LinkTTLMs  = 24 * 60 * 60 * 1000
)

// Link is a one-time invite or reset link. Token is shown once and stored
// only as its hash.
type Link struct {
	Token     string
	Purpose   db.LinkPurpose
	ExpiresAt int64
}

var (
	// errLinkInvalid is the one answer for unknown, used, expired, revoked,
	// and replaced links, and for links whose account changed.
	errLinkInvalid  = apperr.New("LINK_INVALID", 404, "Invite and reset links work once and expire after 24 hours. Ask the person who runs this Ludock panel for a new one.")
	errLinkDisabled = apperr.New("USER_DISABLED", 409, "Enable the account before making a link for it")
	errLinkSelf     = apperr.New("SELF_LINK", 409, "Change your own password in Account")
)

// LinkToken extracts an opened link's cookie.
func LinkToken(r *http.Request) string { return cookieValue(r, LinkCookie) }

func linkSourceKey(client Client) string { return throttleKey("link-source", client.source()) }

// InviteUser creates an account without a password and returns the invite
// link that sets it. The account can't sign in until the link is used.
func (s *Service) InviteUser(ctx context.Context, principal *Principal, username string, role db.Role, client Client) (*db.User, *Link, error) {
	var link *Link
	user, err := s.createUser(ctx, principal, username, "", role, client, func(tx *sql.Tx, id string) error {
		var err error
		link, err = s.putLink(ctx, tx, principal, id, client)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return user, link, nil
}

// CreateLink replaces another account's link with a new one: a reset link,
// or an invite for an account that has no password yet.
func (s *Service) CreateLink(ctx context.Context, principal *Principal, id string, client Client) (*Link, error) {
	if err := RequireAdmin(principal); err != nil {
		return nil, err
	}
	if id == principal.User.ID {
		return nil, errLinkSelf
	}
	var link *Link
	err := s.DB.AdminMutation(ctx, principal.User.ID, principal.TokenHash, s.Now(), func(tx *sql.Tx) error {
		var err error
		link, err = s.putLink(ctx, tx, principal, id, client)
		return err
	})
	if errors.Is(err, db.ErrUserDisabled) {
		return nil, errLinkDisabled
	}
	if err != nil {
		return nil, adminMutationError(err)
	}
	return link, nil
}

func (s *Service) putLink(ctx context.Context, tx *sql.Tx, principal *Principal, id string, client Client) (*Link, error) {
	token, now := s.newToken(), s.Now()
	link := &Link{Token: token, ExpiresAt: now + LinkTTLMs}
	purpose, err := db.PutAccountLink(ctx, tx, id, HashToken(token), now, link.ExpiresAt)
	if err != nil {
		return nil, err
	}
	link.Purpose = purpose
	return link, s.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: principal.User.ID, Action: "user.link.created", TargetType: "user",
		TargetID: id, Details: map[string]any{"purpose": string(purpose), "expiresAt": link.ExpiresAt}, IPAddress: client.IP})
}

// RevokeLink removes another account's outstanding link.
func (s *Service) RevokeLink(ctx context.Context, principal *Principal, id string, client Client) error {
	if err := RequireAdmin(principal); err != nil {
		return err
	}
	err := s.DB.AdminMutation(ctx, principal.User.ID, principal.TokenHash, s.Now(), func(tx *sql.Tx) error {
		removed, err := db.DeleteAccountLink(ctx, tx, id, s.Now())
		if err != nil {
			return err
		}
		if !removed {
			return apperr.New("NO_LINK", 409, "That link was already used, revoked, or expired")
		}
		return s.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: principal.User.ID, Action: "user.link.revoked", TargetType: "user",
			TargetID: id, IPAddress: client.IP})
	})
	return adminMutationError(err)
}

// OpenLink returns the account a working link names. Unknown and dead links
// count against the source like failed sign-ins.
func (s *Service) OpenLink(ctx context.Context, token string, client Client) (*db.LinkAccount, error) {
	now := s.Now()
	sourceKey := linkSourceKey(client)
	if blocked, err := s.tooManyAttempts(ctx, now, sourceKey); err != nil || blocked {
		if err != nil {
			return nil, err
		}
		return nil, busy(0)
	}
	var link *db.LinkAccount
	var err error
	if token != "" {
		if link, err = s.DB.FindAccountLink(ctx, HashToken(token), now); err != nil {
			return nil, err
		}
	}
	if link == nil {
		if _, err := s.DB.RecordLoginFailure(ctx, sourceKey, now, loginWindowMs, loginSourceMax, 0); err != nil {
			return nil, err
		}
		if err := s.DB.WriteAudit(ctx, nil, db.AuditEntry{Action: "auth.link.failed", IPAddress: client.IP}); err != nil {
			return nil, err
		}
		return nil, errLinkInvalid
	}
	return link, nil
}

// UseLink spends a link to set the account's password and signs it out
// everywhere else. An account with two-factor sign-in gets a *CodeRequired
// error instead of a session, so the link never replaces the second factor.
func (s *Service) UseLink(ctx context.Context, token, password string, client Client) (*Principal, error) {
	link, err := s.OpenLink(ctx, token, client)
	if err != nil {
		return nil, err
	}
	if !ValidPassword(password) {
		return nil, apperr.Newf("INVALID_REQUEST", 400, "Use between %d and %d characters", PasswordMin, PasswordMax)
	}
	var hash string
	err = s.Work.Do(passwordWorkSource(client), throttleKey("password-work-link-subject", link.UserID), func() error {
		var err error
		hash, err = s.Hasher.Hash(password)
		return err
	})
	if errors.Is(err, ErrPasswordWorkBusy) {
		return nil, busy(0)
	}
	if err != nil {
		return nil, err
	}
	session, sessionToken := s.sessionInput(link.UserID, client)
	challenge := s.newToken()
	twoFactor, err := s.DB.UseAccountLink(ctx, db.LinkUse{TokenHash: HashToken(token), UserID: link.UserID, PasswordHash: hash,
		Session: session, Challenge: HashToken(challenge), ChallengeExpiresAt: session.CreatedAt + ChallengeTTLMs,
		Audit: db.AuditEntry{UserID: link.UserID, Action: "auth.link.used", TargetType: "user", TargetID: link.UserID,
			Details: map[string]string{"purpose": string(link.Purpose)}, IPAddress: client.IP}})
	if errors.Is(err, db.ErrLinkGone) || errors.Is(err, db.ErrAccountChanged) {
		return nil, errLinkInvalid
	}
	if err != nil {
		return nil, err
	}
	if twoFactor {
		return nil, &CodeRequired{Token: challenge}
	}
	return &Principal{User: db.SessionUser{ID: link.UserID, Username: link.Username, Role: link.Role}, Token: sessionToken,
		TokenHash: session.TokenHash}, nil
}
