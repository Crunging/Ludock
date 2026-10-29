// Package notify queues and delivers alerts to Discord, ntfy, Gotify,
// Telegram, email, and generic webhooks. Webhook URLs, tokens, topics, and
// passwords are write-only: they never appear in output, logs, or stored
// failure details.
package notify

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

type settings map[Destination]Config

// ready lists the destinations that are on and configured.
func (all settings) ready() []Destination {
	var ready []Destination
	for _, d := range Destinations {
		if d.ready(all[d]) {
			ready = append(ready, d)
		}
	}
	return ready
}

// Configuration is one destination's public state: its settings without
// secrets, and which secrets are saved.
type Configuration struct {
	Destination Destination
	Enabled     bool
	Configured  bool
	Config      Config
	Saved       map[string]bool
}

func (c Configuration) Ready() bool { return c.Enabled && c.Configured }

func public(d Destination, c Config) Configuration {
	configured := d.configured(c)
	saved := map[string]bool{}
	for name, field := range d.secrets(&c) {
		saved[name] = *field != ""
		*field = ""
	}
	return Configuration{Destination: d, Enabled: c.Enabled, Configured: configured, Config: c, Saved: saved}
}

var failureMessages = map[string]string{
	"invalid_request": "Delivery to %s was rejected as invalid. Check the destination settings.",
	"unauthorized":    "Delivery to %s was refused. Replace the saved credentials.",
	"not_found":       "Delivery to %s failed because the destination was not found. It may have been deleted; check the saved settings.",
	"rate_limited":    "Delivery to %s was rate limited. Wait before trying again.",
	"unavailable":     "Delivery to %s failed because it is temporarily unavailable. Try again later.",
	"rejected":        "Delivery to %s was rejected. Check the destination settings and try again.",
	"timeout":         "Delivery to %s timed out. Check network access and try again.",
	"network":         "Unable to reach %s. Check network access and try again.",
	"tls":             "Could not open a secure connection to %s. Check the security setting and the server’s certificate.",
	"recipient":       "Delivery to %s failed because it refused the sender or a recipient. Check the addresses.",
}

func failureMessage(d Destination, code string) (string, bool) {
	message, ok := failureMessages[code]
	return fmt.Sprintf(message, d.name()), ok
}

type Delivery struct {
	ID            string
	Destination   Destination
	Kind          string // event or test
	State         string // queued, delivered, failed
	Attempts      int
	CreatedAt     int64
	LastAttemptAt int64 // 0 when never
	DeliveredAt   int64
	NextAttemptAt int64 // only while queued
	LastFailure   string
	// Paused is a queued delivery whose destination is off.
	Paused    bool
	Retryable bool
}

type Service struct {
	DB *db.DB
	// Client sends HTTP requests; nil uses a default client.
	Client *http.Client
	// TLS is the base TLS configuration for mail servers; nil uses the
	// system roots.
	TLS *tls.Config

	mu         sync.Mutex
	active     string
	activeTo   Destination
	delivering bool
	// cancelSend ends the request in flight when its destination's settings
	// change.
	cancelSend context.CancelCauseFunc
	// retries are event notifications waiting to be queued again.
	retries sync.WaitGroup
	quit    chan struct{}
	stopped bool
	wake    chan struct{}
}

// retryDelay is the first wait before queueing an event again; tests
// shorten it.
var retryDelay = time.Second

// Wake receives when a notification was queued and delivery should run
// soon rather than at the next tick.
func (s *Service) Wake() <-chan struct{} { return s.wakeChannel() }

func (s *Service) wakeChannel() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	return s.wake
}

