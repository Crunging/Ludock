package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
)

var ctx = context.Background()

const webhook = "https://discord.com/api/webhooks/123/secret-token_value"

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func respond(status int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

// adminSession is the administrator's session token hash in adminDB.
var adminSession string

// adminDB opens a database with an administrator signed in.
func adminDB(t *testing.T) *db.DB {
	t.Helper()
	d := dbtest.Open(t)
	if err := d.CreateUser(ctx, db.UserRecord{User: db.User{ID: "admin", Username: "admin", Role: db.RoleAdmin, CreatedAt: 1}, PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	adminSession = dbtest.Session(t, d, "admin")
	return d
}

func setup(t *testing.T, transport roundTrip) *Service {
	t.Helper()
	s := &Service{DB: adminDB(t), Client: &http.Client{Transport: transport}}
	if err := s.Configure(ctx, "admin", adminSession, Discord, Config{Enabled: true, URL: webhook}, false); err != nil {
		t.Fatal(err)
	}
	return s
}

// setEnabled turns Discord on or off, keeping its saved webhook.
func setEnabled(t *testing.T, s *Service, enabled bool) {
	t.Helper()
	if err := s.Configure(ctx, "admin", adminSession, Discord, Config{Enabled: enabled}, false); err != nil {
		t.Fatal(err)
	}
}

func TestWebhookValidation(t *testing.T) {
	s := &Service{DB: adminDB(t)}
	for _, url := range []string{"http://discord.com/api/webhooks/1/a", "https://evil.example/api/webhooks/1/a",
		"https://discord.com:8443/api/webhooks/1/a", "https://user@discord.com/api/webhooks/1/a",
		"https://discord.com/api/webhooks/1/a?x=1", "https://discord.com/api/webhooks/1/a#x", "https://discord.com/other", "not a url"} {
		if err := s.Configure(ctx, "admin", adminSession, Discord, Config{Enabled: true, URL: url}, false); err == nil {
			t.Errorf("accepted %s", url)
		}
	}
	if err := s.Configure(ctx, "admin", adminSession, Discord, Config{Enabled: true}, false); !apperr.Is(err, "NOTIFICATION_SETTINGS_REQUIRED") {
		t.Fatal(err)
	}
	s.Configure(ctx, "admin", adminSession, Discord, Config{URL: webhook}, false)
	// Saving without a URL keeps the saved one.
	if err := s.Configure(ctx, "admin", adminSession, Discord, Config{Enabled: true}, false); err != nil || !s.Configuration(ctx, Discord).Ready() {
		t.Fatal(err)
	}
}

// Each destination refuses malformed settings, and a destination that is
// on needs every required field.
func TestDestinationValidation(t *testing.T) {
	s := &Service{DB: adminDB(t)}
	for _, c := range []struct {
		d      Destination
		config Config
	}{
		{Webhook, Config{URL: "ftp://example.com/hook"}},
		{Webhook, Config{URL: "https://user:pass@example.com/hook"}},
		{Webhook, Config{URL: "https://example.com/hook#x"}},
		{Webhook, Config{URL: "https://example.com/hook", Token: "Bearer a\r\nX-Other: b"}},
		{Webhook, Config{Enabled: true}},
		{Ntfy, Config{URL: "https://ntfy.example/?x=1", Topic: "alerts"}},
		{Ntfy, Config{Topic: "has spaces"}},
		{Ntfy, Config{Topic: strings.Repeat("a", 65)}},
		{Ntfy, Config{Enabled: true}},
		{Gotify, Config{Enabled: true, URL: "https://gotify.example"}},
		{Gotify, Config{URL: "gotify.example", Token: "abc"}},
		{Telegram, Config{Token: "not-a-token", ChatID: "1"}},
		{Telegram, Config{Token: "123:" + strings.Repeat("a", 35), ChatID: "me"}},
		{Telegram, Config{Enabled: true, Token: "123:" + strings.Repeat("a", 35)}},
		{Email, Config{Host: "smtp example.com", From: "a@example.com", To: "b@example.com"}},
		{Email, Config{Host: "smtp.example.com", Port: 70000, From: "a@example.com", To: "b@example.com"}},
		{Email, Config{Host: "smtp.example.com", Security: "ssl", From: "a@example.com", To: "b@example.com"}},
		{Email, Config{Host: "smtp.example.com", From: "not an address", To: "b@example.com"}},
		{Email, Config{Host: "smtp.example.com", From: "a@example.com", To: "b@example.com\r\nBcc: c@example.com"}},
		{Email, Config{Host: "smtp.example.com", From: "a@example.com", To: strings.Repeat("b@example.com,", 10) + "b@example.com"}},
		{Email, Config{Host: "smtp.example.com", Username: "a", From: "a@example.com", To: "b@example.com"}},
		{Email, Config{Host: "relay", Security: "none", Username: "a", Password: "p", From: "a@example.com", To: "b@example.com"}},
		{Email, Config{Enabled: true, Host: "smtp.example.com", From: "a@example.com"}},
		{Destination("pager"), Config{}},
	} {
		if err := s.Configure(ctx, "admin", adminSession, c.d, c.config, false); err == nil {
			t.Errorf("%s accepted %+v", c.d, c.config)
		}
	}
	// ntfy defaults to ntfy.sh; email picks the port for its security.
	if err := s.Configure(ctx, "admin", adminSession, Ntfy, Config{Enabled: true, Topic: "alerts", Token: "tk_1"}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Configure(ctx, "admin", adminSession, Email, Config{Enabled: true, Host: "smtp.example.com", Security: "tls",
		Username: "ludock", Password: "pw", From: "Ludock <ludock@example.com>", To: "a@example.com, b@example.com"}, false); err != nil {
		t.Fatal(err)
	}
	ntfy, email := s.Configuration(ctx, Ntfy), s.Configuration(ctx, Email)
	if !ntfy.Ready() || ntfy.Config.URL != DefaultNtfyServer || ntfy.Config.Topic != "" || !ntfy.Saved["topic"] || !ntfy.Saved["token"] {
		t.Fatalf("%+v", ntfy)
	}
	if !email.Ready() || email.Config.Port != 465 || email.Config.Password != "" || !email.Saved["password"] {
		t.Fatalf("%+v", email)
	}
	// Blank secrets keep the saved ones; clearToken drops only the token.
	if err := s.Configure(ctx, "admin", adminSession, Ntfy, Config{Enabled: true}, true); err != nil {
		t.Fatal(err)
	}
	if saved := s.settings(ctx)[Ntfy]; saved.Topic != "alerts" || saved.Token != "" {
		t.Fatalf("%+v", saved)
	}
	// Without a username the password is dropped.
	if err := s.Configure(ctx, "admin", adminSession, Email, Config{Enabled: true, Host: "smtp.example.com", Security: "tls",
		From: "ludock@example.com", To: "a@example.com"}, false); err != nil {
		t.Fatal(err)
	}
	if saved := s.settings(ctx)[Email]; saved.Password != "" || saved.Username != "" {
		t.Fatalf("%+v", saved)
	}
}

// Discord and Telegram live at fixed hosts a journey cannot reach, so their
// requests are checked here, along with every HTTP destination's secrets.
func TestHTTPDestinationRequests(t *testing.T) {
	telegram := "123456:" + strings.Repeat("T", 35)
	for _, c := range []struct {
		d       Destination
		config  Config
		url     string
		headers map[string]string
		body    map[string]any
	}{
		{Discord, Config{URL: webhook}, webhook + "?wait=true", nil,
			map[string]any{"content": "Survival is unavailable.", "allowed_mentions": map[string]any{"parse": []any{}}}},
		{Telegram, Config{Token: telegram, ChatID: "-100123"}, "https://api.telegram.org/bot" + telegram + "/sendMessage", nil,
			map[string]any{"chat_id": "-100123", "text": "Survival is unavailable.", "link_preview_options": map[string]any{"is_disabled": true}}},
		{Ntfy, Config{URL: "https://ntfy.example/base", Topic: "alerts", Token: "tk_secret"}, "https://ntfy.example/base",
			map[string]string{"Authorization": "Bearer tk_secret"},
			map[string]any{"topic": "alerts", "title": "Ludock", "message": "Survival is unavailable."}},
		{Gotify, Config{URL: "http://gotify.lan:8080", Token: "Asecret"}, "http://gotify.lan:8080/message",
			map[string]string{"X-Gotify-Key": "Asecret"},
			map[string]any{"title": "Ludock", "message": "Survival is unavailable.", "priority": float64(5)}},
		{Webhook, Config{URL: "https://hooks.example/in?token=x", Token: "Basic abc"}, "https://hooks.example/in?token=x",
			map[string]string{"Authorization": "Basic abc"},
			map[string]any{"source": "ludock", "id": "d1", "kind": "event", "message": "Survival is unavailable.", "createdAt": "2026-01-02T03:04:05Z"}},
	} {
		var got *http.Request
		var body map[string]any
		s := &Service{DB: adminDB(t), Client: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			got = r
			json.NewDecoder(r.Body).Decode(&body)
			return respond(200, "{}")
		})}}
		c.config.Enabled = true
		if err := s.Configure(ctx, "admin", adminSession, c.d, c.config, false); err != nil {
			t.Fatal(c.d, err)
		}
		if _, err := s.DB.Exec(`INSERT INTO notification_deliveries(id,event_key,destination,message,next_attempt_at,created_at)
			VALUES('d1','k',?,'Survival is unavailable.',0,1767323045000)`, string(c.d)); err != nil {
			t.Fatal(err)
		}
		if err := s.Deliver(ctx); err != nil {
			t.Fatal(err)
		}
		if got == nil || got.Method != http.MethodPost || got.URL.String() != c.url || got.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("%s: %v", c.d, got)
		}
		for name, value := range c.headers {
			if got.Header.Get(name) != value {
				t.Errorf("%s: %s = %q", c.d, name, got.Header.Get(name))
			}
		}
		if !reflect.DeepEqual(body, c.body) {
			t.Errorf("%s: body %v", c.d, body)
		}
		if deliveries, _ := s.Deliveries(ctx); deliveries[0].State != "delivered" {
			t.Errorf("%s: %+v", c.d, deliveries[0])
		}
	}
}

