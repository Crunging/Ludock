package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
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
	url := webhook
	if err := s.Configure(ctx, "admin", adminSession, true, &url); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWebhookValidation(t *testing.T) {
	s := &Service{DB: adminDB(t)}
	for _, url := range []string{"http://discord.com/api/webhooks/1/a", "https://evil.example/api/webhooks/1/a",
		"https://discord.com:8443/api/webhooks/1/a", "https://user@discord.com/api/webhooks/1/a",
		"https://discord.com/api/webhooks/1/a?x=1", "https://discord.com/api/webhooks/1/a#x", "https://discord.com/other", "not a url"} {
		if err := s.Configure(ctx, "admin", adminSession, true, &url); err == nil {
			t.Errorf("accepted %s", url)
		}
	}
	if err := s.Configure(ctx, "admin", adminSession, true, nil); !apperr.Is(err, "WEBHOOK_REQUIRED") {
		t.Fatal(err)
	}
	url := webhook
	s.Configure(ctx, "admin", adminSession, false, &url)
	// Saving without a URL keeps the saved one.
	if err := s.Configure(ctx, "admin", adminSession, true, nil); err != nil || !s.Configuration(ctx).Configured {
		t.Fatal(err)
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
	s.Notify("event-1", "duplicate key ignored")
	for range 6 {
		s.DB.Exec("UPDATE notification_deliveries SET next_attempt_at=0")
		s.Deliver(ctx)
	}
	deliveries, _ := s.Deliveries(ctx)
	if len(deliveries) != 1 || deliveries[0].State != "failed" || deliveries[0].Attempts != 5 || calls != 5 ||
		deliveries[0].LastFailure != failureMessages["network"] || !deliveries[0].Retryable {
		t.Fatalf("%+v calls=%d", deliveries, calls)
	}
	var stored string
	s.DB.QueryRow("SELECT payload_json || COALESCE(failure_code,'') FROM notification_deliveries").Scan(&stored)
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
	if _, err := s.QueueTest(ctx, "admin", adminSession); err != nil {
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
	test, err := s.QueueTest(ctx, "admin", adminSession)
	if err != nil {
		t.Fatal(err)
	}
	s.Deliver(ctx)
	deliveries, _ := s.Deliveries(ctx)
	if deliveries[0].LastFailure != failureMessages["rate_limited"] || deliveries[0].State != "queued" {
		t.Fatalf("%+v", deliveries[0])
	}
	s.DB.Exec("UPDATE notification_deliveries SET next_attempt_at=0")
	s.Deliver(ctx)
	deliveries, _ = s.Deliveries(ctx)
	if deliveries[0].ID != test.ID || deliveries[0].State != "delivered" || deliveries[0].DeliveredAt == 0 {
		t.Fatalf("%+v", deliveries[0])
	}
	// Disabled notifications neither queue nor send.
	s.Configure(ctx, "admin", adminSession, false, nil)
	s.Notify("event-2", "ignored")
	if _, err := s.QueueTest(ctx, "admin", adminSession); !apperr.Is(err, "NOTIFICATIONS_DISABLED") {
		t.Fatal(err)
	}
	var count int
	s.DB.QueryRow("SELECT COUNT(*) FROM notification_deliveries").Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}

}

func TestRevokedAdministratorCannotSaveAWebhook(t *testing.T) {
	s := setup(t, func(*http.Request) (*http.Response, error) { return respond(204, "") })
	if err := s.DB.DeleteUserSessions(ctx, "admin"); err != nil {
		t.Fatal(err)
	}
	other := "https://discord.com/api/webhooks/456/departing-admin"
	if err := s.Configure(ctx, "admin", adminSession, true, &other); !errors.Is(err, db.ErrSessionGone) {
		t.Fatal(err)
	}
	if saved := s.settings(ctx); saved.WebhookURL != webhook {
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
	if _, err := s.QueueTest(ctx, "admin", adminSession); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Deliver(ctx) }()
	<-started
	if err := s.Configure(ctx, "admin", adminSession, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	deliveries, _ := s.Deliveries(ctx)
	if len(deliveries) != 1 || deliveries[0].State != "queued" || deliveries[0].Attempts != 0 {
		t.Fatalf("%+v", deliveries)
	}
}

func TestConcurrentDeliveryListsDoNotHoldThePool(t *testing.T) {
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