func (s *Service) poke() {
	wake := s.wakeChannel()
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (s *Service) load(ctx context.Context) (settings, error) {
	value := settings{}
	_, err := s.DB.Setting(ctx, settingKey, &value)
	return value, err
}

// settings reads the saved settings; an unreadable value counts as off,
// which only ever withholds a send.
func (s *Service) settings(ctx context.Context) settings {
	value, err := s.load(ctx)
	if err != nil {
		return settings{}
	}
	return value
}

// order.
func (s *Service) Configurations(ctx context.Context) []Configuration {
	all := s.settings(ctx)
	result := make([]Configuration, len(Destinations))
	for i, d := range Destinations {
		result[i] = public(d, all[d])
	}
	return result
}

func (s *Service) Configuration(ctx context.Context, d Destination) Configuration {
	return public(d, s.settings(ctx)[d])
}

// Configure saves one destination's settings. Empty secret fields keep the
// saved values; clearToken removes an optional saved token. The
// administrator's session and role are checked again in the transaction that
// saves them.
func (s *Service) Configure(ctx context.Context, adminID, tokenHash string, d Destination, input Config, clearToken bool) error {
	if _, ok := Parse(string(d)); !ok {
		return apperr.New("NOTIFICATION_DESTINATION_NOT_FOUND", 404, "Notification destination not found")
	}
	err := s.DB.AdminMutation(ctx, adminID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		all, err := savedIn(ctx, tx)
		if err != nil {
			return err
		}
		saved := all[d]
		next := input
		previous := d.secrets(&saved)
		for name, field := range d.secrets(&next) {
			if *field == "" {
				*field = *previous[name]
			}
		}
		if clearToken {
			next.Token = ""
		}
		if next, err = d.normalize(next); err != nil {
			return err
		}
		all[d] = next
		return s.DB.SetSetting(ctx, tx, settingKey, all)
	})
	if err == nil {
		// A send in flight may target the old settings; it is released and
		// sent again under the new ones.
		s.mu.Lock()
		if s.cancelSend != nil && s.activeTo == d {
			s.cancelSend(errSettingsChanged)
		}
		s.mu.Unlock()
		s.poke()
	}
	return err
}

var errSettingsChanged = errors.New("notification settings changed")

type row struct {
	id, destination, message, kind, state string
	attempts, retries                     int
	createdAt, next                       int64
	lastAttempt, delivered                sql.NullInt64
	failure                               sql.NullString
}

const columns = "id,destination,message,kind,state,attempts,retry_attempts,created_at,next_attempt_at,last_attempt_at,delivered_at,failure_code"

func scan(s interface{ Scan(...any) error }) (*row, error) {
	var r row
	return &r, s.Scan(&r.id, &r.destination, &r.message, &r.kind, &r.state, &r.attempts, &r.retries, &r.createdAt, &r.next,
		&r.lastAttempt, &r.delivered, &r.failure)
}

func (s *Service) public(r *row, all settings) Delivery {
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	d := Destination(r.destination)
	ready := d.ready(all[d])
	delivery := Delivery{ID: r.id, Destination: d, Kind: r.kind, State: r.state, Attempts: r.attempts, CreatedAt: r.createdAt,
		LastAttemptAt: r.lastAttempt.Int64, DeliveredAt: r.delivered.Int64, Paused: r.state == "queued" && !ready}
	if r.state == "queued" {
		delivery.NextAttemptAt = r.next
	}
	if message, ok := failureMessage(d, r.failure.String); ok && r.failure.Valid {
		delivery.LastFailure = message
	} else if r.state != "delivered" && r.attempts > 0 {
		delivery.LastFailure = "Failure details are unavailable for this delivery."
	}
	delivery.Retryable = ready && active != r.id && (r.state == "failed" || (r.state == "queued" && r.retries > 0))
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
	// Read before the rows hold a connection: reading while they do needs a
	// second one, and enough concurrent lists would hold the whole pool.
	all := s.settings(ctx)
	rows, err := s.DB.QueryContext(ctx, "SELECT "+columns+" FROM notification_deliveries ORDER BY created_at DESC, id DESC LIMIT 50")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Delivery
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, s.public(r, all))
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