// Deliveries waiting on a destination that is off do not hold back
// deliveries to one that is on.
func TestPausedDestinationDoesNotBlockOthers(t *testing.T) {
	calls := 0
	s := setup(t, func(*http.Request) (*http.Response, error) {
		calls++
		return respond(204, "")
	})
	if err := s.Configure(ctx, "admin", adminSession, Gotify, Config{Enabled: true, URL: "http://gotify.lan", Token: "A1"}, false); err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		s.Notify("event-"+strconv.Itoa(i), "Survival is unavailable.")
	}
	setEnabled(t, s, false)
	s.Notify("late", "Creative is unavailable.")
	if err := s.Deliver(ctx); err != nil {
		t.Fatal(err)
	}
	deliveries, _ := s.Deliveries(ctx)
	paused, gotify := 0, 0
	for _, delivery := range deliveries {
		switch {
		case delivery.Destination == Discord && delivery.Paused && delivery.State == "queued":
			paused++
		case delivery.Destination == Gotify && delivery.State == "delivered":
			gotify++
		}
	}
	// The first pass sends ten; the late event waits behind the earlier ones.
	if paused != 12 || gotify != 10 || calls != 10 {
		t.Fatalf("paused=%d gotify=%d calls=%d", paused, gotify, calls)
	}
}

