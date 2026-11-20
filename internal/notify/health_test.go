package notify

import (
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/Crunging/Ludock/internal/db"
)

// One alert that fails once or twice and is still retrying isn't failing
// yet; its third failure, or any failed test, is. Failures before the last
// delivery don't count, and a failing destination says when its failures
// began, never before the last delivery, and why.
func TestHealthFailingRule(t *testing.T) {
	t.Parallel()
	event := func(attempts int, created int64) failure {
		return failure{kind: "event", attempts: attempts, createdAt: created, code: "network"}
	}
	for _, c := range []struct {
		name          string
		lastDelivered int64
		failures      []failure
		want          Health
	}{
		{"nothing yet", 0, nil, Health{}},
		{"delivered", 50, nil, Health{Delivered: true}},
		{"one slow retry", 50, []failure{event(2, 100)},
			Health{Delivered: true, Failed: true, Since: 100, Reason: "Unable to reach ntfy. Check network access and try again."}},
		{"third failure", 0, []failure{event(3, 100)},
			Health{Failing: true, Failed: true, Since: 100, Reason: "Unable to reach ntfy. Check network access and try again."}},
		{"failed test", 0, []failure{{kind: "test", attempts: 1, createdAt: 200, code: "unauthorized"}},
			Health{Failing: true, Failed: true, Since: 200, Reason: "Delivery to ntfy was refused. Replace the saved credentials."}},
		{"many alerts failing once each", 0, []failure{event(1, 300), event(1, 200), event(1, 100)},
			Health{Failed: true, Since: 100, Reason: "Unable to reach ntfy. Check network access and try again."}},
		{"since the last delivery", 150, []failure{event(1, 400), event(5, 100)},
			Health{Failing: true, Failed: true, Delivered: true, Since: 150, Reason: "Unable to reach ntfy. Check network access and try again."}},
		{"unknown failure", 0, []failure{{kind: "test", attempts: 1, createdAt: 10, code: "gone"}},
			Health{Failing: true, Failed: true, Since: 10, Reason: "Failure details are unavailable."}},
	} {
		if got := health(Ntfy, c.lastDelivered, c.failures); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// A destination that stops answering is failing after an alert's third
// attempt, and back to working once a delivery gets through; turning it off
// takes it out of Problems. A failed test is final and not sent again.
func TestHealthFailsAndRecovers(t *testing.T) {
	up := false
	s := setup(t, func(*http.Request) (*http.Response, error) {
		if up {
			return respond(204, "")
		}
		return nil, errors.New("connection refused")
	})
	retry := func() {
		s.DB.Exec("UPDATE notification_deliveries SET next_attempt_at=0")
		s.Deliver(ctx)
	}
	problems := func() []Problem {
		t.Helper()
		p, err := s.Problems(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	s.Notify(ServerProblems, "outage:1", "valheim: the server is down.")
	s.Deliver(ctx)
	retry()
	if len(problems()) != 0 {
		t.Fatal("failing after one retry")
	}
	retry()
	if p := problems(); len(p) != 1 || p[0].Destination != Discord || p[0].Reason != "Unable to reach Discord. Check network access and try again." {
		t.Fatalf("%+v", p)
	}
	setEnabled(t, s, false)
	if len(problems()) != 0 {
		t.Fatal("a destination that is off is failing")
	}
	setEnabled(t, s, true)
	up = true
	retry()
	if h, _ := s.Health(ctx); len(problems()) != 0 || !h[Discord].Delivered || h[Discord].Failed {
		t.Fatalf("still failing after a delivery: %+v", h)
	}

	up = false
	test, err := s.QueueTest(ctx, "admin", adminSession, Discord)
	if err != nil {
		t.Fatal(err)
	}
	s.Deliver(ctx)
	if p := problems(); len(p) != 1 {
		t.Fatal("a failed test doesn't fail its destination")
	}
	retry()
	if got, _ := s.Test(ctx, Discord, test.ID); got.State != "failed" || got.Attempts != 1 {
		t.Fatalf("%+v", got)
	}
}

// Old deliveries are removed, except each destination's newest, which shows
// the destination once worked.
func TestCleanupKeepsNewestDelivery(t *testing.T) {
	s := setup(t, func(*http.Request) (*http.Response, error) { return respond(204, "") })
	s.Notify(ServerProblems, "a", "first")
	s.Notify(ServerProblems, "b", "second")
	s.Deliver(ctx)
	// A destination that never delivered doesn't stop the others' cleanup.
	s.DB.Exec(`INSERT INTO notification_deliveries (id,event_key,destination,message,next_attempt_at,created_at,state,failure_code)
		VALUES ('w','w','webhook','never',0,1,'failed','network')`)
	s.DB.Exec("UPDATE notification_deliveries SET delivered_at=1, created_at=1 WHERE message='first'")
	s.DB.Exec("UPDATE notification_deliveries SET delivered_at=2, created_at=2 WHERE message='second'")
	s.Deliver(ctx)
	var messages []string
	rows, _ := s.DB.Query("SELECT message FROM notification_deliveries")
	for rows.Next() {
		var m string
		rows.Scan(&m)
		messages = append(messages, m)
	}
	rows.Close()
	if len(messages) != 2 || !slices.Contains(messages, "second") || !slices.Contains(messages, "never") {
		t.Fatal(messages)
	}
	if h, _ := s.Health(ctx); !h[Discord].Delivered {
		t.Fatal("forgot the destination delivered")
	}
}

// While an alert's retry is being sent, its destination's health stays what
// the last outcomes say: an old failure retried after a later delivery isn't
// failing, and a destination that is still failing doesn't look fixed.
func TestHealthWhileRetrySends(t *testing.T) {
	var sending chan struct{}
	release := make(chan struct{})
	s := setup(t, func(*http.Request) (*http.Response, error) {
		if sending != nil {
			close(sending)
			<-release
		}
		return respond(204, "")
	})
	failingDuringRetry := func() bool {
		t.Helper()
		sending, release = make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() { done <- s.Deliver(ctx) }()
		<-sending
		p, err := s.Problems(ctx)
		close(release)
		if sent := <-done; err != nil || sent != nil {
			t.Fatal(err, sent)
		}
		sending = nil
		return len(p) > 0
	}
	old := `INSERT INTO notification_deliveries (id,event_key,destination,message,next_attempt_at,created_at,state,attempts,
		retry_attempts,last_attempt_at,failure_code) VALUES (?,?,'discord','valheim: the server is down.',0,1,'queued',3,3,?,'network')`
	// Still failing: nothing has been delivered.
	s.DB.Exec(old, "a", "a", 2)
	if !failingDuringRetry() {
		t.Fatal("a failing destination looked fixed while a retry was sent")
	}
	// The retry delivered; an older failure retried after it isn't failing.
	s.DB.Exec(old, "b", "b", 3)
	if failingDuringRetry() {
		t.Fatal("an old failure being retried made the destination failing")
	}
}

// An alert that keeps failing and a later one that gets through within the
// same millisecond leave the destination working: the delivery came last.
func TestHealthKeepsOrderWithinAMillisecond(t *testing.T) {
	up := false
	s := setup(t, func(*http.Request) (*http.Response, error) {
		if up {
			return respond(204, "")
		}
		return nil, errors.New("connection refused")
	})
	clock := db.Now
	frozen := clock()
	db.Now = func() int64 { return frozen }
	t.Cleanup(func() { db.Now = clock })
	s.Notify(ServerProblems, "outage:1", "valheim: the server is down.")
	for range failingAttempts {
		s.DB.Exec("UPDATE notification_deliveries SET next_attempt_at=0")
		s.Deliver(ctx)
	}
	if p, err := s.Problems(ctx); err != nil || len(p) != 1 {
		t.Fatal(p, err)
	}
	up = true
	s.Notify(ServerProblems, "outage:2", "valheim: the server is down.")
	s.Deliver(ctx)
	if h, err := s.Health(ctx); err != nil || !h[Discord].Delivered || h[Discord].Failing {
		t.Fatalf("a later delivery in the same millisecond left it failing: %+v %v", h, err)
	}
}
