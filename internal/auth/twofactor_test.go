package auth

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
)

// The RFC 6238 SHA-1 test vectors, truncated to the 6 digits apps show.
func TestTOTPMatchesRFC6238(t *testing.T) {
	secret := []byte("12345678901234567890")
	for seconds, want := range map[int64]string{
		59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037",
		20000000000: "353130",
	} {
		if got := hotp(secret, totpStep(seconds*1000)); got != want {
			t.Errorf("at %d: %s, want %s", seconds, got, want)
		}
	}
}

// A code is accepted from one step before or after now, at the step's exact
// edges, and never from a step at or before the last one used.
func TestTOTPWindowAndReplay(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := int64(1_700_000_010_000)
	step := totpStep(now)
	for offset, accepted := range map[int64]bool{-2: false, -1: true, 0: true, 1: true, 2: false} {
		if got := matchTOTP(secret, hotp(secret, step+offset), now, 0); (got == step+offset) != accepted {
			t.Errorf("offset %d: step %d", offset, got)
		}
	}
	edge := (step + 1) * totpPeriodMs
	if matchTOTP(secret, hotp(secret, step-1), edge-1, 0) != step-1 || matchTOTP(secret, hotp(secret, step-1), edge, 0) != 0 {
		t.Error("window edge")
	}
	if matchTOTP(secret, hotp(secret, step), now, step) != 0 || matchTOTP(secret, hotp(secret, step-1), now, step) != 0 {
		t.Error("replayed a used step")
	}
	if matchTOTP(secret, hotp(secret, step+1), now, step) != step+1 {
		t.Error("rejected a later step")
	}
	for _, code := range []string{"", "12345", "1234567", "12345a", "١٢٣٤٥٦"} {
		if matchTOTP(secret, code, now, 0) != 0 {
			t.Errorf("accepted %q", code)
		}
	}
}

func TestRecoveryCodesNormalize(t *testing.T) {
	codes := newRecoveryCodes()
	seen := map[string]bool{}
	for _, code := range codes {
		normalized := normalizeCode(" " + strings.ToUpper(code) + " ")
		if !isRecoveryCode(normalized) || seen[normalized] {
			t.Fatalf("%q -> %q", code, normalized)
		}
		seen[normalized] = true
	}
	if len(codes) != recoveryCodeCount || isRecoveryCode("123456") {
		t.Fatal(codes)
	}
}

