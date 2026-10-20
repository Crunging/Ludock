package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/identity"
)

var ctx = context.Background()

func TestMain(m *testing.M) {
	// Keep the KDF cheap; parameter handling is covered explicitly.
	argonMemoryKiB = 1024
	os.Exit(m.Run())
}

const setupCode = "fixture-setup-throttle-code-0123456789abcdef"

// fastHasher uses cheap Argon2 parameters; gates let tests change accounts
// at the exact point where password work would yield.
type fastHasher struct {
	mu           sync.Mutex
	hashes       int
	beforeVerify func()
	beforeHash   func()
}

func (h *fastHasher) Hash(password string) (string, error) {
	h.mu.Lock()
	h.hashes++
	hook := h.beforeHash
	h.mu.Unlock()
	if hook != nil {
		hook()
	}
	return hashWith(password, argonMemoryKiB, argonTime, argonThreads)
}

func (h *fastHasher) Verify(password, encoded string) bool {
	result := Argon2{}.Verify(password, encoded)
	if h.beforeVerify != nil {
		h.beforeVerify()
	}
	return result
}

type clock struct{ now int64 }

func newService(t *testing.T, d *db.DB) (*Service, *clock, *fastHasher) {
	t.Helper()
	if d == nil {
		d = dbtest.Open(t)
	}
	c := &clock{now: 10_000}
	window, err := NewSetupWindow(func() int64 { return c.now }, 300_000, setupCode)
	if err != nil {
		t.Fatal(err)
	}
	s := NewService(d, &authz.Service{DB: d, Servers: &identity.Store{DB: d}}, window)
	s.Now = func() int64 { return c.now }
	h := &fastHasher{}
	s.Hasher = h
	return s, c, h
}

func addUser(t *testing.T, s *Service, id, username string, role db.Role, password string) {
	t.Helper()
	hash, _ := Argon2{}.Hash(password)
	if err := s.DB.CreateUser(ctx, db.UserRecord{User: db.User{ID: id, Username: username, Role: role, CreatedAt: 1}, PasswordHash: hash}); err != nil {
		t.Fatal(err)
	}
}

func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	public, ok := apperr.As(err)
	if !ok || public.Status != status {
		t.Fatalf("err = %v, want status %d", err, status)
	}
}

func TestPasswordEncoding(t *testing.T) {
	valid, _ := Argon2{}.Hash("a-long-pássword-🙂")
	if !(Argon2{}).Verify("a-long-pássword-🙂", valid) || (Argon2{}).Verify("wrong", valid) || NeedsUpgrade(valid) {
		t.Fatal("round trip failed")
	}
	for _, encoded := range []string{
		strings.Replace(valid, fmt.Sprintf("m=%d", argonMemoryKiB), "m=9999999999", 1),
		strings.Replace(valid, "t=2", "t=9999999999", 1),
		strings.Replace(valid, fmt.Sprintf("m=%d", argonMemoryKiB), "m=4", 1),
		valid[:strings.LastIndex(valid, "$")] + "$!",
		"$argon2id$" + strings.Repeat("a", 300),
		strings.Replace(valid, "$argon2id$", "$argon2i$", 1),
	} {
		if (Argon2{}).Verify("a-long-pássword-🙂", encoded) {
			t.Errorf("accepted %q", encoded)
		}
	}
	weak, _ := hashWith("x", 512, 1, 1)
	if !NeedsUpgrade(weak) || !(Argon2{}).Verify("x", weak) {
		t.Fatal("weak hash handling")
	}
}

func TestPasswordWorkAdmission(t *testing.T) {
	p := NewPasswordWork()
	release := make(chan struct{})
	var started, done sync.WaitGroup
	hold := func(source, subject string) {
		started.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			p.Do(source, subject, func() error { started.Done(); <-release; return nil })
		}()
		started.Wait()
	}
	try := func(source, subject string) error { return p.Do(source, subject, func() error { return nil }) }
	hold("source-a", "subject-a")
	if !errors.Is(try("source-b", "subject-a"), ErrPasswordWorkBusy) {
		t.Fatal("one credential took two slots")
	}
	hold("source-a", "subject-b")
	if !errors.Is(try("source-a", "subject-c"), ErrPasswordWorkBusy) {
		t.Fatal("one source took three slots")
	}
	hold("source-c", "subject-c")
	hold("source-d", "subject-d")
	if !errors.Is(try("source-e", "subject-e"), ErrPasswordWorkBusy) {
		t.Fatal("global limit not enforced")
	}
	close(release)
	done.Wait()
	if try("source-a", "subject-a") != nil {
		t.Fatal("slots not released")
	}
}

