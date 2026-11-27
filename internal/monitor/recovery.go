package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
)

const (
	// RecoveryAttempts restarts in RecoveryWindow, after which recovery
	// gives up until the window has room again.
	RecoveryAttempts = 3
	RecoveryWindow   = 6 * time.Hour
	// recoveryWaitMax caps RecoveryWait for long alert waits.
	recoveryWaitMax = 30 * time.Minute
)

// What recovery last did.
const (
	RecoveryRestarted   = "restarted"
	RecoveryAnswered    = "answered"
	RecoveryNotAnswered = "not_answered"
	RecoveryFailed      = "failed"
	RecoverySkipped     = "skipped"
	RecoveryGaveUp      = "gave_up"
)

// Recovery is a server's automatic restarts, saved so a restart of Ludock
// keeps the count and the pause.
type Recovery struct {
	// Attempts are the restart times within RecoveryWindow, oldest first.
	Attempts    []int64 `json:"attempts,omitempty"`
	OperationID string  `json:"operationId,omitempty"`
	RestartedAt int64   `json:"restartedAt,omitempty"`
	// Pending: the latest restart's outcome isn't known yet.
	Pending bool   `json:"pending,omitempty"`
	Last    string `json:"last,omitempty"`
	LastAt  int64  `json:"lastAt,omitempty"`
	// SilentSince is when the game of container run SilentRun (its start
	// time) was first seen not answering since it last answered.
	SilentSince int64 `json:"silentSince,omitempty"`
	SilentRun   int64 `json:"silentRun,omitempty"`
	// WatchRun is the run followed after it was reported not answering.
	WatchRun int64 `json:"watchRun,omitempty"`
}

// RecoveryOffered reports whether a container's game can be recovered: only
// a status query, or the image's health check in its place, tells a hung
// game from a slow one.
func RecoveryOffered(container servers.Container) bool {
	return (container.Query != nil || container.HealthAnswers) && !container.Refuses(ops.KindRestart)
}

// RecoveryWait is how long after a recovery restart the game has to answer
// again, and the least time between restarts: five times the alert wait, so
// ten minutes by default.
func RecoveryWait(policy Policy) int64 {
	return min(5*int64(policy.GraceSeconds)*1000, recoveryWaitMax.Milliseconds())
}

// RecoveryOn reports whether recovery may restart the server under this
// policy.
func (p Policy) RecoveryOn() bool { return p.Recover && p.Enabled && !p.Maintenance }

// Recovery reads what recovery last did on a server.
func (s *Service) Recovery(ctx context.Context, serverID string) (Recovery, error) {
	return recoveryOf(s.row(ctx, serverID))
}

// RecoveryIn is Recovery through tx.
func (s *Service) RecoveryIn(ctx context.Context, tx *sql.Tx, serverID string) (Recovery, error) {
	return recoveryOf(rowIn(ctx, tx, serverID))
}

func recoveryOf(r *row, err error) (Recovery, error) {
	var state Recovery
	if err != nil || r == nil {
		return state, err
	}
	return state, decode(r.recovery, &state)
}

// SetRecovery turns recovery on or off for a server, keeping the rest of its
// policy. The administrator's session and role are checked again in the
// transaction that saves it.
func (s *Service) SetRecovery(ctx context.Context, adminID, tokenHash, serverID string, on bool) error {
	return s.DB.AdminMutation(ctx, adminID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		var raw string
		var recovery sql.NullString
		err := tx.QueryRowContext(ctx, "SELECT policy_json,recovery_json FROM availability WHERE server_id=?", serverID).Scan(&raw, &recovery)
		policy := DefaultPolicy()
		if err == nil {
			if policy, err = ParsePolicy([]byte(raw)); err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if policy.Recover == on {
			return nil
		}
		recovery = resetRecovery(recovery, policy, Policy{Recover: on})
		policy.Recover = on
		encoded, _ := json.Marshal(policy)
		_, err = tx.ExecContext(ctx, `INSERT INTO availability(server_id,policy_json,updated_at) VALUES(?,?,?)
      ON CONFLICT(server_id) DO UPDATE SET policy_json=excluded.policy_json,updated_at=excluded.updated_at,recovery_json=?`,
			serverID, string(encoded), db.Now(), recovery)
		return err
	})
}

// resetRecovery forgets a restart's pending outcome when recovery is turned
// off, so turning it on later doesn't report a stale one. It returns the
// state to save with policy.
func resetRecovery(raw sql.NullString, previous, policy Policy) sql.NullString {
	var state Recovery
	if !previous.Recover || policy.Recover || decode(raw, &state) != nil || !state.Pending {
		return raw
	}
	state.Pending = false
	return encode(state)
}

// saveRecovery replaces a server's recovery state only if it is still as r
// holds it and the policy it was judged by is still saved, queuing alerts
// with it; r then holds the saved state.
func (s *Service) saveRecovery(ctx context.Context, serverID string, r *row, state Recovery, alerts ...alert) bool {
	saved, changed, err := s.saveState(ctx, serverID, r.policy, recoveryColumn, r.recovery, state, alerts...)
	if err != nil {
		log.Warn("Could not save a server's automatic restart; it will be checked again")
	}
	if changed {
		r.recovery = saved
	}
	return changed
}

