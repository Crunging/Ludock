package schedules

import (
	"context"
	"database/sql"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/schedtime"
)

const (
	// playerCheckMs is how often a waiting run asks for a player reading,
	// which can start a console helper.
	playerCheckMs = 30_000
	// readingSettleMs is how long a reading asked for may take to land or
	// fail, including a console helper's setup.
	readingSettleMs = 30_000
)

var (
	errNoPlayerList = apperr.New("INVALID_REQUEST", 400, "Ludock can't read who is online on this server, so it can't wait for players to leave")
	errWaitMissed   = apperr.New("SCHEDULE_MISSED", 409, "Missed: Ludock was not running when the wait for players to leave ended")
	// Unlike players staying online, an unknown count may mean a broken
	// console, so it notifies.
	errWaitUnknown = apperr.New("SCHEDULE_PLAYERS_UNKNOWN", 409, "Skipped: Ludock couldn't tell whether players were online by the end of the wait")
	errWaitBusy    = apperr.New("OPERATION_CONFLICT", 409, "Other work kept the server busy until the wait ended")
)

const SkipPlayersOnline Skip = "Skipped: players were still online when the wait ended"

func (s *Service) assertListsPlayers(ctx context.Context, actor *db.SessionUser, serverID string, data Input) error {
	if data.WaitForEmpty == nil {
		return nil
	}
	if s.Players == nil {
		return errNoPlayerList
	}
	lists, err := s.Players.Lists(ctx, actor, serverID)
	if err == nil && !lists {
		err = errNoPlayerList
	}
	return err
}

func (s *Service) occupancy(ctx context.Context, user *db.SessionUser, serverID string, data Input, revision int64) (Occupancy, error) {
	if s.Players == nil {
		return Occupancy{}, nil
	}
	return s.Players.Online(ctx, user, serverID, data.Capabilities()[0], revision)
}

// startWaiting spends the slot on a wait that ends WaitForEmpty.Minutes
// after the scheduled minute.
func (s *Service) startWaiting(ctx context.Context, r *row, slot string, scheduledAt, now int64) error {
	result, err := s.DB.ExecContext(ctx, "UPDATE schedules SET last_slot=?,last_run_at=?,last_operation_id=NULL,last_result=? WHERE id=? AND revision=?",
		slot, scheduledAt, WaitingResult, r.id, r.revision)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err == nil && changed == 0 {
		return errScheduleChanged
	}
	s.checkPlayers(ctx, r.serverID, scheduledAt, now)
	return nil
}

// wait decides a run waiting for an empty server: it goes ahead once nobody
// is online, and when the wait ends with players online or uncounted, it
// skips or goes ahead as the schedule chose. Like a one-time run, a wait
// that ended over five minutes ago, while Ludock was down, is missed.
func (s *Service) wait(ctx context.Context, r *row, data Input, now int64) error {
	if !data.Enabled || data.WaitForEmpty == nil {
		return Skip("Ended: the schedule changed while it waited for players to leave")
	}
	ends := waitEnds(r, data)
	if now >= ends+schedtime.OneTimeGraceMs {
		return errWaitMissed
	}
	state, err := authorityState(ctx, s.DB, r.serverID, r.ownerID)
	if err != nil {
		return err
	}
	user, binding, err := s.authority(ctx, r, data)
	if err != nil {
		return err
	}
	occupancy, err := s.occupancy(ctx, user, r.serverID, data, binding.BindingRevision)
	if err != nil {
		return err
	}
	// An empty count counts only from a reading taken since the latest one
	// was asked for, once one asked for during this wait has had time to
	// land: an older reading, which a failed one leaves in place, can
	// predate someone joining.
	asked, settled := s.checkPlayers(ctx, r.serverID, r.lastRunAt.Int64, now)
	empty := occupancy.Stopped || (occupancy.Known && occupancy.Online == 0 && settled && occupancy.ReadAt >= asked)
	// A run that skips acts only before the wait ends, unless the server is
	// stopped: only other work held that run, and nobody is there to kick.
	switch {
	case now < ends && !empty:
		return nil
	case now < ends || data.WaitForEmpty.Then == WaitRun || occupancy.Stopped:
	case occupancy.Known && occupancy.Online > 0:
		return SkipPlayersOnline
	default:
		// Other work holding the server also stops its readings; the run
		// is recorded as blocked by it, like one on a stopped server.
		var busy bool
		if err := s.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE server_id=? AND status IN ('queued','running'))",
			r.serverID).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return errWaitBusy
		}
		return errWaitUnknown
	}
	err = s.queue(ctx, r, data, state, user, binding, r.lastSlot.String, now)
	// Other work on the server holds the run, which still waits.
	if apperr.Is(err, "OPERATION_CONFLICT") && now < ends+schedtime.OneTimeGraceMs-lastChance {
		return errRetryLater
	}
	return err
}

