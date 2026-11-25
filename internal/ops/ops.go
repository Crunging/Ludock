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
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/notify"
)

var log = logging.New("operations")

const (
	Queued         = "queued"
	Running        = "running"
	Succeeded      = "succeeded"
	AlreadyCurrent = "already_current"
	Failed         = "failed"
	Interrupted    = "interrupted"
	// Skipped work decided not to act, such as a scheduled command for a
	// stopped server.
	Skipped = "skipped"
	// Cancelled work was called off while it waited, before it acted.
	Cancelled = "cancelled"
)

// PhaseWarning is a running operation waiting out the time players were
// given. Only it and queued work can be cancelled: every later phase acts.
const PhaseWarning = "warning"

// PhaseWarningPlayers is a running operation still sending its warning,
// which it tries again while Docker or the console is briefly unreachable.
// It can be cancelled like PhaseWarning.
const PhaseWarningPlayers = "warning_players"

// ErrCancelled is what a cancelled job's next save returns; the job then
// stops without acting.
var ErrCancelled = errors.New("operation cancelled")

// Kinds of operation. Lanes, capabilities and the --rm refusal all switch
// on these.
const (
	KindStart       = "start"
	KindStop        = "stop"
	KindRestart     = "restart"
	KindBackup      = "backup"
	KindRestore     = "restore"
	KindUndoRestore = "undo_restore"
	KindUpdate      = "update"
	KindCommand     = "command"
)

// RecoveryActor starts the restarts of automatic recovery, on the authority
// of the server's monitoring policy instead of an account.
const RecoveryActor = "ludock-recovery"

type Job struct {
	ID              string
	ServerID        string
	Kind            string
	Status          string
	Phase           string
	CreatedAt       int64
	UpdatedAt       int64
	Error           string // "" when none
	Result          Result
	ActorID         string
	Input           map[string]any
	Recovery        Recovery
	BindingRevision int64
}