func recentAttempts(attempts []int64, now int64) []int64 {
	var recent []int64
	for _, at := range attempts {
		if now-at < RecoveryWindow.Milliseconds() {
			recent = append(recent, at)
		}
	}
	return recent
}

var errRecoveryChanged = errors.New("recovery changed")

// recover restarts a server whose game has been silent in this container run
// for the alert wait. It reports whether it announced this outage, by
// restarting or by giving up, so the plain outage alert isn't sent as well.
// Another operation on the server, or the pause after the last restart, makes
// it wait.
func (s *Service) recover(ctx context.Context, server identity.Server, container servers.Container, r *row, policy Policy, now int64) bool {
	// A server awaiting review of a change can't be restarted by recovery,
	// nor one Docker deletes when it stops; its outage is alerted instead.
	if s.Runner == nil || server.SharingPaused || container.Refuses(ops.KindRestart) {
		return false
	}
	var state Recovery
	if err := decode(r.recovery, &state); err != nil {
		log.Warn("Could not read a server's automatic restarts; it will be checked again")
		return false
	}
	// An outage with another cause first, such as Docker being unreachable,
	// doesn't count toward the game's silence.
	if state.SilentSince == 0 || state.SilentRun != container.StateSince || now-state.SilentSince < int64(policy.GraceSeconds)*1000 {
		return false
	}
	// Having given up on this silence, recovery leaves it to a person, even
	// if a settings change restarts the outage.
	if state.Last == RecoveryGaveUp && state.LastAt >= state.SilentSince {
		return true
	}
	// An answer since the silence began starts a new one, with its own wait.
	answered := s.lastAnswer(server.ID, container)
	if state.Pending || answered > state.SilentSince {
		return false
	}
	// The pause runs from the last restart's end, even one whose outcome was
	// never settled because recovery was turned off meanwhile.
	var finished int64
	if state.OperationID != "" {
		if err := s.DB.QueryRowContext(ctx, "SELECT updated_at FROM operations WHERE id=?", state.OperationID).Scan(&finished); err != nil &&
			!errors.Is(err, sql.ErrNoRows) {
			return false
		}
	}
	if last := max(state.RestartedAt, finished); last != 0 && now-last < RecoveryWait(policy) {
		return false
	}
	state.Attempts = recentAttempts(state.Attempts, now)
	if len(state.Attempts) >= RecoveryAttempts {
		state.Last, state.LastAt = RecoveryGaveUp, now
		// Like the restart below, giving up is announced only for the outage
		// as it was read: a stop or pause recorded since ends it.
		saved := encode(state)
		changed, err := s.apply(ctx, []alert{{"recovery-gave-up:" + server.ID + ":" + strconv.FormatInt(state.SilentSince, 10),
			server.DisplayName + ": the game stopped answering again. Ludock already tried restarting it 3 times in 6 hours, so it won’t restart it again for now; it needs a look.", nil}},
			"UPDATE availability SET recovery_json=? WHERE server_id=? AND policy_json=? AND recovery_json IS ? AND "+asRead,
			append([]any{saved, server.ID, r.policy, r.recovery}, r.asRead()...)...)
		if err != nil {
			log.Warn("Could not save a server's automatic restart; it will be checked again")
		}
		if !changed {
			return false
		}
		r.recovery = saved
		s.changed(server.ID)
		return true
	}
	var job *ops.Job
	var saved sql.NullString
	var restarting []alert
	err := s.DB.Tx(ctx, func(tx *sql.Tx) error {
		// The policy, restarts and outage are as judged above, read on this
		// connection: a stop or pause recorded since ends the outage.
		var same int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM availability WHERE server_id=? AND policy_json=? AND recovery_json IS ? AND "+asRead,
			append([]any{server.ID, r.policy, r.recovery}, r.asRead()...)...).Scan(&same); err != nil {
			return err
		}
		if same != 1 {
			return errRecoveryChanged
		}
		var err error
		// The restart acts only on the container run judged hung.
		if job, err = s.Runner.Enqueue(ctx, tx, ops.EnqueueOptions{ServerID: server.ID, ActorID: ops.RecoveryActor, Kind: ops.KindRestart,
			BindingRevision: server.BindingRevision, Input: map[string]any{"run": container.StateSince, "answered": answered}}); err != nil {
			return err
		}
		state.Attempts = append(state.Attempts, now)
		state.OperationID, state.RestartedAt, state.Pending, state.Last, state.LastAt = job.ID, now, true, RecoveryRestarted, now
		saved = encode(state)
		if _, err = tx.ExecContext(ctx, "UPDATE availability SET recovery_json=? WHERE server_id=?", saved, server.ID); err != nil {
			return err
		}
		restarting = []alert{{"recovery:" + job.ID, server.DisplayName + ": the game stopped answering, so Ludock is restarting it.", nil}}
		return s.queueIn(ctx, tx, restarting)
	})
	if err != nil {
		if !errors.Is(err, errRecoveryChanged) && !apperr.Is(err, "OPERATION_CONFLICT") {
			log.Warn("Could not queue an automatic restart; it will be tried again")
		}
		return false
	}
	r.recovery = saved
	s.Runner.Wake()
	s.alerted(restarting)
	s.changed(server.ID)
	return true
}

