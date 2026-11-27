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
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/identity"
)

var ctx = context.Background()

// resetAudit records password resets made directly in the database.
var resetAudit = db.AuditEntry{Action: "user.password.recovered", TargetType: "user", TargetID: adminID}

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
	window, err := NewSetup(setupCode)
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
		if !apperr.Is(err, "SETUP_AUTHORIZATION_REQUIRED") || code == wrong && err.Error() != "That setup code didn’t work. Use the latest one in Ludock’s log, or the one you set as LUDOCK_SETUP_CODE." {
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

// Setup has no deadline: the code still works long after startup.
func TestSetupStaysOpenUntilCompleted(t *testing.T) {
	s, c, _ := newService(t, nil)
	c.now += 30 * 24 * 60 * 60 * 1000
	if _, err := s.CompleteSetup(ctx, "admin", "late-but-correct-password", setupCode, Client{IP: "198.51.100.30"}); err != nil {
		t.Fatal(err)
	}
	if state, _ := s.SetupState(ctx); state.Required {
		t.Fatalf("%+v", state)
	}
}

// An unowned Ludock can be set up without the code for its first ten
// minutes of elapsed time, unless the owner set LUDOCK_SETUP_CODE. A code
// that is sent must match, the window is judged again as the account is
// created, a wall clock set back after start doesn't keep it open, and once
// an administrator exists nothing makes another.
func TestSetupOpenWindow(t *testing.T) {
	wrong := "fixture-incorrect-code-0123456789abcdef"
	for _, tc := range []struct {
		name       string
		configured bool
		// after is the time elapsed before the attempt; closesDuring passes
		// while the password is hashed. wallBack sets the wall clock back an
		// hour after start.
		after, closesDuring time.Duration
		wallBack            bool
		code                string
		want                string
	}{
		{name: "no code inside the window", after: 10*time.Minute - time.Millisecond},
		{name: "right code inside the window", code: "right"},
		{name: "wrong code inside the window", code: wrong, want: "SETUP_AUTHORIZATION_REQUIRED"},
		{name: "no code after the window", after: 10 * time.Minute, want: "SETUP_AUTHORIZATION_REQUIRED"},
		{name: "wrong code after the window", after: 10 * time.Minute, code: wrong, want: "SETUP_AUTHORIZATION_REQUIRED"},
		{name: "right code after the window", after: 10 * time.Minute, code: "right"},
		{name: "window closes while hashing", after: 10*time.Minute - time.Millisecond, closesDuring: time.Millisecond, want: "SETUP_AUTHORIZATION_REQUIRED"},
		{name: "wall clock set back an hour", after: 10 * time.Minute, wallBack: true, want: "SETUP_AUTHORIZATION_REQUIRED"},
		{name: "no code with a configured code", configured: true, want: "SETUP_AUTHORIZATION_REQUIRED"},
		{name: "configured code", configured: true, code: "right"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c, h := newService(t, nil)
			code := setupCode
			if !tc.configured {
				s.Setup, _ = NewSetup("")
			}
			c.now = 24 * time.Hour.Milliseconds()
			start, elapsed := time.Now(), time.Duration(0)
			s.Setup.Now = func() time.Time { return start.Add(elapsed) }
			pass := func(d time.Duration) {
				elapsed += d
				c.now += d.Milliseconds()
			}
			var console string
			s.OpenSetup(ctx, "http://localhost:3000", func(text string) { console += text })
			if !tc.configured {
				code = console[strings.LastIndex(console, " ")+1 : len(console)-1]
				if !strings.Contains(console, "http://localhost:3000 for 10 minutes") {
					t.Fatal(console)
				}
			} else if console != "" {
				t.Fatal("configured code printed")
			}
			if tc.code == "right" {
				tc.code = code
			}
			if tc.wallBack {
				c.now -= time.Hour.Milliseconds()
			}
			pass(tc.after)
			h.beforeHash = func() { pass(tc.closesDuring) }
			_, err := s.CompleteSetup(ctx, "first-admin", password, tc.code, Client{IP: "192.0.2.10"})
			count, _ := s.DB.CountUsers(ctx)
			if tc.want != "" {
				if !apperr.Is(err, tc.want) || count != 0 {
					t.Fatalf("err = %v, users %d", err, count)
				}
				return
			}
			if err != nil || count != 1 {
				t.Fatalf("err = %v, users %d", err, count)
			}
			s.OpenSetup(ctx, "http://localhost:3000", func(string) {})
			for _, again := range []string{"", code} {
				if _, err := s.CompleteSetup(ctx, "second-admin", password, again, Client{IP: "192.0.2.11"}); !apperr.Is(err, "SETUP_COMPLETE") {
					t.Fatal(err)
				}
			}
		})
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

// Account cooldown is capped and not extended.
func TestAccountCooldownCap(t *testing.T) {
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

// Login rejects account changed during verification.
func TestLoginDuringAccountChange(t *testing.T) {
	for _, change := range []string{"reset", "disable"} {
		s, h, _ := racingService(t)
		h.beforeVerify = func() {
			if change == "reset" {
				hash, _ := Argon2{}.Hash("reset-password-123")
				s.DB.UpdateUserPassword(ctx, adminID, hash, resetAudit)
			} else {
				dbtest.UpdateUserAccess(ctx, s.DB, adminID, db.RoleAdmin, true)
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
			s.DB.UpdateUserPassword(ctx, adminID, hash, resetAudit)
		case "disable":
			dbtest.UpdateUserAccess(ctx, s.DB, adminID, db.RoleAdmin, true)
		}
		_, err := s.createSession(ctx, db.SessionUser{ID: adminID, Username: "admin", Role: db.RoleAdmin}, Client{IP: "127.0.0.1"},
			record.PasswordHash, nil)
		if (change == "none") != (err == nil) || (err != nil && !errors.Is(err, db.ErrAccountChanged)) {
			t.Fatalf("%s: %v", change, err)
		}
	}
}

func TestHashUpgradeDoesNotOverwriteReset(t *testing.T) {
	s, h, _ := racingService(t)
	weak, _ := hashWith(original, 512, 1, 1)
	s.DB.UpdateUserPassword(ctx, adminID, weak, resetAudit)
	reset, _ := Argon2{}.Hash("reset-password-123")
	h.beforeHash = func() { s.DB.UpdateUserPassword(ctx, adminID, reset, resetAudit) }
	if user, _ := s.Authenticate(ctx, "admin", original); user != nil {
		t.Fatal("authenticated after reset")
	}
	if record, _ := s.DB.FindUserByID(ctx, adminID); record.PasswordHash != reset {
		t.Fatal("upgrade overwrote reset")
	}
}

func TestLoginReturnsCurrentRole(t *testing.T) {
	s, h, _ := racingService(t)
	h.beforeVerify = func() { dbtest.UpdateUserAccess(ctx, s.DB, adminID, db.RoleMember, false) }
	if user, _ := s.Authenticate(ctx, "admin", original); user == nil || user.Role != db.RoleMember {
		t.Fatal(user)
	}
}

// Admin changes rejected after demotion or revocation.
func TestAdminChangesAfterDemotion(t *testing.T) {
	for _, c := range []struct {
		operation, change string
		status            int
	}{{"create", "demote", 403}, {"reset", "revoke-session", 401}} {
		s, h, principal := racingService(t)
		h.beforeHash = func() {
			if c.change == "demote" {
				dbtest.UpdateUserAccess(ctx, s.DB, adminID, db.RoleMember, false)
			} else {
				dbtest.DeleteUserSessions(ctx, s.DB, adminID)
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

// Password change does not restore revoked session.
func TestPasswordChangeKeepsRevocation(t *testing.T) {
	s, h, principal := racingService(t)
	reset, _ := Argon2{}.Hash("reset-password-123")
	h.beforeHash = func() { s.DB.UpdateUserPassword(ctx, adminID, reset, resetAudit) }
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
	if _, err := s.ChangeAccess(ctx, principal, targetID, db.RoleMember, new(false), Client{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangeAccess(ctx, principal, adminID, db.RoleAdmin, new(true), Client{}); !apperr.Is(err, "LAST_ADMIN") {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, principal, targetID, Client{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, principal, "Admin", "new-password-123", db.RoleMember, Client{}); !apperr.Is(err, "USERNAME_TAKEN") {
		t.Fatal(err)
	}
}

// A member made an administrator and back starts with nothing shared, so a
// share from before can't come back unnoticed.
func TestAdministratorAndBackStartsWithNothingShared(t *testing.T) {
	s, _, principal := racingService(t)
	if _, err := s.ChangeAccess(ctx, principal, targetID, db.RoleMember, new(false), Client{}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{`INSERT INTO docker_hosts(id,name,created_at) VALUES('h','local',1)`,
		`INSERT INTO logical_servers(id,host_id,external_identity,display_name,game_type,status,binding_revision,binding_fingerprint,first_seen_at,last_seen_at)
      VALUES('s','h','standalone:mc','mc','minecraft','active',1,'',1,1)`,
		`INSERT INTO server_grants VALUES('` + targetID + `','s','["server.view","server.start"]',1)`} {
		if _, err := s.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ChangeAccess(ctx, principal, targetID, db.RoleAdmin, new(false), Client{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangeAccess(ctx, principal, targetID, db.RoleMember, new(false), Client{}); err != nil {
		t.Fatal(err)
	}
	if grants, err := s.Authz.UserGrants(ctx, targetID); err != nil || len(grants) != 0 {
		t.Fatal(grants, err)
	}
	if _, err := s.ChangeAccess(ctx, principal, adminID, db.RoleMember, new(false), Client{}); !apperr.Is(err, "LAST_ADMIN") {
		t.Fatal(err)
	}
	if _, err := s.ChangeAccess(ctx, principal, targetID, "operator", new(false), Client{}); !apperr.Is(err, "INVALID_REQUEST") {
		t.Fatal(err)
	}
}

// A role change from a page opened before the account was disabled leaves
// it disabled, and enabling it from a page opened before a role change keeps
// the new role.
func TestAccessChangesOnlyWhatTheyName(t *testing.T) {
	s, _, principal := racingService(t)
	disable, enable := true, false
	if _, err := s.ChangeAccess(ctx, principal, targetID, db.RoleMember, &disable, Client{}); err != nil {
		t.Fatal(err)
	}
	user, err := s.ChangeAccess(ctx, principal, targetID, db.RoleAdmin, nil, Client{})
	if err != nil || user.Role != db.RoleAdmin || !user.Disabled {
		t.Fatal(user, err)
	}
	if user, err = s.ChangeAccess(ctx, principal, targetID, "", &enable, Client{}); err != nil || user.Role != db.RoleAdmin || user.Disabled {
		t.Fatal(user, err)
	}
	if _, err := s.ChangeAccess(ctx, principal, targetID, "", nil, Client{}); !apperr.Is(err, "INVALID_REQUEST") {
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

func TestFailedRecoveryKeepsTheOldPassword(t *testing.T) {
	s, _, _ := racingService(t)
	before, _ := s.DB.FindUserByID(ctx, adminID)
	if _, err := s.DB.Exec(`CREATE TRIGGER no_recovery_audit BEFORE INSERT ON audit_log WHEN NEW.action = 'user.password.recovered'
		BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverPassword(ctx, s.DB, "admin", "recovered-password-123"); err == nil {
		t.Fatal("recovery reported success without its audit entry")
	}
	if after, _ := s.DB.FindUserByID(ctx, adminID); after.PasswordHash != before.PasswordHash {
		t.Fatal("the password changed although recovery failed")
	}
}

func TestFailedLoginAuditLeavesNoSession(t *testing.T) {
	s, _, _ := newService(t, nil)
	addUser(t, s, adminID, "admin", db.RoleAdmin, original)
	if _, err := s.DB.Exec(`CREATE TRIGGER no_login_audit BEFORE INSERT ON audit_log WHEN NEW.action = 'auth.login'
		BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "admin", original, Client{IP: "127.0.0.1"}); err == nil {
		t.Fatal("signed in without an audit entry")
	}
	var sessions int
	s.DB.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&sessions)
	if sessions != 0 {
		t.Fatal("left a session the client never received")
	}
}

func TestFailedSetupSessionCanBeRetried(t *testing.T) {
	s, _, _ := newService(t, nil)
	if _, err := s.DB.Exec(`CREATE TRIGGER no_session BEFORE INSERT ON sessions
		BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteSetup(ctx, "owner", original, setupCode, Client{IP: "192.0.2.10"}); err == nil {
		t.Fatal("setup succeeded without a session")
	}
	s.DB.Exec("DROP TRIGGER no_session")
	if principal, err := s.CompleteSetup(ctx, "owner", original, setupCode, Client{IP: "192.0.2.10"}); err != nil || principal.Token == "" {
		t.Fatal(principal, err)
	}
}

// User deleted right after a change is not found.
func TestUserDeletedAfterChange(t *testing.T) {
	s, _, principal := racingService(t)
	// Another administrator deletes the account as soon as each change commits.
	for _, statement := range []string{
		`CREATE TRIGGER gone_created AFTER INSERT ON users WHEN NEW.username = 'newcomer' BEGIN DELETE FROM users WHERE id = NEW.id; END`,
		`CREATE TRIGGER gone_changed AFTER UPDATE OF role ON users WHEN NEW.id = '` + targetID + `' BEGIN DELETE FROM users WHERE id = NEW.id; END`,
	} {
		if _, err := s.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	client := Client{IP: "127.0.0.1"}
	if _, err := s.CreateUser(ctx, principal, "newcomer", "a long enough password", db.RoleMember, client); !apperr.Is(err, "USER_NOT_FOUND") {
		t.Fatal("create", err)
	}
	if _, err := s.ChangeAccess(ctx, principal, targetID, db.RoleMember, new(false), client); !apperr.Is(err, "USER_NOT_FOUND") {
		t.Fatal("update", err)
	}
}
