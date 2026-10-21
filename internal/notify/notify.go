// Package notify queues and delivers Discord webhook notifications. The
// webhook URL is write-only: it never appears in output, logs, or stored
// failure details.
package notify

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/logging"
)

var log = logging.New("notifications")

const settingKey = "notifications"

type settings struct {
	Enabled    bool   `json:"enabled"`
	WebhookURL string `json:"webhookUrl"`
}

// Configuration is the public notification state.
type Configuration struct {
	Enabled    bool
	Configured bool
}

var failureMessages = map[string]string{
	"invalid_request": "Discord rejected the notification. Check the webhook configuration.",
	"unauthorized":    "Discord refused webhook access. Replace the saved webhook URL.",
	"not_found":       "Discord could not find the webhook. It may have been deleted; replace the saved URL.",
	"rate_limited":    "Discord rate limited delivery. Wait before trying again.",
	"unavailable":     "Discord is temporarily unavailable. Try again later.",
	"rejected":        "Discord rejected delivery. Check the webhook configuration and try again.",
	"timeout":         "Discord did not respond in time. Check network access and try again.",
	"network":         "Unable to reach Discord. Check network access and try again.",
}

// Delivery is one queued or sent notification.
type Delivery struct {
	ID            string
	Kind          string // event or test
	State         string // queued, delivered, failed
	Attempts      int
	CreatedAt     int64
	LastAttemptAt int64 // 0 when never
	DeliveredAt   int64
	NextAttemptAt int64 // only while queued
	LastFailure   string
	Retryable     bool
}

// Service delivers notifications.
type Service struct {
	DB *db.DB
	// Client sends webhook requests; nil uses a default client.
	Client *http.Client

	mu         sync.Mutex
	active     string
	delivering bool
	// retries are event notifications waiting to be queued again.
	retries sync.WaitGroup
	quit    chan struct{}
	stopped bool
}

// retryDelay is the first wait before queueing an event again; tests
// shorten it.
var retryDelay = time.Second

func (s *Service) load(ctx context.Context) (settings, error) {
	var value settings
	_, err := s.DB.Setting(ctx, settingKey, &value)
	return value, err
}

// settings reads the saved settings; an unreadable value counts as off,
// which only ever withholds a send.
func (s *Service) settings(ctx context.Context) settings {
	value, _ := s.load(ctx)
	return value
}

// Configuration reports whether notifications are on and configured.
func (s *Service) Configuration(ctx context.Context) Configuration {
	value := s.settings(ctx)
	return Configuration{Enabled: value.Enabled, Configured: value.WebhookURL != ""}
}

var webhookPath = regexp.MustCompile(`^/api/webhooks/\d+/[A-Za-z0-9_-]+$`)

// Configure saves settings. An empty webhookURL keeps the saved one.
// The administrator's session and role are checked again in the transaction
// that saves them.
func (s *Service) Configure(ctx context.Context, adminID, tokenHash string, enabled bool, webhookURL *string) error {
	return s.DB.AdminMutation(ctx, adminID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		value := ""
		if webhookURL != nil {
			value = *webhookURL
		} else {
			var saved settings
			var raw string
			err := tx.QueryRowContext(ctx, "SELECT value_json FROM settings WHERE key=?", settingKey).Scan(&raw)
			if err == nil {
				err = json.Unmarshal([]byte(raw), &saved)
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			value = saved.WebhookURL
		}
		if err := validWebhook(enabled, value); err != nil {
			return err
		}
		return s.DB.SetSetting(ctx, tx, settingKey, settings{Enabled: enabled, WebhookURL: value})
	})
}

