package schedules

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/schedtime"
)

// announcedRun is the run of r that Announce counts players down to, and
// when it acts: the end of a waiting run's wait, or the next run.
func announcedRun(r *row, data Input, now int64) (string, int64) {
	if waiting(r) {
		if data.WaitForEmpty == nil {
			return "", 0
		}
		return r.lastSlot.String, waitEnds(r, data)
	}
	next, ok := schedtime.NextRun(data.Clock(), now, r.lastSlot.String)
	if !ok {
		return "", 0
	}
	runAt := next
	if data.WaitForEmpty != nil {
		runAt += int64(data.WaitForEmpty.Minutes) * 60_000
	}
	return schedtime.Slot(data.Clock(), next), runAt
}

// countdown is the run of r that players may be counting down to and that
// hasn't begun acting, and when it acts: one whose operation is still
// queued (queued), a due run not queued yet, or the announced run. q is the
// caller's transaction: a queued operation it sees is claimed only after
// the caller commits.
func countdown(ctx context.Context, q queryer, r *row, data Input, now int64) (slot string, runAt int64, queued bool, err error) {
	if r.lastOpID.Valid {
		if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE id=? AND status='queued')",
			r.lastOpID.String).Scan(&queued); err != nil {
			return "", 0, false, err
		}
		if queued {
			return r.lastSlot.String, 0, true, nil
		}
	}
	if !waiting(r) && data.Kind == "recurring" {
		if due, at := schedtime.DueSlot(data.Clock(), now, r.lastSlot.String, r.changedAt.Int64); due != "" {
			if data.WaitForEmpty != nil {
				at += int64(data.WaitForEmpty.Minutes) * 60_000
			}
			return due, at, false, nil
		}
	}
	slot, runAt = announcedRun(r, data, now)
	return slot, runAt, false, nil
}

type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// callOff spends the countdown claims of one run and reports the binding
// revision its latest delivered warning reached players on, or 0 when none
// did. Only the write that removes the claims tells players, so they hear
// it once; a later countdown to the same run starts over.
func callOff(ctx context.Context, q querier, scheduleID, slot string) (int64, error) {
	if slot == "" {
		return 0, nil
	}
	return spend(ctx, q, "DELETE FROM schedule_announcements WHERE schedule_id=? AND slot=? RETURNING lead_minutes,heard_binding",
		scheduleID, slot)
}

// CallOffQueued is callOff for the run that queued operationID.
func CallOffQueued(ctx context.Context, q querier, scheduleID, operationID string) (int64, error) {
	return spend(ctx, q, `DELETE FROM schedule_announcements WHERE schedule_id=?
    AND slot=(SELECT last_slot FROM schedules WHERE id=? AND last_operation_id=?) RETURNING lead_minutes,heard_binding`,
		scheduleID, scheduleID, operationID)
}

func spend(ctx context.Context, q querier, query string, args ...any) (int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	binding, latest := int64(0), 0
	for rows.Next() {
		var lead int
		var heard sql.NullInt64
		if err := rows.Scan(&lead, &heard); err != nil {
			return 0, err
		}
		// The shortest lead is the most recent warning.
		if heard.Valid && heard.Int64 != 0 && (binding == 0 || lead < latest) {
			binding, latest = heard.Int64, lead
		}
	}
	return binding, rows.Err()
}

// sayCalledOff tells players on serverID, at the binding revision they
// were warned on, that a countdown to action is off, best effort and in the
// background, so the change that called it off never waits on a busy server
// or its console.
func (s *Service) sayCalledOff(serverID, action string, binding int64) {
	if s.calledOff != nil {
		s.calledOff(serverID, action, binding)
		return
	}
	if s.Console == nil || s.Servers == nil {
		return
	}
	s.warnings.Add(1)
	go func() {
		defer s.warnings.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.tellCalledOff(ctx, serverID, action, binding); err != nil {
			log.Debug("Could not tell players a scheduled run is off", logging.F("serverId", serverID))
		}
	}()
}

// tellCalledOff runs on Ludock's own authority, like a warning's own
// call-off: it only takes back what Ludock told the players, in fixed words,
// and the schedule's owner may be the one who lost access. A server rebound
// since the warning is not the one players heard it on, so it is refused.
func (s *Service) tellCalledOff(ctx context.Context, serverID, action string, binding int64) error {
	allowed := func(context.Context) error { return nil }
	initial, err := s.Servers.ResolveChecked(ctx, serverID, binding, allowed)
	if err != nil {
		return err
	}
	release, err := s.Locks.AcquirePreempting(ctx, initial.LockKeys, nil)
	if err != nil {
		return err
	}
	defer release()
	current, err := s.Servers.ResolveChecked(ctx, serverID, binding, allowed)
	if err != nil {
		return err
	}
	// A server awaiting review after its game or data folders changed may
	// not be the one players were warned on.
	if current.Logical.SharingPaused || current.Container.State != "running" || current.Container.Console == nil ||
		!current.Container.Console.SupportsBroadcast {
		return nil
	}
	template := broadcastTemplate(current)
	if template == "" {
		return nil
	}
	return s.Console.Broadcast(ctx, current, strings.ReplaceAll(template, "{message}", CalledOffMessage(action)), func() error {
		_, err := s.Identity.AssertObservedBinding(ctx, serverID, current.Observation, binding)
		return err
	})
}
