package schedules

import (
	"context"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/schedtime"
)

const (
	// playerCheckMs is how often a waiting run asks for a player reading,
	// which can start a console helper.
	playerCheckMs = 30_000
	// readingSettleMs is how long a reading asked for may take to land.
	readingSettleMs = 10_000
)

var (
	errNoPlayerList = apperr.New("INVALID_REQUEST", 400, "Ludock can't read who is online on this server, so it can't wait for players to leave")
	errWaitMissed   = apperr.New("SCHEDULE_MISSED", 409, "Missed: Ludock was not running when the wait for players to leave ended")
	// Unlike players staying online, an unknown count may mean a broken
	// console, so it notifies.
	errWaitUnknown = apperr.New("SCHEDULE_PLAYERS_UNKNOWN", 409, "Skipped: Ludock couldn't tell whether players were online by the end of the wait")
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

// empty reports whether the server is known to have nobody online, and
// whether its count is known at all.
func (s *Service) empty(ctx context.Context, user *db.SessionUser, serverID string, data Input, revision int64) (bool, bool, error) {
	if s.Players == nil {
		return false, false, nil
	}
	online, known, err := s.Players.Online(ctx, user, serverID, data.Capabilities()[0], revision)
	return known && online == 0, known, err
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
	user, binding, err := s.authority(ctx, s.DB, r, data)
	if err != nil {
		return err
	}
	empty, known, err := s.empty(ctx, user, r.serverID, data, binding.BindingRevision)
	if err != nil {
		return err
	}
	// An empty count counts only from a reading asked for during this wait,
	// so it can't predate someone joining.
	asked := s.checkPlayers(ctx, r.serverID, r.lastRunAt.Int64, now)
	if empty && (asked < r.lastRunAt.Int64 || now-asked < readingSettleMs) {
		return nil
	}
	if !empty {
		switch {
		case now < ends:
			return nil
		case data.WaitForEmpty.Then == WaitSkip && known:
			return SkipPlayersOnline
		case data.WaitForEmpty.Then == WaitSkip:
			return errWaitUnknown
		}
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
// readings otherwise run only while someone watches. It returns when the
// previous reading was asked for.
func (s *Service) checkPlayers(ctx context.Context, serverID string, since, now int64) int64 {
	s.mu.Lock()
	if s.checked == nil {
		s.checked = map[string]int64{}
	}
	last := s.checked[serverID]
	due := last < since || now-last >= playerCheckMs || now < last
	if due {
		s.checked[serverID] = now
	}
	s.mu.Unlock()
	if due && s.Players != nil {
		s.Players.Check(ctx, serverID)
	}
	return last
}