func validWebhook(enabled bool, value string) error {
	if len(value) > 1024 {
		return apperr.New("INVALID_WEBHOOK", 400, "Enter a valid Discord webhook URL")
	}
	if value != "" {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" {
			return apperr.New("INVALID_WEBHOOK", 400, "Enter a valid Discord webhook URL")
		}
		if parsed.Scheme != "https" || parsed.Hostname() != "discord.com" || parsed.Port() != "" || parsed.User != nil ||
			parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(value, "#") ||
			!webhookPath.MatchString(parsed.EscapedPath()) {
			return apperr.New("INVALID_WEBHOOK", 400, "Use a Discord HTTPS webhook URL")
		}
	}
	if enabled && value == "" {
		return apperr.New("WEBHOOK_REQUIRED", 400, "Configure a webhook before enabling notifications")
	}
	return nil
}

type row struct {
	id, payload, kind, state string
	attempts, retries        int
	createdAt, next          int64
	lastAttempt, delivered   sql.NullInt64
	failure                  sql.NullString
}

const columns = "id,payload_json,kind,state,attempts,retry_attempts,created_at,next_attempt_at,last_attempt_at,delivered_at,failure_code"

func scan(s interface{ Scan(...any) error }) (*row, error) {
	var r row
	return &r, s.Scan(&r.id, &r.payload, &r.kind, &r.state, &r.attempts, &r.retries, &r.createdAt, &r.next, &r.lastAttempt, &r.delivered, &r.failure)
}

func (s *Service) public(r *row, enabled bool) Delivery {
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	delivery := Delivery{ID: r.id, Kind: r.kind, State: r.state, Attempts: r.attempts, CreatedAt: r.createdAt,
		LastAttemptAt: r.lastAttempt.Int64, DeliveredAt: r.delivered.Int64}
	if r.state == "queued" {
		delivery.NextAttemptAt = r.next
	}
	if message, ok := failureMessages[r.failure.String]; ok && r.failure.Valid {
		delivery.LastFailure = message
	} else if r.state != "delivered" && r.attempts > 0 {
		delivery.LastFailure = "Failure details are unavailable for this delivery."
	}
	delivery.Retryable = enabled && active != r.id && (r.state == "failed" || (r.state == "queued" && r.retries > 0))
	return delivery
}

func (s *Service) row(ctx context.Context, id string) (*row, error) {
	r, err := scan(s.DB.QueryRowContext(ctx, "SELECT "+columns+" FROM notification_deliveries WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New("NOTIFICATION_NOT_FOUND", 404, "Notification delivery not found")
	}
	return r, err
}

// Deliveries lists the 50 newest deliveries.
func (s *Service) Deliveries(ctx context.Context) ([]Delivery, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+columns+" FROM notification_deliveries ORDER BY created_at DESC, id DESC LIMIT 50")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	enabled := s.Configuration(ctx).Enabled
	var result []Delivery
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, s.public(r, enabled))
	}
	return result, rows.Err()
}