func TestBoundedRetriesKeepSecretsOut(t *testing.T) {
	calls := 0
	s := setup(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.RawQuery != "wait=true" {
			t.Fatal(r.URL)
		}
		return nil, errors.New("dial " + webhook + ": refused")
	})
	s.Notify("event-1", "Server is unavailable.")
	// Another destination that is off queues nothing.
	s.Configure(ctx, "admin", adminSession, Ntfy, Config{Topic: "alerts"}, false)
	s.Notify("event-1", "duplicate key ignored")
	for range 6 {
		s.DB.Exec("UPDATE notification_deliveries SET next_attempt_at=0")
		s.Deliver(ctx)
	}
	deliveries, _ := s.Deliveries(ctx)
	if len(deliveries) != 1 || deliveries[0].State != "failed" || deliveries[0].Attempts != 5 || calls != 5 ||
		deliveries[0].LastFailure != "Unable to reach Discord. Check network access and try again." || !deliveries[0].Retryable {
		t.Fatalf("%+v calls=%d", deliveries, calls)
	}
	var stored string
	s.DB.QueryRow("SELECT message || COALESCE(failure_code,'') FROM notification_deliveries").Scan(&stored)
	if strings.Contains(stored, "secret-token") {
		t.Fatal("webhook secret stored")
	}
	retried, err := s.Retry(ctx, "admin", adminSession, deliveries[0].ID)
	if err != nil || retried.State != "queued" || retried.Retryable {
		t.Fatal(retried, err)
	}
	if _, err := s.Retry(ctx, "admin", adminSession, deliveries[0].ID); !apperr.Is(err, "NOTIFICATION_NOT_RETRYABLE") {
		t.Fatal(err)
	}
}

func TestUnsavedResultIsNotResentEveryTick(t *testing.T) {
	calls := 0
	s := setup(t, func(*http.Request) (*http.Response, error) {
		calls++
		return respond(204, "")
	})
	if _, err := s.QueueTest(ctx, "admin", adminSession, Discord); err != nil {
		t.Fatal(err)
	}
	// Recording the delivered state fails, as with a full or broken disk.
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_result BEFORE UPDATE OF state ON notification_deliveries
		BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	s.Deliver(ctx)
	s.Deliver(ctx)
	if calls != 1 {
		t.Fatalf("sent %d times before the retry delay", calls)
	}
}

