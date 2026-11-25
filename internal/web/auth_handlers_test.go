package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/db"
)

// A crafted sign-in link can't send the browser to another site. Browsers
// drop tabs and newlines from a Location, so "/\t/evil.example" would become
// "//evil.example".
func TestLoginNextStaysOnSite(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	for next, want := range map[string]string{
		"/servers?view=list":    "/servers?view=list",
		"//evil.example":        "/",
		"/\\evil.example":       "/",
		"/\t/evil.example":      "/",
		"/\n/evil.example":      "/",
		"/\x00/evil.example":    "/",
		"https://evil.example/": "/",
		"/login?next=/x":        "/",
	} {
		response := p.do("GET", "/login?next="+url.QueryEscape(next), p.admin, nil, nil)
		if response.Code != http.StatusSeeOther || response.Header().Get("Location") != want {
			t.Errorf("next %q: %d %q, want %q", next, response.Code, response.Header().Get("Location"), want)
		}
	}
}

// Signing out over HTTPS clears the session cookie with the same Secure
// attribute it was set with.
func TestLogoutClearsSecureCookie(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	response := p.do("POST", "/logout", p.admin, nil, map[string]string{
		"X-Forwarded-Proto": "https", "Origin": "https://panel.example", "X-CSRF-Token": CSRFToken(p.admin.Token)})
	if response.Code != http.StatusSeeOther {
		t.Fatal(response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != auth.SessionCookie || cookies[0].MaxAge >= 0 || !cookies[0].Secure {
		t.Fatalf("%+v", cookies)
	}
}

// totp computes the authenticator code for a base32 secret at a time in Unix
// milliseconds.
func totp(t *testing.T, secret string, nowMs int64) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(nowMs/30_000))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[offset:])&0x7fffffff)%1_000_000)
}

// Between the password and the code, the browser holds only a challenge
// cookie scoped to /login. Neither it nor its value works as a session,
// a code without the challenge starts over, and a session cookie arrives
// only once the code is accepted.
func TestSignInChallengeIsNotASession(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleMember)
	// The password check can take longer than a code lives on a busy machine.
	now := time.Now().UnixMilli()
	p.app.Auth.Now = func() int64 { return now }
	enrollment, err := p.app.Auth.BeginTwoFactor(context.Background(), operator)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.app.Auth.EnableTwoFactor(context.Background(), operator, "password-u-operator", totp(t, enrollment.Secret, now), auth.Client{}); err != nil {
		t.Fatal(err)
	}
	form := func(values url.Values) io.Reader { return strings.NewReader(values.Encode()) }
	response := p.do("POST", "/login", nil, form(url.Values{"username": {"operator"}, "password": {"password-u-operator"}}), nil)
	cookies := response.Result().Cookies()
	if response.Code != http.StatusOK || len(cookies) != 1 || cookies[0].Name != auth.ChallengeCookie || cookies[0].Path != "/login" ||
		!cookies[0].HttpOnly || !strings.Contains(response.Body.String(), `action="/login/code"`) {
		t.Fatal(response.Code, cookies, response.Body.String())
	}
	challenge := cookies[0].Value
	for _, cookie := range []string{auth.SessionCookie + "=" + challenge, auth.ChallengeCookie + "=" + challenge} {
		for _, target := range []string{"/", "/account", "/fragments/servers"} {
			request := httptest.NewRequest("GET", "http://panel.example"+target, nil)
			request.Header.Set("Cookie", cookie)
			recorder := httptest.NewRecorder()
			p.handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusSeeOther || !strings.HasPrefix(recorder.Header().Get("Location"), "/login") {
				t.Fatalf("%s with %s: %d", target, cookie, recorder.Code)
			}
		}
	}
	response = p.do("POST", "/login/code", nil, form(url.Values{"code": {"123456"}}), nil)
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "Your sign-in expired") {
		t.Fatal(response.Code, response.Body.String())
	}
	request := httptest.NewRequest("POST", "http://panel.example/login/code", form(url.Values{"code": {"not-a-code"}, "next": {"/account"}}))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Cookie", auth.ChallengeCookie+"="+challenge)
	recorder := httptest.NewRecorder()
	p.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || len(recorder.Result().Cookies()) != 0 ||
		!strings.Contains(recorder.Body.String(), "That code didn&#39;t work") {
		t.Fatal(recorder.Code, recorder.Result().Cookies(), recorder.Body.String())
	}
}