func truncate(message string, limit int) string {
	runes := []rune(message)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

func (s *Service) enqueue(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, key, message, kind string) (string, error) {
	id := identity.NewUUID()
	now := db.Now()
	payload, _ := json.Marshal(map[string]any{"content": truncate(message, 1800), "allowed_mentions": map[string]any{"parse": []string{}}})
	_, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO notification_deliveries
    (id,event_key,payload_json,next_attempt_at,created_at,kind) VALUES(?,?,?,?,?,?)`, id, key, string(payload), now, now, kind)
	return id, err
}

// Notify queues an event once per key, when notifications are enabled.
// Callers fire and forget, and most events never recur, so a failed queue
// is retried in the background instead of dropping the alert.
func (s *Service) Notify(key, message string) {
	if s.queueEvent(key, message) == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		log.Error("Could not queue a notification; it was dropped")
		return
	}
	if s.quit == nil {
		s.quit = make(chan struct{})
	}
	quit := s.quit
	s.retries.Add(1)
	s.mu.Unlock()
	log.Warn("Could not queue a notification; retrying")
	go func() {
		defer s.retries.Done()
		delay := retryDelay
		for range 8 {
			select {
			case <-time.After(delay):
			case <-quit:
				log.Error("Could not queue a notification before shutdown; it was dropped")
				return
			}
			if s.queueEvent(key, message) == nil {
				return
			}
			delay = min(delay*2, time.Minute)
		}
		log.Error("Could not queue a notification; it was dropped")
	}()
}

func (s *Service) queueEvent(key, message string) error {
	ctx := context.Background()
	value, err := s.load(ctx)
	if err != nil || !value.Enabled {
		return err
	}
	_, err = s.enqueue(ctx, s.DB, key, message, "event")
	return err
}

// Stop abandons pending queue retries, before the database closes.
func (s *Service) Stop() {
	s.mu.Lock()
	s.stopped = true
	if s.quit != nil {
		close(s.quit)
	}
	s.mu.Unlock()
	s.retries.Wait()
}

func (s *Service) assertEnabled(ctx context.Context) error {
	configuration := s.Configuration(ctx)
	if !configuration.Enabled || !configuration.Configured {
		return apperr.New("NOTIFICATIONS_DISABLED", 400, "Save an enabled Discord webhook before sending or retrying a notification")
	}
	return nil
}

// QueueTest queues a test message, checking the administrator again in the
// transaction that queues it.
func (s *Service) QueueTest(ctx context.Context, adminID, tokenHash string) (*Delivery, error) {
	if err := s.assertEnabled(ctx); err != nil {
		return nil, err
	}
	var id string
	err := s.DB.AdminMutation(ctx, adminID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		var err error
		id, err = s.enqueue(ctx, tx, "test:"+identity.NewUUID(), "Ludock test notification: Discord delivery is working.", "test")
		return err
	})
	if err != nil {
		return nil, err
	}
	r, err := s.row(ctx, id)
	if err != nil {
		return nil, err
	}
	delivery := s.public(r, true)
	return &delivery, nil
}

// Retry grants a failed delivery a fresh bounded retry cycle, keeping its
// lifetime attempts and last failure. Clearing the cycle count also rejects
// duplicate clicks.
func (s *Service) Retry(ctx context.Context, adminID, tokenHash, id string) (*Delivery, error) {
	if err := s.assertEnabled(ctx); err != nil {
		return nil, err
	}
	r, err := s.row(ctx, id)
	if err != nil {
		return nil, err
	}
	if !s.public(r, true).Retryable {
		return nil, apperr.New("NOTIFICATION_NOT_RETRYABLE", 409, "This delivery is already queued, being sent, or has been delivered")
	}
	// Only the row as read is retried: a send claimed or finished since then
	// changes last_attempt_at or the counters, and must not be queued again.
	changed := int64(0)
	err = s.DB.AdminMutation(ctx, adminID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET state='queued',retry_attempts=0,next_attempt_at=?
      WHERE id=? AND state=? AND attempts=? AND retry_attempts=? AND last_attempt_at IS ?`,
			db.Now(), id, r.state, r.attempts, r.retries, r.lastAttempt)
		if err == nil {
			changed, err = result.RowsAffected()
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if changed != 1 {
		return nil, apperr.New("NOTIFICATION_NOT_RETRYABLE", 409, "This delivery is already queued, being sent, or has been delivered")
	}
	if r, err = s.row(ctx, id); err != nil {
		return nil, err
	}
	delivery := s.public(r, true)
	return &delivery, nil
}

func responseFailure(status int) string {
	switch {
	case status == 400:
		return "invalid_request"
	case status == 401 || status == 403:
		return "unauthorized"
	case status == 404:
		return "not_found"
	case status == 429:
		return "rate_limited"
	case status >= 500:
		return "unavailable"
	}
	return "rejected"
}

// Deliver sends up to ten due notifications. Overlapping calls return
// immediately.
func (s *Service) Deliver(ctx context.Context) error {
	s.mu.Lock()
	if s.delivering {
		s.mu.Unlock()
		return nil
	}
	s.delivering = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.delivering = false
		s.mu.Unlock()
	}()
	if !s.Configuration(ctx).Enabled {
		return nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM notification_deliveries
    WHERE state='queued' AND next_attempt_at<=? ORDER BY created_at, rowid LIMIT 10`, db.Now())
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	client := s.Client
	if client == nil {
		client = &http.Client{}
	}
	// Redirects could forward the payload elsewhere.
	noRedirects := *client
	noRedirects.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }
	for _, id := range ids {
		config := s.settings(ctx)
		if !config.Enabled {
			break
		}
		// An administrator may retry another row while a send is waiting; read
		// the current counters right before sending, not from the batch.
		r, err := s.row(ctx, id)
		if err != nil || r.state != "queued" || r.next > db.Now() {
			continue
		}
		attempts, retries := r.attempts+1, r.retries+1
		backoff := min(int64(3600_000), int64(30_000)<<min(retries, 20))
		// Marked active first, so Retry refuses the row from here on, and
		// claimed only as read: a retry that already reset it wins, and the
		// row is sent with fresh counters on a later pass. If the result
		// cannot be saved, the message is sent again after the backoff, not
		// on every tick.
		s.mu.Lock()
		s.active = id
		s.mu.Unlock()
		result, err := s.DB.ExecContext(ctx, `UPDATE notification_deliveries SET last_attempt_at=?, next_attempt_at=?
      WHERE id=? AND state='queued' AND attempts=? AND retry_attempts=? AND last_attempt_at IS ?`,
			db.Now(), db.Now()+backoff, id, r.attempts, r.retries, r.lastAttempt)
		claimed := int64(0)
		if err == nil {
			claimed, err = result.RowsAffected()
		}
		if err != nil || claimed != 1 {
			if err != nil {
				log.Warn("Could not claim a notification delivery; it will be retried")
			}
			s.mu.Lock()
			s.active = ""
			s.mu.Unlock()
			continue
		}
		failure := s.send(ctx, &noRedirects, config.WebhookURL, r.payload)
		finished := db.Now()
		state := "queued"
		var delivered, failureCode any
		if failure == "" {
			state, delivered = "delivered", finished
		} else {
			failureCode = failure
			if retries >= 5 {
				state = "failed"
			}
		}
		for try := 1; ; try++ {
			_, err := s.DB.ExecContext(ctx, `UPDATE notification_deliveries
      SET attempts=?,retry_attempts=?,state=?,next_attempt_at=?,delivered_at=?,failure_code=? WHERE id=?`,
				attempts, retries, state, finished+backoff, delivered, failureCode, id)
			if err == nil {
				break
			}
			if try == 3 || ctx.Err() != nil {
				log.Warn("Could not record a notification delivery; it may be sent again after its retry delay")
				break
			}
			time.Sleep(time.Duration(try) * 500 * time.Millisecond)
		}
		s.mu.Lock()
		s.active = ""
		s.mu.Unlock()
	}
	_, err = s.DB.ExecContext(ctx, "DELETE FROM notification_deliveries WHERE state='delivered' AND COALESCE(delivered_at,created_at)<?",
		db.Now()-30*86400_000)
	return err
}

// send posts one payload. Neither the response body nor transport errors are
// kept: any of them could include the webhook secret.
func (s *Service) send(ctx context.Context, client *http.Client, webhook, payload string) string {
	timeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Discord confirms it saved the message only with wait=true. Saved URLs
	// reject query strings, so this option is controlled here.
	request, err := http.NewRequestWithContext(timeout, http.MethodPost, webhook+"?wait=true", bytes.NewReader([]byte(payload)))
	if err != nil {
		return "network"
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		if timeout.Err() != nil {
			return "timeout"
		}
		return "network"
	}
	response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return ""
	}
	return responseFailure(response.StatusCode)
}