func TestSetupCodeThrottling(t *testing.T) {
	s, _, h := newService(t, nil)
	wrong := "fixture-incorrect-code-0123456789abcdef"
	for failure := 0; failure < 20; failure++ {
		code := ""
		if failure%2 == 1 {
			code = wrong
		}
		_, err := s.CompleteSetup(ctx, "x", "short", code, Client{IP: "192.0.2.10"})
		if !apperr.Is(err, "SETUP_AUTHORIZATION_REQUIRED") || err.Error() != "Initial setup authorization failed" {
			t.Fatal(err)
		}
	}
	for _, code := range []string{"", wrong, setupCode} {
		_, err := s.CompleteSetup(ctx, "x", "short", code, Client{IP: "192.0.2.10"})
		wantStatus(t, err, 429)
		if err.Error() != "Too many attempts. Try again later." {
			t.Fatal(err)
		}
	}
	_, err := s.CompleteSetup(ctx, "x", "short", setupCode, Client{IP: "198.51.100.20"})
	wantStatus(t, err, 400)
	if h.hashes != 0 {
		t.Fatal("password work before authorization")
	}
	if count, _ := s.DB.CountUsers(ctx); count != 0 {
		t.Fatal(count)
	}
	principal, err := s.CompleteSetup(ctx, " first-admin ", "fixture-correct-password", setupCode, Client{IP: "198.51.100.20"})
	if err != nil || principal.User.Username != "first-admin" || principal.User.Role != db.RoleAdmin {
		t.Fatal(principal, err)
	}
	if _, err := s.Session(ctx, principal.Token); err != nil {
		t.Fatal(err)
	}
	_, err = s.CompleteSetup(ctx, "second", "fixture-correct-password", setupCode, Client{IP: "198.51.100.21"})
	if !apperr.Is(err, "SETUP_COMPLETE") {
		t.Fatal(err)
	}
}

func TestSetupWindowExpires(t *testing.T) {
	s, c, _ := newService(t, nil)
	c.now = s.Setup.ExpiresAt
	_, err := s.CompleteSetup(ctx, "admin", "locked-test-password", setupCode, Client{})
	wantStatus(t, err, 403)
	if err.Error() != "Initial setup has expired. Restart the panel to reopen setup." {
		t.Fatal(err)
	}
	state, _ := s.SetupState(ctx)
	if !state.Required || !state.Locked || state.RemainingMs != 0 {
		t.Fatalf("%+v", state)
	}
}