func enqueue(ctx context.Context, tx *sql.Tx, key string, d Destination, message, kind string) (string, error) {
	id := identity.NewUUID()
	now := db.Now()
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO notification_deliveries
    (id,event_key,destination,message,next_attempt_at,created_at,kind) VALUES(?,?,?,?,?,?,?)`,
		id, key, string(d), truncate(message, 1800), now, now, kind)
	return id, err
}

// Notify queues an event once per key for each destination that is on.
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

// queueEvent reads the settings in the transaction that queues, so an event
// raised as a destination is turned off is not left to send once it is
// turned on again.
func (s *Service) queueEvent(key, message string) error {
	ctx := context.Background()
	queued := false
	err := s.DB.Tx(ctx, func(tx *sql.Tx) error {
		all, err := savedIn(ctx, tx)
		if err != nil {
			return err
		}
		for _, d := range all.ready() {
			if _, err := enqueue(ctx, tx, key, d, message, "event"); err != nil {
				return err
			}
			queued = true
		}
		return nil
	})
	if err == nil && queued {
		s.poke()
	}
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

func errDisabled(d Destination) error {
	return apperr.New("NOTIFICATIONS_DISABLED", 400, "Save and turn on "+d.Label()+" before sending or retrying a notification")
}

func savedIn(ctx context.Context, tx *sql.Tx) (settings, error) {
	saved := settings{}
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT value_json FROM settings WHERE key=?", settingKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return saved, nil
	}
	if err == nil {
		err = json.Unmarshal([]byte(raw), &saved)
	}
	return saved, err
}

// assertReadyIn checks d in the transaction that queues, so a destination
// turned off meanwhile queues nothing to send once it is turned on again.
func assertReadyIn(ctx context.Context, tx *sql.Tx, d Destination) error {
	all, err := savedIn(ctx, tx)
	if err != nil {
		return err
	}
	if !d.ready(all[d]) {
		return errDisabled(d)
	}
	return nil
}

// QueueTest queues a test message to d, checking the administrator again in
// the transaction that queues it.
func (s *Service) QueueTest(ctx context.Context, adminID, tokenHash string, d Destination) (*Delivery, error) {
	if !s.Configuration(ctx, d).Ready() {
		return nil, errDisabled(d)
	}
	if testHook != nil {
		testHook("checked")
	}
	var id string
	err := s.DB.AdminMutation(ctx, adminID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		if err := assertReadyIn(ctx, tx, d); err != nil {
			return err
		}
		var err error
		id, err = enqueue(ctx, tx, "test:"+identity.NewUUID(), d, "Ludock test notification: "+d.Label()+" delivery is working.", "test")
		return err
	})
	if err != nil {
		return nil, err
	}
	s.poke()
	r, err := s.row(ctx, id)
	if err != nil {
		return nil, err
	}
	delivery := s.public(r, s.settings(ctx))
	return &delivery, nil
}

var errNotRetryable = apperr.New("NOTIFICATION_NOT_RETRYABLE", 409, "This delivery is already queued, being sent, or has been delivered")

// Retry grants a failed delivery a fresh bounded retry cycle, keeping its
// lifetime attempts and last failure. Clearing the cycle count also rejects
// duplicate clicks.
func (s *Service) Retry(ctx context.Context, adminID, tokenHash, id string) (*Delivery, error) {
	r, err := s.row(ctx, id)
	if err != nil {
		return nil, err
	}
	d := Destination(r.destination)
	if !s.Configuration(ctx, d).Ready() {
		return nil, errDisabled(d)
	}
	if !s.public(r, s.settings(ctx)).Retryable {
		return nil, errNotRetryable
	}
	// Only the row as read is retried: a send claimed or finished since then
	// changes last_attempt_at or the counters, and must not be queued again.
	changed := int64(0)
	err = s.DB.AdminMutation(ctx, adminID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		if err := assertReadyIn(ctx, tx, d); err != nil {
			return err
		}
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
		return nil, errNotRetryable
	}
	s.poke()
	if r, err = s.row(ctx, id); err != nil {
		return nil, err
	}
	delivery := s.public(r, s.settings(ctx))
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

// Deliver sends up to ten due notifications to destinations that are on.
// Overlapping calls return immediately.
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
	ready := s.settings(ctx).ready()
	if len(ready) == 0 {
		return nil
	}
	// Deliveries to a destination that is off wait without taking the
	// batch from the others.
	args := []any{db.Now()}
	for _, d := range ready {
		args = append(args, string(d))
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM notification_deliveries
    WHERE state='queued' AND next_attempt_at<=? AND destination IN (?`+strings.Repeat(",?", len(ready)-1)+`)
    ORDER BY created_at, rowid LIMIT 10`, args...)
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
		if ctx.Err() != nil {
			break
		}
		// An administrator may retry another row while a send is waiting; read
		// the current counters right before sending, not from the batch.
		r, err := s.row(ctx, id)
		if err != nil || r.state != "queued" || r.next > db.Now() {
			continue
		}
		d := Destination(r.destination)
		config := s.settings(ctx)[d]
		if !d.ready(config) {
			continue
		}
		attempts, retries := r.attempts+1, r.retries+1
		backoff := min(int64(3600_000), int64(30_000)<<min(retries, 20))
		// Mark active first so Retry refuses the row, and claim only the row as
		// read so a retry that already reset it wins. If the result cannot be
		// saved, the message is sent again after the backoff.
		s.mu.Lock()
		s.active, s.activeTo = id, d
		s.mu.Unlock()
		claimedAt := db.Now()
		result, err := s.DB.ExecContext(ctx, `UPDATE notification_deliveries SET last_attempt_at=?, next_attempt_at=?
      WHERE id=? AND state='queued' AND attempts=? AND retry_attempts=? AND last_attempt_at IS ?`,
			claimedAt, claimedAt+backoff, id, r.attempts, r.retries, r.lastAttempt)
		claimed := int64(0)
		if err == nil {
			claimed, err = result.RowsAffected()
		}
		if err != nil || claimed != 1 {
			if err != nil {
				log.Warn("Could not claim a notification delivery; it will be retried")
			}
			s.mu.Lock()
			s.active, s.activeTo = "", ""
			s.mu.Unlock()
			continue
		}
		// Settings saved before the reread below are seen by it, and a save
		// after it cancels the send, so nothing more goes out under the old
		// settings.
		sendCtx, cancelSend := context.WithCancelCause(ctx)
		s.mu.Lock()
		s.cancelSend = cancelSend
		s.mu.Unlock()
		finishSend := func() {
			s.mu.Lock()
			s.active, s.activeTo, s.cancelSend = "", "", nil
			s.mu.Unlock()
			cancelSend(nil)
		}
		release := func() {
			if _, err := s.DB.ExecContext(ctx, `UPDATE notification_deliveries SET last_attempt_at=?, next_attempt_at=?
        WHERE id=? AND last_attempt_at=?`, r.lastAttempt, r.next, id, claimedAt); err != nil {
				log.Warn("Could not release a notification delivery; it will be retried after its delay")
			}
			finishSend()
		}
		if current := s.settings(ctx)[d]; current != config {
			release()
			continue
		}
		failure := s.send(sendCtx, &noRedirects, d, config, message{id: r.id, kind: r.kind, text: r.message, createdAt: r.createdAt})
		if failure != "" && errors.Is(context.Cause(sendCtx), errSettingsChanged) {
			release()
			continue
		}
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
		finishSend()
	}
	_, err = s.DB.ExecContext(ctx, "DELETE FROM notification_deliveries WHERE state='delivered' AND COALESCE(delivered_at,created_at)<?",
		db.Now()-30*86400_000)
	return err
}

// send delivers one message. Neither response bodies nor transport errors
// are kept: any of them could include a secret.
func (s *Service) send(ctx context.Context, client *http.Client, d Destination, c Config, m message) string {
	if d == Email {
		return s.sendEmail(ctx, c, m)
	}
	timeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := d.request(timeout, c, m)
	if err != nil {
		return "network"
	}
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

// testHook, set only by tests, runs at named points so a test can force an
// interleaving.
var testHook func(point string)
