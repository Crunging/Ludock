// Package ops runs persisted server operations. Every operation is saved
// before it acts on Docker. After a restart, interrupted operations are
// reconciled by their handler, never replayed.
package ops

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/logging"
)

var log = logging.New("operations")

// Statuses.
const (
	Queued         = "queued"
	Running        = "running"
	Succeeded      = "succeeded"
	AlreadyCurrent = "already_current"
	Failed         = "failed"
	Interrupted    = "interrupted"
)

// Job is a persisted operation.
type Job struct {
	ID              string
	ServerID        string
	Kind            string
	Status          string
	Phase           string
	CreatedAt       int64
	UpdatedAt       int64
	Error           string // "" when none
	Result          map[string]any
	ActorID         string
	Input           map[string]any
	Recovery        map[string]any
	BindingRevision int64
}

// Context is a running job.
type Context struct {
	Job    *Job
	runner *Runner
}

// Progress persists the phase with merged recovery state and an optional
// public result, atomically.
//
// The job in memory changes only once the write succeeds, so recovery
// decisions made after a failed write match what a restart would see.
func (c *Context) Progress(phase string, recovery, result map[string]any) error {
	merged := make(map[string]any, len(c.Job.Recovery)+len(recovery))
	for key, value := range c.Job.Recovery {
		merged[key] = value
	}
	for key, value := range recovery {
		merged[key] = value
	}
	if result == nil {
		result = c.Job.Result
	}
	recoveryJSON, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	var resultJSON any
	if result != nil {
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		resultJSON = string(encoded)
	}
	if _, err := c.runner.DB.Exec("UPDATE operations SET phase=?, recovery_json=?, result_json=?, updated_at=? WHERE id=?",
		phase, string(recoveryJSON), resultJSON, db.Now(), c.Job.ID); err != nil {
		return err
	}
	c.Job.Recovery, c.Job.Result, c.Job.Phase = merged, result, phase
	return nil
}

// Handler runs and reconciles one operation kind.
type Handler struct {
	Run     func(ctx context.Context, job *Context) (map[string]any, error)
	Recover func(ctx context.Context, job *Context) (map[string]any, error)
}

// Lanes: start, stop, and plain restart are short and never touch backup
// storage, so they run beside each other and beside one data operation.
// Everything else, including unknown kinds, runs one at a time.
const (
	laneLifecycle = "lifecycle"
	laneData      = "data"
)

var laneCapacity = map[string]int{laneLifecycle: 4, laneData: 1}

// Runner schedules operations.
type Runner struct {
	DB    *db.DB
	Locks *locks.Table
	// Notify queues a notification (notifications.Service.Notify).
	Notify func(key, message string)
	// Username resolves an actor ID for history, or "".
	Username func(id string) string

	mu       sync.Mutex
	handlers map[string]Handler
	enabled  bool
	claimed  map[string]bool
	// yielded jobs found their resources held by another job. No new work
	// starts until each retries, so a job never waits longer than it would
	// have in serial execution.
	yielded map[string]bool
	// waiting jobs wait for background work's cleanup; they leave their lane
	// and return to the queue at the next lock release.
	waiting  map[string]bool
	laneLoad map[string]int
	settled  chan struct{}
	wake     chan struct{}
	stopped  chan struct{}
	inFlight sync.WaitGroup
	loop     sync.WaitGroup
}

// Register adds a handler.
func (r *Runner) Register(kind string, handler Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handlers == nil {
		r.handlers = map[string]Handler{}
	}
	r.handlers[kind] = handler
}

var errInvalidState = apperr.New("INVALID_OPERATION_STATE", 409, "Saved operation state is invalid; administrator review is required")

func parseObject(value string) (map[string]any, error) {
	var result map[string]any
	if json.Unmarshal([]byte(value), &result) != nil || result == nil {
		return nil, errInvalidState
	}
	return result, nil
}

type row struct {
	id, serverID, actorID, kind, status, phase, input, recovery string
	revision, createdAt, updatedAt                              int64
	errorText, result                                           sql.NullString
}