// enrolled signs admin in and turns on two-factor sign-in, returning the
// secret and recovery codes.
func enrolled(t *testing.T, s *Service, c *clock) ([]byte, []string) {
	t.Helper()
	principal, err := s.Login(ctx, "admin", "correct horse battery staple", Client{IP: "192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := s.BeginTwoFactor(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := secretEncoding.DecodeString(enrollment.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnableTwoFactor(ctx, principal, "correct horse battery staple", hotp(secret, totpStep(c.now)+5), Client{}); !apperr.Is(err, "INVALID_CODE") {
		t.Fatal(err)
	}
	_, codes, err := s.EnableTwoFactor(ctx, principal, "correct horse battery staple", hotp(secret, totpStep(c.now)), Client{})
	if err != nil {
		t.Fatal(err)
	}
	return secret, codes
}

func challengeFor(t *testing.T, s *Service, ip string) string {
	t.Helper()
	_, err := s.Login(ctx, "admin", "correct horse battery staple", Client{IP: ip})
	var required *CodeRequired
	if !errors.As(err, &required) {
		t.Fatalf("login: %v", err)
	}
	return required.Token
}

// Each authenticator code signs in once: the code that confirmed setup
// can't sign in, a code works in a later step, and reusing it fails. Each
// recovery code works once too.
func TestCompleteLoginSpendsCodesOnce(t *testing.T) {
	s, c, _ := newService(t, nil)
	c.now = 1_700_000_000_000
	addUser(t, s, adminID, "admin", db.RoleAdmin, "correct horse battery staple")
	secret, codes := enrolled(t, s, c)

	if _, err := s.CompleteLogin(ctx, challengeFor(t, s, "192.0.2.2"), hotp(secret, totpStep(c.now)), Client{}); !apperr.Is(err, "INVALID_CODE") {
		t.Fatalf("enrollment code reused: %v", err)
	}
	c.now += totpPeriodMs
	code := hotp(secret, totpStep(c.now))
	principal, err := s.CompleteLogin(ctx, challengeFor(t, s, "192.0.2.2"), code, Client{})
	if err != nil || principal.User.ID != adminID {
		t.Fatal(principal, err)
	}
	if _, err := s.CompleteLogin(ctx, challengeFor(t, s, "192.0.2.3"), code, Client{}); !apperr.Is(err, "INVALID_CODE") {
		t.Fatalf("code reused: %v", err)
	}
	if _, err := s.CompleteLogin(ctx, challengeFor(t, s, "192.0.2.3"), strings.ToUpper(codes[0]), Client{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteLogin(ctx, challengeFor(t, s, "192.0.2.4"), codes[0], Client{}); !apperr.Is(err, "INVALID_CODE") {
		t.Fatalf("recovery code reused: %v", err)
	}
	if on, left, err := s.TwoFactorStatus(ctx, principal); !on || left != recoveryCodeCount-1 || err != nil {
		t.Fatal(on, left, err)
	}
}

// A challenge is not a session, and it ends when the password changes, the
// account is disabled, or two-factor sign-in is reset between the steps.
func TestChallengeEndsWithAccountChanges(t *testing.T) {
	s, c, _ := newService(t, nil)
	c.now = 1_700_000_000_000
	addUser(t, s, adminID, "admin", db.RoleAdmin, "correct horse battery staple")
	secret, _ := enrolled(t, s, c)
	token := challengeFor(t, s, "192.0.2.2")
	if principal, err := s.Session(ctx, token); principal != nil || err != nil {
		t.Fatal("challenge works as a session")
	}
	hash, _ := Argon2{}.Hash("another long password")
	if err := s.DB.UpdateUserPassword(ctx, adminID, hash, resetAudit); err != nil {
		t.Fatal(err)
	}
	c.now += totpPeriodMs
	if _, err := s.CompleteLogin(ctx, token, hotp(secret, totpStep(c.now)), Client{}); !apperr.Is(err, "SIGN_IN_EXPIRED") {
		t.Fatalf("after reset: %v", err)
	}
	if record, _ := s.DB.FindUserByID(ctx, adminID); record.TwoFactor {
		t.Fatal("recovery kept two-factor sign-in")
	}
	if _, err := s.CompleteLogin(ctx, "", "123456", Client{}); !apperr.Is(err, "SIGN_IN_EXPIRED") {
		t.Fatal(err)
	}
	c.now += ChallengeTTLMs
	if _, err := s.CompleteLogin(ctx, token, "123456", Client{}); !apperr.Is(err, "SIGN_IN_EXPIRED") {
		t.Fatal(err)
	}
}

// Wrong codes count against the sign-in limits: one challenge takes five,
// one source five for the account, and the account ten across sources.
func TestWrongCodesAreThrottled(t *testing.T) {
	s, c, _ := newService(t, nil)
	c.now = 1_700_000_000_000
	addUser(t, s, adminID, "admin", db.RoleAdmin, "correct horse battery staple")
	secret, _ := enrolled(t, s, c)
	wrong := hotp(secret, totpStep(c.now)+5)
	token := challengeFor(t, s, "192.0.2.2")
	for attempt := range challengeAttempts {
		if _, err := s.CompleteLogin(ctx, token, wrong, Client{IP: "192.0.2.2"}); !apperr.Is(err, "INVALID_CODE") {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
	// A used-up challenge starts over even while its source is blocked.
	if _, err := s.CompleteLogin(ctx, token, hotp(secret, totpStep(c.now)+1), Client{IP: "192.0.2.2"}); !apperr.Is(err, "SIGN_IN_EXPIRED") {
		t.Fatalf("exhausted challenge: %v", err)
	}
	if _, err := s.Login(ctx, "admin", "correct horse battery staple", Client{IP: "192.0.2.2"}); !apperr.Is(err, "TOO_MANY_ATTEMPTS") {
		t.Fatalf("source not blocked: %v", err)
	}
	for attempt := range loginCodeAccountMax - challengeAttempts {
		ip := "198.51.100." + itoa(attempt+1)
		if _, err := s.CompleteLogin(ctx, challengeFor(t, s, ip), wrong, Client{IP: ip}); !apperr.Is(err, "INVALID_CODE") {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
	token = challengeFor(t, s, "203.0.113.9")
	if _, err := s.CompleteLogin(ctx, token, hotp(secret, totpStep(c.now)+1), Client{IP: "203.0.113.9"}); !apperr.Is(err, "TOO_MANY_ATTEMPTS") {
		t.Fatalf("account not blocked: %v", err)
	}
	c.now += loginWindowMs
	token = challengeFor(t, s, "203.0.113.9")
	if _, err := s.CompleteLogin(ctx, token, hotp(secret, totpStep(c.now)), Client{IP: "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
}

// The secret is stored sealed to its account, and no audit entry carries it
// or a recovery code.
func TestTwoFactorSecretStaysSealed(t *testing.T) {
	s, c, _ := newService(t, nil)
	c.now = 1_700_000_000_000
	addUser(t, s, adminID, "admin", db.RoleAdmin, "correct horse battery staple")
	secret, codes := enrolled(t, s, c)
	encoded := secretEncoding.EncodeToString(secret)
	var stored string
	if err := s.DB.QueryRowContext(ctx, "SELECT totp_secret FROM users WHERE id = ?", adminID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, encoded) || !strings.HasPrefix(stored, "aes-256-gcm:") {
		t.Fatal(stored)
	}
	if _, err := s.DB.Unseal(totpPurpose, "another-user", stored); err == nil {
		t.Fatal("secret opened for another account")
	}
	var audit string
	s.DB.QueryRowContext(ctx, "SELECT group_concat(action || COALESCE(details_json, ''), ' ') FROM audit_log").Scan(&audit)
	if strings.Contains(audit, encoded) || strings.Contains(audit, codes[1]) || !strings.Contains(audit, "auth.two-factor.enabled") {
		t.Fatal(audit)
	}
}

// Only another administrator's session can reset an account's two-factor
// sign-in, and the reset signs that account out.
func TestResetTwoFactorNeedsAnotherAdmin(t *testing.T) {
	s, c, _ := newService(t, nil)
	c.now = 1_700_000_000_000
	addUser(t, s, adminID, "admin", db.RoleAdmin, "correct horse battery staple")
	addUser(t, s, "u-operator", "operator", db.RoleOperator, "operator long password")
	addUser(t, s, "u-admin2", "admin2", db.RoleAdmin, "second admin password")
	secret, _ := enrolled(t, s, c)
	operator, err := s.Login(ctx, "operator", "operator long password", Client{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResetTwoFactor(ctx, operator, adminID, Client{}); !apperr.Is(err, "FORBIDDEN") {
		t.Fatal(err)
	}
	// A stale session role is checked again against the database.
	operator.User.Role = db.RoleAdmin
	if err := s.ResetTwoFactor(ctx, operator, adminID, Client{}); !apperr.Is(err, "FORBIDDEN") {
		t.Fatal(err)
	}
	c.now += totpPeriodMs
	admin, err := s.CompleteLogin(ctx, challengeFor(t, s, "192.0.2.5"), hotp(secret, totpStep(c.now)), Client{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResetTwoFactor(ctx, admin, adminID, Client{}); !apperr.Is(err, "SELF_TWO_FACTOR_RESET") {
		t.Fatal(err)
	}
	other, err := s.Login(ctx, "admin2", "second admin password", Client{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResetTwoFactor(ctx, other, adminID, Client{}); err != nil {
		t.Fatal(err)
	}
	if current, _ := s.Session(ctx, admin.Token); current != nil {
		t.Fatal("reset kept the account's sessions")
	}
	if _, err := s.Login(ctx, "admin", "correct horse battery staple", Client{}); err != nil {
		t.Fatal(err)
	}
}

// A password sign-in verified before two-factor sign-in turned on can't
// create a session afterward.
func TestPasswordSessionLosesToEnrollment(t *testing.T) {
	s, c, _ := newService(t, nil)
	c.now = 1_700_000_000_000
	addUser(t, s, adminID, "admin", db.RoleAdmin, "correct horse battery staple")
	record, _ := s.DB.FindUserByID(ctx, adminID)
	enrolled(t, s, c)
	_, err := s.createSession(ctx, db.SessionUser{ID: adminID, Username: "admin", Role: db.RoleAdmin}, Client{}, record.PasswordHash, nil)
	if !errors.Is(err, db.ErrAccountChanged) {
		t.Fatal(err)
	}
}

// Concurrent guesses on one challenge, each from a different source, still
// get only the challenge's five attempts.
func TestConcurrentCodesShareTheLimit(t *testing.T) {
	s, c, _ := newService(t, nil)
	c.now = 1_700_000_000_000
	addUser(t, s, adminID, "admin", db.RoleAdmin, "correct horse battery staple")
	secret, _ := enrolled(t, s, c)
	token := challengeFor(t, s, "192.0.2.2")
	wrong := hotp(secret, totpStep(c.now)+5)
	var wg sync.WaitGroup
	var mu sync.Mutex
	checked := 0
	for attempt := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.CompleteLogin(ctx, token, wrong, Client{IP: "198.51.100." + itoa(attempt+1)})
			if apperr.Is(err, "INVALID_CODE") {
				mu.Lock()
				checked++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if checked != challengeAttempts {
		t.Fatalf("%d codes checked", checked)
	}
}

// Setting up two-factor sign-in belongs to the session that started it:
// another session of the account can't see or confirm the secret.
func TestPendingSecretStaysWithItsSession(t *testing.T) {
	s, c, _ := newService(t, nil)
	c.now = 1_700_000_000_000
	addUser(t, s, adminID, "admin", db.RoleAdmin, "correct horse battery staple")
	laptop, _ := s.Login(ctx, "admin", "correct horse battery staple", Client{})
	stolen, _ := s.Login(ctx, "admin", "correct horse battery staple", Client{})
	enrollment, err := s.BeginTwoFactor(ctx, laptop)
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := secretEncoding.DecodeString(enrollment.Secret)
	if pending, err := s.PendingTwoFactor(ctx, stolen); pending != nil || err != nil {
		t.Fatal(pending, err)
	}
	if _, _, err := s.EnableTwoFactor(ctx, stolen, "correct horse battery staple", hotp(secret, totpStep(c.now)), Client{}); !apperr.Is(err, "TWO_FACTOR_CHANGED") {
		t.Fatal(err)
	}
	if pending, err := s.PendingTwoFactor(ctx, laptop); pending == nil || pending.Secret != enrollment.Secret || err != nil {
		t.Fatal(pending, err)
	}
}

// Disabling an account ends its pending sign-ins, even if it is enabled
// again before they expire.
func TestDisablingEndsChallenges(t *testing.T) {
	s, c, _ := newService(t, nil)
	c.now = 1_700_000_000_000
	addUser(t, s, adminID, "admin", db.RoleAdmin, "correct horse battery staple")
	addUser(t, s, "u-admin2", "admin2", db.RoleAdmin, "second admin password")
	secret, _ := enrolled(t, s, c)
	token := challengeFor(t, s, "192.0.2.2")
	other, err := s.Login(ctx, "admin2", "second admin password", Client{})
	if err != nil {
		t.Fatal(err)
	}
	for _, disabled := range []bool{true, false} {
		if _, err := s.UpdateAccess(ctx, other, adminID, db.RoleAdmin, disabled, Client{}); err != nil {
			t.Fatal(err)
		}
	}
	c.now += totpPeriodMs
	if _, err := s.CompleteLogin(ctx, token, hotp(secret, totpStep(c.now)), Client{}); !apperr.Is(err, "SIGN_IN_EXPIRED") {
		t.Fatal(err)
	}
}
