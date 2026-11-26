package auth

import (
	"errors"
	"sync"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
)

const linkPassword = "chosen by link holder"

func linkService(t *testing.T) (*Service, *clock, *Principal) {
	t.Helper()
	s, c, _ := newService(t, nil)
	addUser(t, s, adminID, "admin", db.RoleAdmin, original)
	principal, err := s.Login(ctx, "admin", original, Client{IP: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	return s, c, principal
}

func wantLinkInvalid(t *testing.T, err error) {
	t.Helper()
	if !apperr.Is(err, "LINK_INVALID") {
		t.Fatalf("err = %v, want LINK_INVALID", err)
	}
}

// An invited account can't sign in until its link is used, and only one of
// several concurrent uses of the link sets the password. Uses that overlap
// in password work are turned away as busy.
func TestInviteLinkWorksOnceUnderConcurrentUse(t *testing.T) {
	s, _, admin := linkService(t)
	user, link, err := s.InviteUser(ctx, admin, "invitee", db.RoleMember, Client{}, nil)
	if err != nil || link.Purpose != db.LinkInvite {
		t.Fatal(link, err)
	}
	if _, err := s.Login(ctx, "invitee", linkPassword, Client{IP: "192.0.2.1"}); !apperr.Is(err, "INVALID_CREDENTIALS") {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]error, 6)
	for index := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[index] = s.UseLink(ctx, link.Token, linkPassword+string(rune('a'+index)), Client{IP: "192.0.2." + itoa(10+index)})
		}()
	}
	wg.Wait()
	used := -1
	for index, err := range results {
		if err == nil {
			if used >= 0 {
				t.Fatal("link used twice")
			}
			used = index
		} else if !apperr.Is(err, "TOO_MANY_ATTEMPTS") {
			wantLinkInvalid(t, err)
		}
	}
	if used < 0 {
		t.Fatal("no use succeeded")
	}
	principal, err := s.Login(ctx, "invitee", linkPassword+string(rune('a'+used)), Client{IP: "192.0.2.3"})
	if err != nil || principal.User.ID != user.ID {
		t.Fatal(principal, err)
	}
	_, err = s.UseLink(ctx, link.Token, linkPassword, Client{IP: "192.0.2.2"})
	wantLinkInvalid(t, err)
}

// A link stops working when it expires, when a newer one replaces it, and
// when the password changes some other way.
func TestResetLinkEndsWithExpiryOrChange(t *testing.T) {
	s, c, admin := linkService(t)
	addUser(t, s, targetID, "target", db.RoleMember, original)
	expired, err := s.CreateLink(ctx, admin, targetID, Client{})
	if err != nil || expired.Purpose != db.LinkReset {
		t.Fatal(expired, err)
	}
	c.now += LinkTTLMs
	_, err = s.UseLink(ctx, expired.Token, linkPassword, Client{})
	wantLinkInvalid(t, err)

	replaced, _ := s.CreateLink(ctx, admin, targetID, Client{})
	newer, _ := s.CreateLink(ctx, admin, targetID, Client{})
	_, err = s.UseLink(ctx, replaced.Token, linkPassword, Client{})
	wantLinkInvalid(t, err)

	hash, _ := Argon2{}.Hash("changed by recovery")
	if err := s.DB.UpdateUserPassword(ctx, targetID, hash, resetAudit); err != nil {
		t.Fatal(err)
	}
	_, err = s.UseLink(ctx, newer.Token, linkPassword, Client{})
	wantLinkInvalid(t, err)
	if _, err := s.Login(ctx, "target", "changed by recovery", Client{}); err != nil {
		t.Fatal(err)
	}
}