// Recovery is what an operation saves before each step that acts on Docker,
// so a restart reconciles the step instead of repeating it.
type Recovery struct {
	// The container, binding, and locks a data operation acted on.
	ContainerID string   `json:"containerId,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	LockKeys    []string `json:"lockKeys,omitempty"`

	// InitialRunning is saved once, before a data operation stops the server;
	// nil means it never got that far.
	InitialRunning *bool `json:"initialRunning,omitempty"`
	// A dispatched stop may still be running in Docker until StopSettled or
	// StopRecoveryDeadlineAt (Unix ms). A container whose start time differs
	// from StopInitialStartedAt was started again by another manager.
	StopStarted            bool   `json:"stopStarted,omitempty"`
	StopSettled            bool   `json:"stopSettled,omitempty"`
	StopRecoveryDeadlineAt int64  `json:"stopRecoveryDeadlineAt,omitempty"`
	StopInitialStartedAt   *int64 `json:"stopInitialStartedAt,omitempty"`
	StopRecoveryTimedOut   bool   `json:"stopRecoveryTimedOut,omitempty"`
	ForceStopped           bool   `json:"forceStopped,omitempty"`
	// StoppedStartedAt is the stopped container's StartedAt, which changes if
	// anything starts it.
	StoppedStartedAt string `json:"stoppedStartedAt,omitempty"`
	StartRequested   bool   `json:"startRequested,omitempty"`
	StateRestored    bool   `json:"stateRestored,omitempty"`

	SaveStatus string `json:"saveStatus,omitempty"`
	SaveReason string `json:"saveReason,omitempty"`

	DataHelperID      string `json:"dataHelperId,omitempty"`
	BackupID          string `json:"backupId,omitempty"`
	BackupDestination string `json:"backupDestination,omitempty"`
	BackupComplete    bool   `json:"backupComplete,omitempty"`

	// DataUnsafe is set while a restore is replacing or rolling back data.
	DataUnsafe bool `json:"dataUnsafe,omitempty"`
	// PendingUndo is set while this operation finishes undoing an earlier
	// restore that ended partway; the data is unsafe until it is cleared.
	// Undone says, for Activity, what that left.
	PendingUndo bool   `json:"pendingUndo,omitempty"`
	Undone      string `json:"undone,omitempty"`
	// LeftOut names a folder holding a restore's stage folder, which the
	// backup this operation takes leaves out.
	LeftOut string `json:"leftOut,omitempty"`

	// WarnUntil (Unix ms) is when players were told the action happens,
	// saved before the warning is sent; WarnSent once it was.
	WarnUntil int64 `json:"warnUntil,omitempty"`
	WarnSent  bool  `json:"warnSent,omitempty"`
	// WarnSkipped: nobody was online to warn, so the operation didn't wait.
	WarnSkipped bool `json:"warnSkipped,omitempty"`
	// WarnFailed: the warning could not be sent, and the work went ahead.
	WarnFailed bool `json:"warnFailed,omitempty"`
	// Restore and Swap are the restore journal and update swap, which their
	// handlers decode and validate.
	Restore json.RawMessage `json:"restore,omitempty"`
	Swap    json.RawMessage `json:"swap,omitempty"`
}

type Context struct {
	Job       *Job
	runner    *Runner
	cancelled chan struct{}
	// ended is the status of a job that already ended and whose recovery
	// state later work finishes; "" for a running job.
	ended string
}

// Ended is the context of a job that already ended, failed or interrupted,
// whose recovery state another operation or startup still finishes. Its
// Progress saves without changing the outcome.
func (r *Runner) Ended(job *Job) *Context {
	return &Context{Job: job, runner: r, ended: job.Status}
}

// Cancelled closes when someone cancels the job while it waits.
func (c *Context) Cancelled() <-chan struct{} { return c.cancelled }

// Active returns ErrCancelled once the job was cancelled.
func (c *Context) Active(ctx context.Context) error {
	var status string
	if err := c.runner.DB.QueryRowContext(ctx, "SELECT status FROM operations WHERE id=?", c.Job.ID).Scan(&status); err != nil {
		return err
	}
	if status != Running {
		return ErrCancelled
	}
	return nil
}

// Progress atomically persists the phase, the recovery state after change
// (when not nil), and the public result unless it is nil. The in-memory job
// changes only once the write succeeds, so it matches what a restart would see.
func (c *Context) Progress(phase string, change func(*Recovery), result *Result) error {
	saved, err := c.progress(c.runner.DB, phase, change, result)
	if err == nil {
		saved()
	}
	return err
}

// ProgressIn is Progress through tx. saved updates the job once tx commits.
func (c *Context) ProgressIn(tx *sql.Tx, phase string, change func(*Recovery), result *Result) (saved func(), err error) {
	return c.progress(tx, phase, change, result)
}

func (c *Context) progress(q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, phase string, change func(*Recovery), result *Result) (func(), error) {
	merged := c.Job.Recovery
	merged.LockKeys = slices.Clone(merged.LockKeys)
	if change != nil {
		change(&merged)
	}
	kept := c.Job.Result
	if result != nil {
		kept = *result
	}
	recoveryJSON, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	var resultJSON any
	if result != nil || kept != (Result{}) {
		resultJSON = encodeResult(&kept)
	}
	// A cancelled job is no longer running, so it saves nothing more, and
	// whichever of the cancel and this write commits first wins.
	// An ended job keeps the time it ended, which Activity and Needs
	// attention show, through a later undo.
	status, updated := Running, db.Now()
	if c.ended != "" {
		status, phase, updated = c.ended, c.Job.Phase, c.Job.UpdatedAt
	}
	saved, err := q.ExecContext(context.Background(), "UPDATE operations SET phase=?, recovery_json=?, result_json=?, updated_at=? WHERE id=? AND status=?",
		phase, string(recoveryJSON), resultJSON, updated, c.Job.ID, status)
	if err != nil {
		return nil, err
	}
	if changed, err := saved.RowsAffected(); err != nil {
		return nil, err
	} else if changed == 0 {
		return nil, ErrCancelled
	}
	return func() { c.Job.Recovery, c.Job.Result, c.Job.Phase = merged, kept, phase }, nil
}

// ErrResume, returned by Recover, queues an interrupted job again: it had not
// acted on anything yet, so running it once more is not a replay.
var ErrResume = errors.New("operation resumes")

// Handler runs and reconciles one operation kind.
type Handler struct {
	Run     func(ctx context.Context, job *Context) (*Result, error)
	Recover func(ctx context.Context, job *Context) (*Result, error)
}

// Lanes: start, stop, and plain restart are short and never touch backup
// storage, so they run beside each other and beside one data operation.
// Scheduled console commands are sent at their time or not at all, so they
// have their own lane.
// Everything else, including unknown kinds, runs one at a time.
const (
	laneLifecycle = "lifecycle"
	laneData      = "data"
	laneConsole   = "console"
)

var laneCapacity = map[string]int{laneLifecycle: 4, laneData: 1, laneConsole: 4}

type Runner struct {
	DB    *db.DB
	Locks *locks.Table
	// Queue queues an alert inside tx (notify.Service.QueueIn), so a failed
	// scheduled job never finishes without its alert; Alerted wakes delivery
	// once tx commits. Either may be nil.
	Queue   func(ctx context.Context, tx *sql.Tx, alert notify.Alert, key, message string) (bool, error)
	Alerted func()
	// Username resolves an actor ID for history, or "".
	Username func(id string) string
	// Changed tells open pages that a server's operation ended: Docker has
	// no event for that, and the container's last one can come well before.
	// It must not block.
	Changed func(serverID string)
	// AfterRecovery, when set, runs once interrupted operations are
	// reconciled and before queued work can take locks.
	AfterRecovery func(ctx context.Context)

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
	waiting map[string]bool
	// signals tell running handlers they were cancelled.
	signals  map[string]chan struct{}
	laneLoad map[string]int
	settled  chan struct{}
	wake     chan struct{}
	stopped  chan struct{}
	inFlight sync.WaitGroup
	loop     sync.WaitGroup
}

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

func encodeResult(result *Result) string {
	encoded, _ := json.Marshal(result)
	return string(encoded)
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
		var result *Result
		if json.Unmarshal([]byte(r.result.String), &result) != nil || result == nil {
			return nil, errInvalidState
		}
		job.Result = *result
	}
	if job.Input, err = parseObject(r.input); err != nil {
		return nil, err
	}
	var recovery *Recovery
	if json.Unmarshal([]byte(r.recovery), &recovery) != nil || recovery == nil {
		return nil, errInvalidState
	}
	job.Recovery = *recovery
	return job, nil
}

func (r *Runner) row(ctx context.Context, id string) (*row, error) {
	result, err := scan(r.DB.QueryRowContext(ctx, "SELECT "+columns+" FROM operations WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return result, err
}

func (r *Runner) Get(ctx context.Context, id string) (*Job, error) {
	result, err := r.row(ctx, id)
	if err != nil || result == nil {
		return nil, err
	}
	return result.job()
}

// LastOutcome is a server's newest work of kind that ran to an end, or nil.
func (r *Runner) LastOutcome(ctx context.Context, serverID, kind string) (*Job, error) {
	result, err := scan(r.DB.QueryRowContext(ctx, "SELECT "+columns+` FROM operations WHERE server_id=? AND kind=?
      AND status IN ('succeeded','failed','interrupted') ORDER BY created_at DESC,id DESC LIMIT 1`, serverID, kind))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return result.job()
}

type EnqueueOptions struct {
	ServerID        string
	ActorID         string
	Kind            string
	BindingRevision int64
	Input           map[string]any
	// IdempotencyKey returns the earlier operation for a repeated request.
	IdempotencyKey string
}

var errConflict = apperr.New("OPERATION_CONFLICT", 409, "Something else is already running on this server. Wait for it to finish.")

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
	entry := db.AuditEntry{UserID: options.ActorID, Action: "server." + options.Kind + ".queued",
		TargetType: "server", TargetID: options.ServerID, Details: map[string]string{"operationId": id}}
	if options.ActorID == RecoveryActor {
		entry.UserID, entry.Details = "", map[string]string{"operationId": id, "actorId": RecoveryActor}
	}
	if err := r.DB.WriteAudit(ctx, tx, entry); err != nil {
		return nil, err
	}
	result, err := scan(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM operations WHERE id = ?", id))
	if err != nil {
		return nil, err
	}
	return result.job()
}

// finish records a running job's outcome, reporting false when it was
// cancelled instead. waited notes in its result that it waited for Docker.
func (r *Runner) finish(ctx context.Context, job *Job, status string, result *Result, errorText string, waited bool) (bool, error) {
	return r.finishAlerting(ctx, job, status, result, errorText, waited, nil)
}

// finishAlerting is finish for the job of rw, which queues the alert of a
// failed scheduled job with its outcome.
func (r *Runner) finishAlerting(ctx context.Context, job *Job, status string, result *Result, errorText string, waited bool,
	scheduled *row) (bool, error) {
	var resultJSON, errorValue any
	if result != nil {
		resultJSON = encodeResult(result)
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
	// The outcome and its audit entry commit together. A job cancelled
	// meanwhile keeps the outcome its cancellation recorded.
	finished, alerted := false, false
	if err := r.DB.Tx(ctx, func(tx *sql.Tx) error {
		updated, err := tx.ExecContext(ctx, `UPDATE operations SET status=?1,phase=?1,result_json=CASE
			WHEN ?2 AND json_valid(COALESCE(?3,result_json,'{}')) THEN json_set(COALESCE(?3,result_json,'{}'),'`+PathWaitedForDocker+`',json('true'))
			ELSE COALESCE(?3,result_json) END,error=?4,updated_at=?5 WHERE id=?6 AND status='running'`,
			status, waited, resultJSON, errorValue, db.Now(), job.ID)
		if err != nil {
			return err
		}
		if changed, err := updated.RowsAffected(); err != nil || changed == 0 {
			return err
		}
		finished = true
		if err := r.DB.FinishOperationAudit(ctx, tx, job.ID, "server."+job.Kind+".queued", entry); err != nil {
			return err
		}
		alerted, err = r.queueScheduledFailure(ctx, tx, scheduled)
		return err
	}); err != nil {
		return false, err
	}
	if alerted && r.Alerted != nil {
		r.Alerted()
	}
	if !finished {
		return false, nil
	}
	if r.Changed != nil {
		r.Changed(job.ServerID)
	}
	r.DB.PruneAuditIfNeeded(ctx)
	return true, nil
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
		if ctx.Err() != nil {
			return false
		}
		log.Warn("Could not save an operation's state; retrying", append([]logging.Field{logging.F("step", what)}, logging.Cause(err, 0)...)...)
		if testHook != nil {
			testHook("retry " + what)
		}
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

// queueScheduledFailure queues the alert for a failed job that a schedule
// started, through tx.
func (r *Runner) queueScheduledFailure(ctx context.Context, tx *sql.Tx, rw *row) (bool, error) {
	if rw == nil || r.Queue == nil {
		return false, nil
	}
	input, err := parseObject(rw.input)
	if err != nil {
		return false, nil
	}
	if _, ok := input["scheduleId"].(string); !ok {
		return false, nil
	}
	// The backup handler may already have recorded its failure; share its key.
	key := "schedule-failed:" + rw.id
	if rw.kind == KindBackup {
		key = "backup-failed:" + rw.id
	}
	name := "A server"
	var display string
	if tx.QueryRowContext(ctx, "SELECT display_name FROM logical_servers WHERE id=?", rw.serverID).Scan(&display) == nil {
		name = display
	}
	return r.Queue(ctx, tx, notify.FailedWork, key, name+": a scheduled "+rw.kind+" failed. Its Activity says why.")
}

// resourcesBusy returns a job to the queue until its resources are free.
type resourcesBusy struct {
	released <-chan struct{}
	// settled closes when any job finishes; nil unless holdsQueue.
	settled <-chan struct{}
	// holdsQueue: the holder is another job, so no new work starts meanwhile.
	holdsQueue bool
}

func (e *resourcesBusy) Error() string { return "Operation resources are busy" }

// WithLocks acquires a job's resources before any effect. If another job
// holds them, this job returns to the queue and runs again from the start
// once they are released, as it would have after that job. A job cancels
// background work holding its resources and waits for its cleanup, and waits
// for a request holding them, such as a download, without holding back other
// jobs.
func (c *Context) WithLocks(keys []string, action func() (*Result, error)) (*Result, error) {
	r := c.runner
	id := c.Job.ID
	// Taken before trying, so a release or finish right after a conflict
	// still wakes this job instead of leaving the queue held.
	released := r.Locks.NextRelease()
	release, err := r.Locks.Acquire(keys, nil)
	for err != nil {
		if testHook != nil {
			testHook("conflict")
		}
		// Keys were released since this attempt began, perhaps by the holder
		// as it finished: try again rather than report a conflict.
		select {
		case <-released:
			released = r.Locks.NextRelease()
			release, err = r.Locks.Acquire(keys, nil)
			continue
		default:
		}
		r.mu.Lock()
		if r.settled == nil {
			r.settled = make(chan struct{})
		}
		settled := r.settled
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
			background.Cancel()
			if wasYielded {
				r.Wake()
			}
			return nil, &resourcesBusy{released: released}
		}
		if claimed && errors.Is(err, locks.ErrConflict) && otherActive {
			if testHook != nil {
				testHook("yield")
			}
			return nil, &resourcesBusy{released: released, settled: settled, holdsQueue: true}
		}
		if claimed && errors.Is(err, locks.ErrConflict) {
			if wasYielded {
				r.Wake()
			}
			return nil, &resourcesBusy{released: released}
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

// testHook, set only by tests, runs at named points in WithLocks and persist
// so a test can force an interleaving or see a retry.
var testHook func(point string)

func laneFor(rw *row) string {
	switch rw.kind {
	case KindStart, KindStop:
		return laneLifecycle
	case KindCommand:
		return laneConsole
	case KindRestart:
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
	// Nothing can be claimed while every lane is busy.
	if !r.enabled || len(r.yielded) > 0 || r.lanesFull() {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	rows, err := r.DB.QueryContext(ctx, "SELECT "+columns+" FROM operations WHERE status='queued' ORDER BY created_at,id")
	if ctx.Err() != nil {
		return
	}
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
	if ctx.Err() != nil {
		return
	}
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

func (r *Runner) lanesFull() bool {
	for lane, capacity := range laneCapacity {
		if r.laneLoad[lane] < capacity {
			return false
		}
	}
	return true
}

// retryFill looks for queued work again shortly. Nothing else may wake the
// runner, so a queue that could not be read must not wait for unrelated work.
func (r *Runner) retryFill() {
	log.Warn("Could not read queued operations; retrying")
	time.AfterFunc(time.Second, r.Wake)
}

func (r *Runner) handler(kind string) (Handler, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	handler, ok := r.handlers[kind]
	return handler, ok
}

func (r *Runner) run(ctx context.Context, rw *row) {
	var waited atomic.Bool
	work := docker.RideOut(ctx, func() { waited.Store(true) })
	defer func() {
		r.mu.Lock()
		delete(r.signals, rw.id)
		r.mu.Unlock()
	}()
	fail := func(err error) {
		message, _ := apperr.Public(err)
		if docker.StatusCode(err) >= 500 {
			message = "Docker reported an error, so this stopped. Check the server before trying again."
		} else if docker.Unavailable(err) {
			message = "Ludock couldn’t reach Docker, so this stopped. Check the server before trying again."
		}
		// Work stopped by shutdown is reconciled on the next start, not
		// failed, and cancelled work keeps the outcome Cancel recorded.
		if _, public := apperr.As(err); !public && ctx.Err() == nil && !errors.Is(err, ErrCancelled) {
			fields := append([]logging.Field{logging.F("operation", rw.id), logging.F("kind", rw.kind)},
				logging.Cause(err, docker.StatusCode(err))...)
			// Docker failing or going away is the daemon's fault, not Ludock's.
			if docker.Unavailable(err) {
				log.Warn("Operation stopped by a Docker error", fields...)
			} else {
				log.Error("Operation failed unexpectedly", fields...)
			}
		}
		job := &Job{ID: rw.id, ServerID: rw.serverID, Kind: rw.kind, ActorID: rw.actorID}
		r.persist(ctx, "finish", func() error {
			_, err := r.finishAlerting(ctx, job, Failed, nil, message, waited.Load(), rw)
			return err
		})
	}
	for {
		// A job runs only once it is saved as running: a restart then
		// reconciles it instead of running it a second time. The row may be
		// a stale read of a job that already ran, so only a queued one is
		// claimed.
		claimed := false
		if !r.persist(ctx, "start", func() error {
			result, err := r.DB.ExecContext(ctx, "UPDATE operations SET status='running', phase='validating', updated_at=? WHERE id=? AND status='queued'",
				db.Now(), rw.id)
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			claimed = changed == 1
			return err
		}) || !claimed {
			return
		}
		// Damaged saved state fails this job without wedging the runner.
		job, err := rw.job()
		if err != nil {
			fail(err)
			return
		}
		handler, ok := r.handler(job.Kind)
		if !ok {
			fail(apperr.New("UNSUPPORTED_OPERATION", 409, "This operation type is unavailable"))
			return
		}
		// The signal stays registered while the job waits in the queue below.
		signal := make(chan struct{})
		r.mu.Lock()
		r.signals[job.ID] = signal
		r.mu.Unlock()
		result, err := handler.Run(work, &Context{Job: job, runner: r, cancelled: signal})
		var busy *resourcesBusy
		if !errors.As(err, &busy) {
			if err != nil {
				fail(err)
				return
			}
			if result == nil {
				result = &Result{}
			}
			status := Succeeded
			if result.AlreadyCurrent {
				status = AlreadyCurrent
			} else if result.Skipped {
				status = Skipped
			}
			r.persist(ctx, "finish", func() error {
				_, err := r.finish(ctx, job, status, result, "", waited.Load())
				return err
			})
			return
		}
		// Nothing has changed yet, so keep the job queued: a restart while it
		// waits runs it normally instead of reconciling it.
		requeued := false
		if !r.persist(ctx, "requeue", func() error {
			result, err := r.DB.ExecContext(ctx, "UPDATE operations SET status='queued', phase='queued', updated_at=? WHERE id=? AND status='running'", db.Now(), job.ID)
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			requeued = changed == 1
			return err
		}) || !requeued {
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
		select {
		case <-busy.released:
		case <-busy.settled:
		case <-r.stopped:
		case <-signal:
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

// Start reconciles interrupted operations, calls recovered (if not nil),
// then runs queued work. recovered lifts the startup lock fence, so queued
// work never reaches the fence and fails.
func (r *Runner) Start(ctx context.Context, recovered func()) error {
	r.mu.Lock()
	if r.enabled {
		r.mu.Unlock()
		return nil
	}
	r.enabled = true
	r.claimed, r.yielded, r.waiting, r.laneLoad = map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]int{}
	r.signals = map[string]chan struct{}{}
	r.wake = make(chan struct{}, 1)
	r.stopped = make(chan struct{})
	r.mu.Unlock()
	if err := r.recoverInterrupted(ctx); err != nil {
		r.mu.Lock()
		r.enabled = false
		r.mu.Unlock()
		return err
	}
	if recovered != nil {
		recovered()
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
		var result *Result
		startedBefore := false
		if err == nil {
			startedBefore = job.Recovery.StartRequested
			if handler, ok := r.handler(job.Kind); ok && handler.Recover != nil {
				result, err = handler.Recover(ctx, &Context{Job: job, runner: r})
			}
		}
		// Cancelled recovery is not a failed one: the row stays running and is
		// recovered again on the next startup.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrResume) {
			if _, err := r.DB.ExecContext(ctx, "UPDATE operations SET status='queued', phase='queued', updated_at=? WHERE id=? AND status='running'",
				db.Now(), rw.id); err != nil {
				return err
			}
			logRecovered(rw, "queued to run again")
			continue
		}
		if err != nil {
			if _, err := r.finish(ctx, &Job{ID: rw.id, ServerID: rw.serverID, Kind: rw.kind, ActorID: rw.actorID}, Interrupted, nil,
				"Recovery requires administrator attention. The server has been left stopped where possible.", false); err != nil {
				return err
			}
			logRecovered(rw, "needs an administrator")
			continue
		}
		if _, err := r.finish(ctx, job, Interrupted, result, "Ludock restarted while this ran. Check the server before trying again.", false); err != nil {
			return err
		}
		outcome := "ended as interrupted"
		if job.Recovery.StateRestored && job.Recovery.StartRequested && !startedBefore {
			outcome = "ended as interrupted, server started again"
		}
		logRecovered(rw, outcome)
	}
	if r.AfterRecovery != nil {
		r.AfterRecovery(ctx)
	}
	return nil
}

// logRecovered logs what startup did with an interrupted operation, in fixed
// words.
func logRecovered(rw *row, outcome string) {
	log.Info("Recovered an operation a restart interrupted", logging.F("operation", rw.id), logging.F("kind", rw.kind), logging.F("outcome", outcome))
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

var (
	errNotWaiting = apperr.New("OPERATION_STARTED", 409, "It already began, so it can’t be cancelled. Ludock will finish it.")
	errEnded      = apperr.New("OPERATION_ENDED", 409, "It already ended, so there’s nothing to cancel.")
)

// Waiting reports whether a job can still be cancelled: it is queued, or
// waits out a warning.
func Waiting(job *Job) bool {
	return job.Status == Queued || (job.Status == Running && (job.Phase == PhaseWarning || job.Phase == PhaseWarningPlayers))
}

// Cancel calls off a job that is still waiting, on behalf of userID, and
// returns it as it was just before. The status write is conditional on
// the job still waiting, so of a cancel and the job's next save, whichever
// commits first wins; a running handler then stops at its next wait or save.
// within, when set, runs the write in a transaction that also rechecks the
// requester, such as db.AdminMutation.
func (r *Runner) Cancel(ctx context.Context, id, userID string, within func(func(*sql.Tx) error) error) (*Job, error) {
	if within == nil {
		within = func(fn func(*sql.Tx) error) error { return r.DB.Tx(ctx, fn) }
	}
	var before *Job
	err := within(func(tx *sql.Tx) error {
		rw, err := scan(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM operations WHERE id = ?", id))
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New("NOT_FOUND", 404, "Not found")
		}
		if err != nil {
			return err
		}
		if before, err = rw.job(); err != nil {
			return err
		}
		if !Waiting(before) {
			if before.Status == Running {
				return errNotWaiting
			}
			return errEnded
		}
		updated, err := tx.ExecContext(ctx, `UPDATE operations SET status=?, phase=?, error=NULL,
      result_json=json_set(COALESCE(result_json,'{}'),'`+PathCancelledBy+`',?), updated_at=? WHERE id=? AND status=? AND phase=?`,
			Cancelled, Cancelled, userID, db.Now(), id, rw.status, rw.phase)
		if err != nil {
			return err
		}
		if changed, err := updated.RowsAffected(); err != nil {
			return err
		} else if changed == 0 {
			return errNotWaiting
		}
		details := map[string]any{"operationId": id, "actorId": before.ActorID, "cancelledBy": userID}
		// The entry stays the owner's, as queued; details name who cancelled.
		entry := db.AuditEntry{Action: "server." + before.Kind + "." + Cancelled, TargetType: "server",
			TargetID: before.ServerID, Details: details}
		return r.DB.FinishOperationAudit(ctx, tx, id, "server."+before.Kind+".queued", entry)
	})
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if signal := r.signals[id]; signal != nil {
		close(signal)
		delete(r.signals, id)
	}
	r.mu.Unlock()
	if r.Changed != nil {
		r.Changed(before.ServerID)
	}
	r.DB.PruneAuditIfNeeded(ctx)
	return before, nil
}