const columns = "id,server_id,actor_id,kind,status,phase,input_json,recovery_json,binding_revision,created_at,updated_at,error,result_json"

type scanner interface{ Scan(...any) error }

func scan(s scanner) (*row, error) {
	var r row
	err := s.Scan(&r.id, &r.serverID, &r.actorID, &r.kind, &r.status, &r.phase, &r.input, &r.recovery, &r.revision,
		&r.createdAt, &r.updatedAt, &r.errorText, &r.result)
	return &r, err
}

func (r *row) job() (*Job, error) {
	job := &Job{ID: r.id, ServerID: r.serverID, Kind: r.kind, Status: r.status, Phase: r.phase, CreatedAt: r.createdAt,
		UpdatedAt: r.updatedAt, Error: r.errorText.String, ActorID: r.actorID, BindingRevision: r.revision}
	var err error
	if r.result.Valid {
		if job.Result, err = parseObject(r.result.String); err != nil {
			return nil, err
		}
	}
	if job.Input, err = parseObject(r.input); err != nil {
		return nil, err
	}
	if job.Recovery, err = parseObject(r.recovery); err != nil {
		return nil, err
	}
	return job, nil
}

func (r *Runner) row(ctx context.Context, id string) (*row, error) {
	result, err := scan(r.DB.QueryRowContext(ctx, "SELECT "+columns+" FROM operations WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return result, err
}

// Get returns an operation, or nil.
func (r *Runner) Get(ctx context.Context, id string) (*Job, error) {
	result, err := r.row(ctx, id)
	if err != nil || result == nil {
		return nil, err
	}
	return result.job()
}

// EnqueueOptions describes a new operation.
type EnqueueOptions struct {
	ServerID        string
	ActorID         string
	Kind            string
	BindingRevision int64
	Input           map[string]any
	// IdempotencyKey returns the earlier operation for a repeated request.
	IdempotencyKey string
}

var errConflict = apperr.New("OPERATION_CONFLICT", 409, "An operation is already queued or running for this server")

// Enqueue saves an operation and wakes the runner. tx, when non-nil, makes
// the insert part of the caller's transaction; the caller then calls Wake
// after committing.
func (r *Runner) Enqueue(ctx context.Context, tx *sql.Tx, options EnqueueOptions) (*Job, error) {
	if tx == nil {
		var job *Job
		err := r.DB.Tx(ctx, func(tx *sql.Tx) error {
			var err error
			job, err = r.Enqueue(ctx, tx, options)
			return err
		})
		if err != nil {
			return nil, err
		}
		r.DB.PruneAuditIfNeeded(ctx)
		r.Wake()
		return job, nil
	}
	if options.Input == nil {
		options.Input = map[string]any{}
	}
	encodedInput, _ := json.Marshal(options.Input)
	input := string(encodedInput)
	var key any
	if options.IdempotencyKey != "" {
		sum := sha256.Sum256([]byte(options.ActorID + ":" + options.ServerID + ":" + options.Kind + ":" + options.IdempotencyKey))
		hexKey := hex.EncodeToString(sum[:])
		key = hexKey
		prior, err := scan(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM operations WHERE request_key = ?", hexKey))
		if err == nil {
			if prior.input != input {
				return nil, apperr.New("IDEMPOTENCY_CONFLICT", 409, "This request key was used for different operation settings")
			}
			return prior.job()
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	var conflict string
	err := tx.QueryRowContext(ctx, "SELECT id FROM operations WHERE server_id = ? AND status IN ('queued','running')", options.ServerID).Scan(&conflict)
	if err == nil {
		return nil, errConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	id := identity.NewUUID()
	now := db.Now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO operations (id,server_id,actor_id,kind,status,phase,input_json,binding_revision,request_key,created_at,updated_at)
    VALUES (?,?,?,?,'queued','queued',?,?,?,?,?)`, id, options.ServerID, options.ActorID, options.Kind, input,
		options.BindingRevision, key, now, now); err != nil {
		return nil, err
	}
	if err := r.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: options.ActorID, Action: "server." + options.Kind + ".queued",
		TargetType: "server", TargetID: options.ServerID, Details: map[string]string{"operationId": id}}); err != nil {
		return nil, err
	}
	result, err := scan(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM operations WHERE id = ?", id))
	if err != nil {
		return nil, err
	}
	return result.job()
}

func (r *Runner) finish(ctx context.Context, job *Job, status string, result map[string]any, errorText string) error {
	var resultJSON, errorValue any
	if result != nil {
		encoded, _ := json.Marshal(result)
		resultJSON = string(encoded)
	}
	if errorText != "" {
		errorValue = errorText
	}
	details := map[string]any{"operationId": job.ID, "actorId": job.ActorID}
	if errorText != "" {
		details["error"] = errorText
	}
	entry := db.AuditEntry{Action: "server." + job.Kind + "." + status, TargetType: "server", TargetID: job.ServerID, Details: details}
	// The owner may have been deleted since queueing.
	if user, _ := r.DB.FindUserByID(ctx, job.ActorID); user != nil {
		entry.UserID = job.ActorID
	}
	// The outcome and its audit entry commit together.
	if err := r.DB.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE operations SET status=?,phase=?,result_json=COALESCE(?,result_json),error=?,updated_at=? WHERE id=?",
			status, status, resultJSON, errorValue, db.Now(), job.ID); err != nil {
			return err
		}
		return r.DB.WriteAudit(ctx, tx, entry)
	}); err != nil {
		return err
	}
	r.DB.PruneAuditIfNeeded(ctx)
	return nil
}

// persist retries a state transition the runner must not skip, such as
// marking a job running before its handler acts or recording its outcome.
// It gives up only when the runner stops; the job is then reconciled or run
// again after a restart, from the state that was last saved.
func (r *Runner) persist(ctx context.Context, what string, write func() error) bool {
	delay := 250 * time.Millisecond
	for {
		err := write()
		if err == nil {
			return true
		}
		log.Warn("Could not save an operation's state; retrying", logging.F("step", what), logging.F("error", err.Error()))
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return false
		case <-r.stopped:
			return false
		}
		delay = min(delay*2, 10*time.Second)
	}
}

func (r *Runner) notifyScheduledFailure(rw *row) {
	input, err := parseObject(rw.input)
	if err != nil || r.Notify == nil {
		return
	}
	if _, ok := input["scheduleId"].(string); !ok {
		return
	}
	// The backup handler may already have recorded its failure; share its key.
	key := "schedule-failed:" + rw.id
	if rw.kind == "backup" {
		key = "backup-failed:" + rw.id
	}
	r.Notify(key, "A scheduled Ludock action failed. Review its operation details.")
}

// resourcesBusy returns a job to the queue until its resources are free.
type resourcesBusy struct {
	released <-chan struct{}
	// holdsQueue: the holder is another job, so no new work starts meanwhile.
	holdsQueue bool
}

func (e *resourcesBusy) Error() string { return "Operation resources are busy" }

// WithLocks acquires a job's resources before any effect. If another job
// holds them, this job returns to the queue and runs again from the start
// once they are released, as it would have after that job. A job cancels
// background work holding its resources and waits for its cleanup without
// holding back other jobs. Conflicts with requests outside the runner fail.
func (c *Context) WithLocks(keys []string, action func() (map[string]any, error)) (map[string]any, error) {
	r := c.runner
	id := c.Job.ID
	release, err := r.Locks.Acquire(keys, nil)
	if err != nil {
		r.mu.Lock()
		claimed := r.claimed[id]
		otherActive := false
		for other := range r.claimed {
			if other != id && !r.yielded[other] {
				otherActive = true
			}
		}
		wasYielded := r.yielded[id]
		delete(r.yielded, id)
		r.mu.Unlock()
		var background *locks.BackgroundConflict
		if claimed && errors.As(err, &background) {
			released := r.Locks.NextRelease()
			background.Cancel()
			if wasYielded {
				r.Wake()
			}
			return nil, &resourcesBusy{released: released}
		}
		if claimed && errors.Is(err, locks.ErrConflict) && otherActive {
			return nil, &resourcesBusy{released: r.Locks.NextRelease(), holdsQueue: true}
		}
		if wasYielded {
			r.Wake()
		}
		return nil, err
	}
	r.mu.Lock()
	wasYielded := r.yielded[id]
	delete(r.yielded, id)
	r.mu.Unlock()
	if wasYielded {
		r.Wake()
	}
	defer release()
	return action()
}

func laneFor(rw *row) string {
	switch rw.kind {
	case "start", "stop":
		return laneLifecycle
	case "restart":
		input, err := parseObject(rw.input)
		if err == nil && input["createBackup"] == true {
			return laneData
		}
		if err == nil {
			return laneLifecycle
		}
	}
	return laneData
}

// Wake asks the runner to look for queued work.
func (r *Runner) Wake() {
	r.mu.Lock()
	wake := r.wake
	r.mu.Unlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func (r *Runner) fill(ctx context.Context) {
	r.mu.Lock()
	if !r.enabled || len(r.yielded) > 0 {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	rows, err := r.DB.QueryContext(ctx, "SELECT "+columns+" FROM operations WHERE status='queued' ORDER BY created_at,id")
	if err != nil {
		r.retryFill()
		return
	}
	var queued []*row
	for rows.Next() {
		if rw, err := scan(rows); err == nil {
			queued = append(queued, rw)
		}
	}
	rows.Close()
	if rows.Err() != nil {
		r.retryFill()
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rw := range queued {
		lane := laneFor(rw)
		if !r.enabled || r.claimed[rw.id] || r.waiting[rw.id] || r.laneLoad[lane] >= laneCapacity[lane] {
			continue
		}
		r.claimed[rw.id] = true
		r.laneLoad[lane]++
		r.inFlight.Add(1)
		go func() {
			defer r.inFlight.Done()
			r.run(ctx, rw)
			r.mu.Lock()
			delete(r.claimed, rw.id)
			delete(r.yielded, rw.id)
			r.laneLoad[lane]--
			if r.settled != nil {
				close(r.settled)
				r.settled = nil
			}
			r.mu.Unlock()
			r.Wake()
		}()
	}
}

// retryFill looks for queued work again shortly. Nothing else may wake the
// runner, so a queue that could not be read must not wait for unrelated work.
func (r *Runner) retryFill() {
	log.Warn("Could not read queued operations; retrying")
	time.AfterFunc(time.Second, r.Wake)
}

func (r *Runner) nextSettled() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settled == nil {
		r.settled = make(chan struct{})
	}
	return r.settled
}

func (r *Runner) handler(kind string) (Handler, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	handler, ok := r.handlers[kind]
	return handler, ok
}

func (r *Runner) run(ctx context.Context, rw *row) {
	fail := func(err error) {
		message, _ := apperr.Public(err)
		job := &Job{ID: rw.id, ServerID: rw.serverID, Kind: rw.kind, ActorID: rw.actorID}
		if r.persist(ctx, "finish", func() error { return r.finish(ctx, job, Failed, nil, message) }) {
			r.notifyScheduledFailure(rw)
		}
	}
	for {
		// Damaged saved state fails this job without wedging the runner.
		job, err := rw.job()
		if err != nil {
			fail(err)
			return
		}
		// A job runs only once it is saved as running: a restart then
		// reconciles it instead of running it a second time.
		if !r.persist(ctx, "start", func() error {
			_, err := r.DB.ExecContext(ctx, "UPDATE operations SET status='running', phase='validating', updated_at=? WHERE id=?", db.Now(), job.ID)
			return err
		}) {
			return
		}
		handler, ok := r.handler(job.Kind)
		if !ok {
			fail(apperr.New("UNSUPPORTED_OPERATION", 409, "This operation type is unavailable"))
			return
		}
		result, err := handler.Run(ctx, &Context{Job: job, runner: r})
		var busy *resourcesBusy
		if !errors.As(err, &busy) {
			if err != nil {
				fail(err)
				return
			}
			if result == nil {
				result = map[string]any{}
			}
			status := Succeeded
			if result["alreadyCurrent"] == true {
				status = AlreadyCurrent
			}
			r.persist(ctx, "finish", func() error { return r.finish(ctx, job, status, result, "") })
			return
		}
		// Nothing has changed yet. Keep the job queued, so a restart while it
		// waits runs it normally instead of reconciling an interruption. Only
		// a saved queued row is picked up again once the resource is free.
		if !r.persist(ctx, "requeue", func() error {
			_, err := r.DB.ExecContext(ctx, "UPDATE operations SET status='queued', phase='queued', updated_at=? WHERE id=?", db.Now(), job.ID)
			return err
		}) {
			return
		}
		if !busy.holdsQueue {
			r.mu.Lock()
			r.waiting[job.ID] = true
			r.mu.Unlock()
			go func() {
				<-busy.released
				r.mu.Lock()
				delete(r.waiting, job.ID)
				r.mu.Unlock()
				r.Wake()
			}()
			return
		}
		r.mu.Lock()
		r.yielded[job.ID] = true
		r.mu.Unlock()
		settled := r.nextSettled()
		select {
		case <-busy.released:
		case <-settled:
		case <-r.stopped:
		}
		current, err := r.row(ctx, job.ID)
		r.mu.Lock()
		enabled := r.enabled
		r.mu.Unlock()
		if err != nil || !enabled || current == nil || current.status != Queued {
			return
		}
		rw = current
	}
}

// Start reconciles interrupted operations, then runs queued work.
func (r *Runner) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.enabled {
		r.mu.Unlock()
		return nil
	}
	r.enabled = true
	r.claimed, r.yielded, r.waiting, r.laneLoad = map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]int{}
	r.wake = make(chan struct{}, 1)
	r.stopped = make(chan struct{})
	r.mu.Unlock()
	if err := r.recoverInterrupted(ctx); err != nil {
		r.mu.Lock()
		r.enabled = false
		r.mu.Unlock()
		return err
	}
	r.loop.Add(1)
	go func() {
		defer r.loop.Done()
		for {
			select {
			case <-r.stopped:
				return
			case <-r.wake:
				r.fill(ctx)
			}
		}
	}()
	r.Wake()
	return nil
}

func (r *Runner) recoverInterrupted(ctx context.Context) error {
	rows, err := r.DB.QueryContext(ctx, "SELECT "+columns+" FROM operations WHERE status='running'")
	if err != nil {
		return err
	}
	var running []*row
	for rows.Next() {
		rw, err := scan(rows)
		if err != nil {
			rows.Close()
			return err
		}
		running = append(running, rw)
	}
	rows.Close()
	// A partial list would leave the rest unreconciled while work starts.
	if err := rows.Err(); err != nil {
		return err
	}
	for _, rw := range running {
		job, err := rw.job()
		var result map[string]any
		if err == nil {
			if handler, ok := r.handler(job.Kind); ok && handler.Recover != nil {
				result, err = handler.Recover(ctx, &Context{Job: job, runner: r})
			}
		}
		// Cancelled recovery is not a failed one: the row stays running and is
		// recovered again on the next startup.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if err := r.finish(ctx, &Job{ID: rw.id, ServerID: rw.serverID, Kind: rw.kind, ActorID: rw.actorID}, Interrupted, nil,
				"Recovery requires administrator attention. The server has been left stopped where possible."); err != nil {
				return err
			}
			continue
		}
		if err := r.finish(ctx, job, Interrupted, result, "Ludock restarted during this operation. Review the server state before retrying."); err != nil {
			return err
		}
	}
	return nil
}

// Stop lets running jobs finish; queued and waiting jobs stay queued.
func (r *Runner) Stop() {
	r.mu.Lock()
	if !r.enabled {
		r.mu.Unlock()
		return
	}
	r.enabled = false
	close(r.stopped)
	if r.settled != nil {
		close(r.settled)
		r.settled = nil
	}
	r.mu.Unlock()
	r.loop.Wait()
	r.inFlight.Wait()
}