func TestEventsAreQueuedAgainAfterAFailure(t *testing.T) {
	retryDelay = 10 * time.Millisecond
	t.Cleanup(func() { retryDelay = time.Second })
	s := setup(t, func(*http.Request) (*http.Response, error) { return respond(204, "") })
	t.Cleanup(s.Stop)
	if _, err := s.DB.Exec(`CREATE TRIGGER busy BEFORE INSERT ON notification_deliveries
		BEGIN SELECT RAISE(ABORT, 'database is locked'); END`); err != nil {
		t.Fatal(err)
	}
	s.Notify("outage:server:1", "Survival is unavailable.")
	time.Sleep(50 * time.Millisecond)
	s.DB.Exec("DROP TRIGGER busy")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if deliveries, _ := s.Deliveries(ctx); len(deliveries) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the event was dropped after a failed queue")
}

func TestDeliveryAndProviderDetailsStayOut(t *testing.T) {
	var mu sync.Mutex
	statuses := []int{429, 204}
	s := setup(t, func(*http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		status := statuses[0]
		statuses = statuses[1:]
		return respond(status, `{"message":"token secret-token_value"}`)
	})
	test, err := s.QueueTest(ctx, "admin", adminSession, Discord)
	if err != nil {
		t.Fatal(err)
	}
	s.Deliver(ctx)
	deliveries, _ := s.Deliveries(ctx)
	if deliveries[0].LastFailure != "Delivery to Discord was rate limited. Wait before trying again." || deliveries[0].State != "queued" {
		t.Fatalf("%+v", deliveries[0])
	}
	s.DB.Exec("UPDATE notification_deliveries SET next_attempt_at=0")
	s.Deliver(ctx)
	deliveries, _ = s.Deliveries(ctx)
	if deliveries[0].ID != test.ID || deliveries[0].State != "delivered" || deliveries[0].DeliveredAt == 0 {
		t.Fatalf("%+v", deliveries[0])
	}
	// Disabled notifications neither queue nor send.
	setEnabled(t, s, false)
	s.Notify("event-2", "ignored")
	if _, err := s.QueueTest(ctx, "admin", adminSession, Discord); !apperr.Is(err, "NOTIFICATIONS_DISABLED") {
		t.Fatal(err)
	}
	var count int
	s.DB.QueryRow("SELECT COUNT(*) FROM notification_deliveries").Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}

}

// Revoked administrator cannot save a webhook.
func TestRevokedAdminWebhook(t *testing.T) {
	s := setup(t, func(*http.Request) (*http.Response, error) { return respond(204, "") })
	if err := s.DB.DeleteUserSessions(ctx, "admin"); err != nil {
		t.Fatal(err)
	}
	other := "https://discord.com/api/webhooks/456/departing-admin"
	if err := s.Configure(ctx, "admin", adminSession, Discord, Config{Enabled: true, URL: other}, false); !errors.Is(err, db.ErrSessionGone) {
		t.Fatal(err)
	}
	if saved := s.settings(ctx)[Discord]; saved.URL != webhook {
		t.Fatal("saved a webhook after the session was revoked")
	}
}

func TestSavingSettingsCancelsASendInFlight(t *testing.T) {
	started := make(chan struct{})
	s := setup(t, func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	if _, err := s.QueueTest(ctx, "admin", adminSession, Discord); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Deliver(ctx) }()
	<-started
	setEnabled(t, s, false)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	deliveries, _ := s.Deliveries(ctx)
	if len(deliveries) != 1 || deliveries[0].State != "queued" || deliveries[0].Attempts != 0 {
		t.Fatalf("%+v", deliveries)
	}
}

// Concurrent delivery lists do not hold the pool.
func TestConcurrentDeliveryLists(t *testing.T) {
	s := setup(t, func(*http.Request) (*http.Response, error) { return nil, errors.New("unused") })
	var wg sync.WaitGroup
	failures := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			for range 20 {
				ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_, err := s.Deliveries(ctx)
				cancel()
				if err != nil {
					failures <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal("a delivery list stalled:", err)
	}
}

// A test queued as notifications are turned off is refused.
func TestQueueTestWhileDisabling(t *testing.T) {
	s := setup(t, func(*http.Request) (*http.Response, error) { return nil, errors.New("unused") })
	// Another administrator turns notifications off after the check.
	testHook = func(string) { setEnabled(t, s, false) }
	t.Cleanup(func() { testHook = nil })
	if _, err := s.QueueTest(ctx, "admin", adminSession, Discord); !apperr.Is(err, "NOTIFICATIONS_DISABLED") {
		t.Fatal(err)
	}
	if deliveries, _ := s.Deliveries(ctx); len(deliveries) != 0 {
		t.Fatal("queued a test to send once notifications are re-enabled")
	}
}
