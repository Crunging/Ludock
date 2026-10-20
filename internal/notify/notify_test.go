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
	"github.com/Crunging/Ludock/internal/db/dbtest"
)

var ctx = context.Background()

const webhook = "https://discord.com/api/webhooks/123/secret-token_value"

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func respond(status int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func setup(t *testing.T, transport roundTrip) *Service {
	t.Helper()
	s := &Service{DB: dbtest.Open(t), Client: &http.Client{Transport: transport}}
	url := webhook
	if err := s.Configure(ctx, true, &url); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWebhookValidation(t *testing.T) {
	s := &Service{DB: dbtest.Open(t)}
	for _, url := range []string{"http://discord.com/api/webhooks/1/a", "https://evil.example/api/webhooks/1/a",
		"https://discord.com:8443/api/webhooks/1/a", "https://user@discord.com/api/webhooks/1/a",
		"https://discord.com/api/webhooks/1/a?x=1", "https://discord.com/api/webhooks/1/a#x", "https://discord.com/other", "not a url"} {
		if err := s.Configure(ctx, true, &url); err == nil {
			t.Errorf("accepted %s", url)
		}
	}
	if err := s.Configure(ctx, true, nil); !apperr.Is(err, "WEBHOOK_REQUIRED") {
		t.Fatal(err)
	}
	url := webhook
	s.Configure(ctx, false, &url)
	// Saving without a URL keeps the saved one.
	if err := s.Configure(ctx, true, nil); err != nil || !s.Configuration(ctx).Configured {
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
	retried, err := s.Retry(ctx, deliveries[0].ID)
	if err != nil || retried.State != "queued" || retried.Retryable {
		t.Fatal(retried, err)
	}
	if _, err := s.Retry(ctx, deliveries[0].ID); !apperr.Is(err, "NOTIFICATION_NOT_RETRYABLE") {
		t.Fatal(err)
	}
}

func TestUnsavedResultIsNotResentEveryTick(t *testing.T) {
	calls := 0
	s := setup(t, func(*http.Request) (*http.Response, error) {
		calls++
		return respond(204, "")
	})
	if _, err := s.QueueTest(ctx); err != nil {
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
	test, err := s.QueueTest(ctx)
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
	s.Configure(ctx, false, nil)
	s.Notify("event-2", "ignored")
	if _, err := s.QueueTest(ctx); !apperr.Is(err, "NOTIFICATIONS_DISABLED") {
		t.Fatal(err)
	}
	var count int
	s.DB.QueryRow("SELECT COUNT(*) FROM notification_deliveries").Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}

}