// Disabling an account ends its link, even if the account is enabled again
// or is disabled while the new password is being hashed.
func TestLinkEndsWhenAccountIsDisabled(t *testing.T) {
	s, _, admin := linkService(t)
	addUser(t, s, targetID, "target", db.RoleMember, original)
	link, _ := s.CreateLink(ctx, admin, targetID, Client{})
	for _, disabled := range []bool{true, false} {
		if _, err := s.ChangeAccess(ctx, admin, targetID, db.RoleMember, new(disabled), Client{}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.UseLink(ctx, link.Token, linkPassword, Client{})
	wantLinkInvalid(t, err)

	link, _ = s.CreateLink(ctx, admin, targetID, Client{})
	s.Hasher.(*fastHasher).beforeHash = func() {
		s.Hasher.(*fastHasher).beforeHash = nil
		if _, err := s.ChangeAccess(ctx, admin, targetID, db.RoleMember, new(true), Client{}); err != nil {
			t.Error(err)
		}
	}
	_, err = s.UseLink(ctx, link.Token, linkPassword, Client{})
	wantLinkInvalid(t, err)
	if _, err := s.CreateLink(ctx, admin, targetID, Client{}); !apperr.Is(err, "USER_DISABLED") {
		t.Fatal(err)
	}
}

// A reset link sets the password of an account with two-factor sign-in but
// signs it in only after a code.
func TestResetLinkStillAsksForTheCode(t *testing.T) {
	s, c, _ := newService(t, nil)
	c.now = 1_700_000_000_000
	addUser(t, s, adminID, "admin", db.RoleAdmin, "correct horse battery staple")
	addUser(t, s, targetID, "boss", db.RoleAdmin, original)
	secret, _ := enrolled(t, s, c)
	boss, err := s.Login(ctx, "boss", original, Client{})
	if err != nil {
		t.Fatal(err)
	}
	link, err := s.CreateLink(ctx, boss, adminID, Client{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.UseLink(ctx, link.Token, linkPassword, Client{})
	var required *CodeRequired
	if !errors.As(err, &required) {
		t.Fatal(err)
	}
	if sessions, _ := s.DB.ListUserSessions(ctx, adminID, ""); len(sessions) != 0 {
		t.Fatal("signed in without a code", sessions)
	}
	if _, err := s.Login(ctx, "admin", linkPassword, Client{}); !errors.As(err, &required) {
		t.Fatal(err)
	}
	c.now += 30_000
	principal, err := s.CompleteLogin(ctx, required.Token, hotp(secret, totpStep(c.now)), Client{})
	if err != nil || principal.User.ID != adminID {
		t.Fatal(principal, err)
	}
}

// Unknown tokens count against the source like failed sign-ins, and a
// blocked source can't use even a working link.
func TestLinkAttemptsAreThrottled(t *testing.T) {
	s, _, admin := linkService(t)
	addUser(t, s, targetID, "target", db.RoleMember, original)
	link, _ := s.CreateLink(ctx, admin, targetID, Client{})
	attacker := Client{IP: "203.0.113.9"}
	for range loginSourceMax {
		_, err := s.OpenLink(ctx, "guessed-token", attacker)
		wantLinkInvalid(t, err)
	}
	_, err := s.UseLink(ctx, link.Token, linkPassword, attacker)
	wantStatus(t, err, 429)
	if _, err := s.UseLink(ctx, link.Token, linkPassword, Client{IP: "198.51.100.1"}); err != nil {
		t.Fatal(err)
	}
}

// Making or revoking a link needs the administrator's session to still be
// valid, and an administrator can't make one for their own account.
func TestLinkChangesRecheckTheAdmin(t *testing.T) {
	s, _, admin := linkService(t)
	addUser(t, s, targetID, "target", db.RoleMember, original)
	if _, err := s.CreateLink(ctx, admin, adminID, Client{}); !apperr.Is(err, "SELF_LINK") {
		t.Fatal(err)
	}
	link, _ := s.CreateLink(ctx, admin, targetID, Client{})
	if err := dbtest.DeleteUserSessions(ctx, s.DB, adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLink(ctx, admin, targetID, Client{}); !apperr.Is(err, "AUTHENTICATION_REQUIRED") {
		t.Fatal(err)
	}
	if err := s.RevokeLink(ctx, admin, targetID, Client{}); !apperr.Is(err, "AUTHENTICATION_REQUIRED") {
		t.Fatal(err)
	}
	if _, err := s.OpenLink(ctx, link.Token, Client{}); err != nil {
		t.Fatal("stale revoke removed the link", err)
	}
}

// Concurrent transactions spending the same link: exactly one commits.
func TestUseAccountLinkCommitsOnce(t *testing.T) {
	s, _, admin := linkService(t)
	addUser(t, s, targetID, "target", db.RoleMember, original)
	link, _ := s.CreateLink(ctx, admin, targetID, Client{})
	hash, _ := Argon2{}.Hash(linkPassword)
	var wg sync.WaitGroup
	results := make([]error, 8)
	for index := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session, _ := s.sessionInput(targetID, Client{})
			_, results[index] = s.DB.UseAccountLink(ctx, db.LinkUse{TokenHash: HashToken(link.Token), UserID: targetID,
				PasswordHash: hash, Session: session, Audit: db.AuditEntry{Action: "auth.link.used"}})
		}()
	}
	wg.Wait()
	committed := 0
	for _, err := range results {
		if err == nil {
			committed++
		} else if !errors.Is(err, db.ErrLinkGone) {
			t.Fatal(err)
		}
	}
	if committed != 1 {
		t.Fatal("commits:", committed)
	}
}

// An invited administrator who hasn't chosen a password doesn't count as
// another administrator, so the only one who can sign in can't step down.
func TestInvitedAdminIsNotAnotherAdmin(t *testing.T) {
	s, _, admin := linkService(t)
	if _, _, err := s.InviteUser(ctx, admin, "next-admin", db.RoleAdmin, Client{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangeAccess(ctx, admin, adminID, db.RoleMember, new(false), Client{}); !apperr.Is(err, "LAST_ADMIN") {
		t.Fatal(err)
	}
}