// trackRecovery times the game's silence in the current container run, and
// settles the latest restart's outcome once its operation has ended: the game
// answered again, or it hasn't within RecoveryWait. A game reported as not
// answering is followed until it answers or its run ends.
func (s *Service) trackRecovery(ctx context.Context, server identity.Server, container servers.Container, running bool, game GameAnswer,
	r *row, policy Policy, now int64) {
	var state Recovery
	if err := decode(r.recovery, &state); err != nil {
		return
	}
	next := state
	// An answer between two checks ends the silence they saw.
	answeredSince := running && state.SilentSince != 0 && s.lastAnswer(server.ID, container) > state.SilentSince
	// A silence in this run timed before Ludock restarted went unwatched
	// meanwhile, so while its outage hasn't alerted it is timed again from
	// this process's first check, as that outage is.
	since := s.since.Load()
	unwatched := state.SilentSince != 0 && state.SilentSince < since && state.SilentRun == container.StateSince && r.notified == 0 &&
		!(state.Last == RecoveryGaveUp && state.LastAt >= state.SilentSince)
	if running && unwatched && (game == GameNotAnswering || game == GameUnknown) {
		next.SilentSince = since
	} else if running && game == GameNotAnswering && (state.SilentSince == 0 || state.SilentRun != container.StateSince || answeredSince) {
		next.SilentSince, next.SilentRun = now, container.StateSince
	} else if running && game == GameAnswering {
		next.SilentSince, next.SilentRun = 0, 0
	}
	message := ""
	if state.Pending {
		var status string
		var finished int64
		err := s.DB.QueryRowContext(ctx, "SELECT status,updated_at FROM operations WHERE id=?", state.OperationID).Scan(&status, &finished)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return
		}
		// The game's time to answer, and the pause, run from the restart's end.
		finished = max(finished, state.RestartedAt)
		newRun := running && container.StateSince > state.RestartedAt
		// Ludock's run started while its restart ran; a later one is someone
		// else's.
		othersRun := running && container.StateSince > finished
		answered := running && (game == GameAnswering || (game == GameNotAnswering && newRun))
		waited := now-finished >= RecoveryWait(policy)
		settled := true
		switch {
		case status == ops.Queued || status == ops.Running:
			settled = false
		case status == ops.Failed || (status == ops.Interrupted && !newRun):
			next.Last = RecoveryFailed
			message = server.DisplayName + ": Ludock couldn’t restart it after the game stopped answering. Its Activity says why."
		case status == ops.Skipped || status == ops.Cancelled:
			// It never acted, so it doesn't count as an attempt.
			next.Last = RecoverySkipped
			if n := len(next.Attempts); n > 0 && next.Attempts[n-1] == state.RestartedAt {
				next.Attempts = next.Attempts[:n-1]
			}
		case !running || othersRun || (state.Last == RecoveryNotAnswered && container.StateSince != state.WatchRun):
			// The run Ludock started ended; what happens next is someone else's.
		case answered:
			next.Last = RecoveryAnswered
			message = server.DisplayName + ": the game is answering again after Ludock restarted it."
		case running && state.Last == RecoveryNotAnswered:
			settled = false
		case !waited || (running && game == GameUnknown):
			// GameUnknown: no reading since the restart, such as just after
			// Ludock started.
			settled = false
		case running && game == GameWaiting:
			next.Last, next.LastAt, next.WatchRun = RecoveryNotAnswered, now, container.StateSince
			message = server.DisplayName + ": the game still isn’t answering after Ludock restarted it. Ludock won’t restart it again until the game answers; it needs a look."
			settled = false
		}
		// A server that stopped, or lost its status query, ends the wait quietly;
		// its outage alerts as usual.
		if settled {
			next.Pending, next.WatchRun = false, 0
			if next.Last != RecoveryRestarted && next.Last != RecoveryNotAnswered {
				next.LastAt = now
			}
			if n := len(next.Attempts); next.Last != RecoverySkipped && n > 0 && next.Attempts[n-1] == state.RestartedAt {
				next.Attempts[n-1], next.RestartedAt = finished, finished
			}
		}
	}
	if equalRecovery(next, state) {
		return
	}
	var alerts []alert
	if message != "" {
		alerts = []alert{{"recovery-" + next.Last + ":" + next.OperationID, message, nil}}
	}
	if !s.saveRecovery(ctx, server.ID, r, next, alerts...) {
		return
	}
	if next.Last != state.Last || next.Pending != state.Pending {
		s.changed(server.ID)
	}
}

func equalRecovery(a, b Recovery) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func (s *Service) lastAnswer(serverID string, container servers.Container) int64 {
	if s.LastAnswer == nil {
		return 0
	}
	return s.LastAnswer(serverID, container)
}