// checkPlayers asks for a reading of a waiting run's server at once if
// none was asked for since the wait began, then every playerCheckMs, since
// readings otherwise run only while someone watches. It reports whether a
// reading asked for since then has had readingSettleMs to land, and when
// the latest was asked for. Runs on one server share the asks.
func (s *Service) checkPlayers(ctx context.Context, serverID string, since, now int64) (int64, bool) {
	s.mu.Lock()
	if s.checked == nil {
		s.checked = map[string][2]int64{}
	}
	asks := s.checked[serverID]
	due := asks[0] < since || now-asks[0] >= playerCheckMs || now < asks[0]
	if due {
		asks = [2]int64{now, asks[0]}
		s.checked[serverID] = asks
	}
	s.mu.Unlock()
	if due && s.Players != nil {
		s.Players.Check(ctx, serverID)
	}
	settled := func(at int64) bool { return at >= since && now-at >= readingSettleMs }
	return asks[0], settled(asks[0]) || settled(asks[1])
}

var errWaitOver = apperr.New("SCHEDULE_WAIT_OVER", 409, "The wait already ended, so there’s nothing to cancel. The schedule shows what happened.")

// CanCancelWait reports whether actor may cancel a schedule's waiting run:
// whoever may start its action on the server, and administrators.
func (s *Service) CanCancelWait(ctx context.Context, actor *db.SessionUser, item *Schedule) bool {
	for _, capability := range append([]authz.Capability{authz.ServerView}, item.Capabilities()...) {
		if ok, err := s.Authz.Has(ctx, actor, item.ServerID, capability); err != nil || !ok {
			return false
		}
	}
	return item.WaitingUntil != 0
}

// CancelWait ends the run of slot waiting for players to leave without
// acting. Only a write that finds that run still waiting succeeds, as with
// the run's own end, so exactly one of them wins. The schedule keeps its
// next run. The session, role, and grants are checked again in the write.
func (s *Service) CancelWait(ctx context.Context, actor *db.SessionUser, tokenHash, serverID, id, slot string) error {
	state, err := authorityState(ctx, s.DB, serverID, actor.ID)
	if err != nil {
		return err
	}
	r, err := s.get(ctx, s.DB, id, serverID)
	if err != nil {
		return err
	}
	if _, err := s.Authz.Assert(ctx, actor, serverID, authz.ServerView); err != nil {
		return err
	}
	if r == nil {
		return errNotFound
	}
	data, err := ParseSaved(r.input)
	if err != nil {
		return err
	}
	for _, capability := range data.Capabilities() {
		if _, err := s.Authz.Assert(ctx, actor, serverID, capability); err != nil {
			return err
		}
	}
	if !waiting(r) || r.lastSlot.String != slot {
		return errWaitOver
	}
	var heard int64
	err = s.write(ctx, actor, tokenHash, actor.Role == db.RoleAdmin, state, serverID, []string{actor.ID}, func(tx *sql.Tx) error {
		now := db.Now()
		result, err := tx.ExecContext(ctx, "UPDATE schedules SET last_result=?,last_run_at=? WHERE id=? AND revision=? AND last_slot=?"+stillWaitingSQL,
			"Cancelled by "+actor.Username+" while it waited for players to leave", now, r.id, r.revision, slot)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed == 0 {
			return errWaitOver
		}
		if heard, err = callOff(ctx, tx, r.id, slot); err != nil {
			return err
		}
		return s.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: actor.ID, Action: "schedule.wait.cancelled", TargetType: "server",
			TargetID: serverID, Details: map[string]any{"scheduleId": id, "action": data.Action}})
	})
	if err != nil {
		return err
	}
	if err := s.DB.PruneAuditIfNeeded(ctx); err != nil {
		log.Warn("Audit retention cleanup failed after cancelling a schedule run; it will be retried")
	}
	if heard != 0 {
		s.sayCalledOff(serverID, data.Action, heard)
	}
	return nil
}