func TestConcurrentSetupCreatesOneAdmin(t *testing.T) {
	s, _, _ := newService(t, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for index, name := range []string{"legit-admin", "attacker", "attacker-two", "attacker-three"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.CompleteSetup(ctx, name, "race-test-password-1", setupCode, Client{IP: "192.0.2." + strconv.Itoa(index+1)})
			if err == nil {
				mu.Lock()
				created++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if count, _ := s.DB.CountUsers(ctx); created != 1 || count != 1 {
		t.Fatalf("created %d, users %d", created, count)
	}
}

const password = "fixture-correct-password"

func TestLoginSourceLockoutKeepsOtherClients(t *testing.T) {
	s, _, _ := newService(t, nil)
	addUser(t, s, "80000000-0000-4000-8000-000000000001", "admin", db.RoleAdmin, password)
	for failure := 0; failure < 20; failure++ {
		_, err := s.Login(ctx, "missing"+string(rune('0'+failure/5)), "fixture-incorrect-password", Client{IP: "198.51.100.10"})
		wantStatus(t, err, 401)
	}
	_, err := s.Login(ctx, "admin", password, Client{IP: "198.51.100.10"})
	wantStatus(t, err, 429)
	if _, err := s.Login(ctx, "admin", password, Client{IP: "203.0.113.20"}); err != nil {
		t.Fatal(err)
	}
}

func TestAccountCooldownIsCappedAndNotExtended(t *testing.T) {
	s, c, _ := newService(t, nil)
	addUser(t, s, "80000000-0000-4000-8000-000000000001", "admin", db.RoleAdmin, password)
	accountKey := HashToken("login-account-failures:admin")
	throttle := func() db.LoginThrottle {
		value, _ := s.DB.GetLoginThrottle(ctx, accountKey, c.now, loginWindowMs)
		return value
	}
	for failure := 0; failure < 50; failure++ {
		c.now = max(c.now, throttle().BlockedUntil)
		username := "admin"
		if failure%2 == 1 {
			username = " Admin "
		}
		_, err := s.Login(ctx, username, "fixture-incorrect-password", Client{IP: "192.0.2." + itoa(failure+1)})
		wantStatus(t, err, 401)
		current := throttle()
		if current.Failures != failure+1 {
			t.Fatalf("failure %d: %+v", failure, current)
		}
		if failure < 19 {
			if current.BlockedUntil != 0 {
				t.Fatalf("failure %d blocked", failure)
			}
			continue
		}
		expected := min(int64(5000), int64(250)<<((failure+1-20)/5))
		if current.BlockedUntil-c.now != expected {
			t.Fatalf("failure %d: cooldown %d, want %d", failure, current.BlockedUntil-c.now, expected)
		}
		c.now = current.BlockedUntil - 1
		_, err = s.Login(ctx, "admin", "fixture-incorrect-password", Client{IP: "198.51.100.20"})
		public, _ := apperr.As(err)
		if public == nil || public.Status != 429 || public.RetryAfterMs != 1 {
			t.Fatalf("blocked attempt: %v", err)
		}
		if throttle() != current {
			t.Fatal("rejected attempt extended cooldown")
		}
	}
	c.now = throttle().BlockedUntil
	if _, err := s.Login(ctx, "admin", password, Client{IP: "198.51.100.21"}); err != nil {
		t.Fatal(err)
	}
	if throttle() != (db.LoginThrottle{}) {
		t.Fatal("success did not clear cooldown")
	}
}

func itoa(value int) string { return strconv.Itoa(value) }

const (
	adminID  = "11111111-1111-4111-8111-111111111111"
	targetID = "22222222-2222-4222-8222-222222222222"
	original = "original-password-123"
)

func racingService(t *testing.T) (*Service, *fastHasher, *Principal) {
	s, _, h := newService(t, nil)
	addUser(t, s, adminID, "admin", db.RoleAdmin, original)
	addUser(t, s, targetID, "target", db.RoleAdmin, original)
	principal, err := s.Login(ctx, "admin", original, Client{IP: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	return s, h, principal
}

func TestLoginRejectsAccountChangedDuringVerification(t *testing.T) {
	for _, change := range []string{"reset", "disable"} {
		s, h, _ := racingService(t)
		h.beforeVerify = func() {
			if change == "reset" {
				hash, _ := Argon2{}.Hash("reset-password-123")
				s.DB.UpdateUserPassword(ctx, adminID, hash)
			} else {
				s.DB.UpdateUserAccess(ctx, adminID, db.RoleAdmin, true)
			}
		}
		if user, err := s.Authenticate(ctx, "admin", original); user != nil || err != nil {
			t.Fatalf("%s: %v %v", change, user, err)
		}
	}
}

func TestSessionRequiresTheVerifiedPassword(t *testing.T) {
	for _, change := range []string{"none", "reset", "disable"} {
		s, _, _ := racingService(t)
		record, _ := s.DB.FindUserByID(ctx, adminID)
		// The account changes after its password was verified.
		switch change {
		case "reset":
			hash, _ := Argon2{}.Hash("reset-password-123")
			s.DB.UpdateUserPassword(ctx, adminID, hash)
		case "disable":
			s.DB.UpdateUserAccess(ctx, adminID, db.RoleAdmin, true)
		}
		_, err := s.createSession(ctx, db.SessionUser{ID: adminID, Username: "admin", Role: db.RoleAdmin}, Client{IP: "127.0.0.1"},
			record.PasswordHash)
		if (change == "none") != (err == nil) || (err != nil && !errors.Is(err, db.ErrAccountChanged)) {
			t.Fatalf("%s: %v", change, err)
		}
	}
}

func TestHashUpgradeDoesNotOverwriteReset(t *testing.T) {
	s, h, _ := racingService(t)
	weak, _ := hashWith(original, 512, 1, 1)
	s.DB.UpdateUserPassword(ctx, adminID, weak)
	reset, _ := Argon2{}.Hash("reset-password-123")
	h.beforeHash = func() { s.DB.UpdateUserPassword(ctx, adminID, reset) }
	if user, _ := s.Authenticate(ctx, "admin", original); user != nil {
		t.Fatal("authenticated after reset")
	}
	if record, _ := s.DB.FindUserByID(ctx, adminID); record.PasswordHash != reset {
		t.Fatal("upgrade overwrote reset")
	}
}

func TestLoginReturnsCurrentRole(t *testing.T) {
	s, h, _ := racingService(t)
	h.beforeVerify = func() { s.DB.UpdateUserAccess(ctx, adminID, db.RoleViewer, false) }
	if user, _ := s.Authenticate(ctx, "admin", original); user == nil || user.Role != db.RoleViewer {
		t.Fatal(user)
	}
}

func TestAdminChangesRejectedAfterDemotionOrRevocation(t *testing.T) {
	for _, c := range []struct {
		operation, change string
		status            int
	}{{"create", "demote", 403}, {"reset", "revoke-session", 401}} {
		s, h, principal := racingService(t)
		h.beforeHash = func() {
			if c.change == "demote" {
				s.DB.UpdateUserAccess(ctx, adminID, db.RoleViewer, false)
			} else {
				s.DB.DeleteUserSessions(ctx, adminID)
			}
		}
		before, _ := s.DB.FindUserByID(ctx, targetID)
		var err error
		if c.operation == "create" {
			_, err = s.CreateUser(ctx, principal, "new-admin", "new-password-123", db.RoleAdmin, Client{})
		} else {
			err = s.ResetPassword(ctx, principal, targetID, "new-password-123", Client{})
		}
		wantStatus(t, err, c.status)
		if user, _ := s.DB.FindUserByUsername(ctx, "new-admin"); user != nil {
			t.Fatal("user created")
		}
		if after, _ := s.DB.FindUserByID(ctx, targetID); after.PasswordHash != before.PasswordHash {
			t.Fatal("password reset")
		}
	}
}

func TestPasswordChangeDoesNotRestoreRevokedSession(t *testing.T) {
	s, h, principal := racingService(t)
	reset, _ := Argon2{}.Hash("reset-password-123")
	h.beforeHash = func() { s.DB.UpdateUserPassword(ctx, adminID, reset) }
	_, err := s.ChangePassword(ctx, principal, original, "new-password-123", Client{})
	wantStatus(t, err, 401)
	if record, _ := s.DB.FindUserByID(ctx, adminID); record.PasswordHash != reset {
		t.Fatal("change overwrote reset")
	}
	if sessions, _ := s.DB.ListUserSessions(ctx, adminID, ""); len(sessions) != 0 {
		t.Fatal("session restored")
	}
}

func TestPasswordChangeReplacesSessions(t *testing.T) {
	s, _, principal := racingService(t)
	other, _ := s.Login(ctx, "admin", original, Client{IP: "192.0.2.9"})
	_, err := s.ChangePassword(ctx, principal, "wrong-password", "new-password-123", Client{})
	if !apperr.Is(err, "INVALID_PASSWORD") {
		t.Fatal(err)
	}
	next, err := s.ChangePassword(ctx, principal, original, "new-password-123", Client{})
	if err != nil {
		t.Fatal(err)
	}
	for _, old := range []*Principal{principal, other} {
		if current, _ := s.Session(ctx, old.Token); current != nil {
			t.Fatal("old session survived")
		}
	}
	if current, _ := s.Session(ctx, next.Token); current == nil {
		t.Fatal("replacement session missing")
	}
	var failures int
	s.DB.QueryRow("SELECT COUNT(*) FROM login_attempts WHERE attempt_key = ?",
		throttleKey("password-change-credential", principal.TokenHash)).Scan(&failures)
	if failures != 0 {
		t.Fatal("failed attempt was not cleared")
	}
}

func TestLastAdminIsProtected(t *testing.T) {
	s, _, principal := racingService(t)
	if err := s.DeleteUser(ctx, principal, adminID, Client{}); !apperr.Is(err, "SELF_DELETE") {
		t.Fatal(err)
	}
	if _, err := s.UpdateAccess(ctx, principal, targetID, db.RoleViewer, false, Client{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateAccess(ctx, principal, adminID, db.RoleAdmin, true, Client{}); !apperr.Is(err, "LAST_ADMIN") {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, principal, targetID, Client{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, principal, "Admin", "new-password-123", db.RoleViewer, Client{}); !apperr.Is(err, "USERNAME_TAKEN") {
		t.Fatal(err)
	}
}

func TestSessionCookieParsing(t *testing.T) {
	r := httptestRequestWithCookie("other=1; ludock_session=first; ludock_session=se%63ond; bad%ZZ=x")
	if got := SessionToken(r); got != "second" {
		t.Fatal(got)
	}
	if got := SessionToken(httptestRequestWithCookie("ludock_session=%ZZ")); got != "" {
		t.Fatal(got)
	}
}
