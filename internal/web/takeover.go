package web

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/attention"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/jobs"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/monitor"
	"github.com/Crunging/Ludock/internal/schedules"
	"github.com/Crunging/Ludock/internal/web/views"
)

// sameGame: "unknown" is every unrecognized image, so it shows nothing.
func sameGame(a, b string) bool {
	return !strings.EqualFold(a, "unknown") && strings.EqualFold(games.Canonical(a), games.Canonical(b))
}

// sameData: the same game and data folders, kept in the same place. Servers
// without data folders share one fingerprint per game, which shows nothing.
func (s *Server) sameData(from, to *identity.Server) bool {
	none, err := s.App.Identity.BindingFingerprint(identity.Observation{GameType: from.GameType})
	return err == nil && from.BindingFingerprint != "" && from.BindingFingerprint != none && from.BindingFingerprint == to.BindingFingerprint
}

// takeOverOffer is what a missing server's page and Needs attention offer:
// the active server Ludock found after it with the same game and data
// folders, which is very likely it under a new name, or else the active
// servers of its game. It is nil when nothing is plausible.
func (s *Server) takeOverOffer(ctx context.Context, from *identity.Server, all []identity.Server) *views.TakeOver {
	if from == nil || from.Status != identity.StatusMissing {
		return nil
	}
	offer := &views.TakeOver{From: from.ID, FromName: from.DisplayName}
	var likely []views.TakeOverTarget
	for _, server := range all {
		if server.ID == from.ID || server.HostID != from.HostID || server.Status != identity.StatusActive || server.ContainerID == "" {
			continue
		}
		target := views.TakeOverTarget{ID: server.ID, Name: server.DisplayName, Revision: server.BindingRevision}
		if s.sameData(from, &server) && server.FirstSeenAt > from.FirstSeenAt {
			likely = append(likely, target)
		} else if sameGame(from.GameType, server.GameType) {
			offer.Others = append(offer.Others, target)
		}
	}
	if len(likely) == 1 {
		offer.Likely = &likely[0]
	} else {
		offer.Others = append(likely, offer.Others...)
	}
	if offer.Likely == nil && len(offer.Others) == 0 {
		return nil
	}
	err := s.App.DB.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM backups WHERE server_id=?1 AND state='complete'),
    (SELECT COUNT(*) FROM schedules WHERE server_id=?1), (SELECT COUNT(*) FROM server_grants WHERE server_id=?1)`, from.ID).
		Scan(&offer.Backups, &offer.Schedules, &offer.People)
	if err != nil {
		return nil
	}
	return offer
}

// takeOvers are the offers for Needs attention's missing servers.
func (s *Server) takeOvers(ctx context.Context, items []attention.Item) map[string]*views.TakeOver {
	all, err := s.App.Identity.List(ctx, "")
	if err != nil {
		return nil
	}
	offers := map[string]*views.TakeOver{}
	for _, item := range items {
		if index := slices.IndexFunc(all, func(server identity.Server) bool { return server.ID == item.ServerID }); index >= 0 && item.Kind == "binding" {
			if offer := s.takeOverOffer(ctx, &all[index], all); offer != nil && offer.Likely != nil {
				offers[item.ServerID] = offer
			}
		}
	}
	return offers
}

// takeOver moves a missing server's backups, schedules, sharing, monitoring
// choices, and history to the server an administrator says it now is, and
// then removes the missing one.
func (s *Server) takeOver(w http.ResponseWriter, r *http.Request, request *Request) {
	ctx := r.Context()
	from, to := r.PathValue("id"), r.PostFormValue("to")
	// A suggestion is for the target as the page showed it; a pick has none.
	revision, err := strconv.ParseInt(cmp.Or(r.PostFormValue("bindingRevision"), "-1"), 10, 64)
	if err != nil || !identity.ValidID(from) || !identity.ValidID(to) {
		s.fail(w, r, request, apperr.New("INVALID_REQUEST", 400, "Choose the server it is now."))
		return
	}
	// The move is for the servers as Docker has them now.
	if _, err := s.App.Servers.RefreshNow(ctx); err != nil {
		s.fail(w, r, request, err)
		return
	}
	release, err := s.App.Locks.AcquireYielding(ctx, []string{locks.ServerKey(from), locks.ServerKey(to)})
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	defer release()
	done, err := s.moveServer(ctx, request, from, to, revision)
	if err != nil {
		s.fail(w, r, request, authz.AdminMutationError(err))
		return
	}
	w.Header().Set("HX-Trigger", "servers-changed")
	if fromDetail(r) {
		render(w, r, http.StatusOK, views.TakeOverOnPage(*done))
		return
	}
	render(w, r, http.StatusOK, views.TakeOverResult(*done))
}

func errTakeOver(message string) error { return apperr.New("TAKE_OVER_REFUSED", 409, message) }

// scheduleKey is what makes two schedules the same: one of them is left
// behind rather than both running.
func scheduleKey(input schedules.Input) string {
	days := slices.Clone(input.Days)
	slices.Sort(days)
	return fmt.Sprint(input.Action, input.Kind, input.Enabled, input.Time, input.Timezone, input.Date, days, input.Command)
}

func (s *Server) moveServer(ctx context.Context, request *Request, fromID, toID string, expected int64) (*views.TakeOverDone, error) {
	admin := request.User().ID
	var done views.TakeOverDone
	err := s.App.DB.AdminMutation(ctx, admin, request.Principal.TokenHash, db.Now(), func(tx *sql.Tx) error {
		from, err := s.App.Identity.GetIn(ctx, tx, fromID)
		if err != nil {
			return err
		}
		to, err := s.App.Identity.GetIn(ctx, tx, toID)
		if err != nil {
			return err
		}
		switch {
		case from == nil || to == nil || from.ID == to.ID:
			return authz.ErrServerNotFound
		case from.Status != identity.StatusMissing:
			return errTakeOver("Ludock can find this server in Docker again, so there is nothing to move.")
		case to.Status != identity.StatusActive || to.ContainerID == "" || to.HostID != from.HostID:
			return errTakeOver(to.DisplayName + " isn’t in Docker as one container right now. Check it, then try again.")
		case expected >= 0 && to.BindingRevision != expected:
			return errTakeOver(to.DisplayName + " changed in Docker since this page loaded. Reload and check it, then try again.")
		case from.BindingFingerprint != to.BindingFingerprint && !sameGame(from.GameType, to.GameType):
			return errTakeOver(to.DisplayName + " runs another game, so this server’s backups can’t move there.")
		}
		if err := s.App.Authz.AssertIn(ctx, tx, admin, request.Principal.TokenHash, to.ID, authz.All...); err != nil {
			return err
		}
		var active, existing, moving int
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM operations WHERE server_id IN (?1,?2) AND status IN ('queued','running')),
      (SELECT COUNT(*) FROM schedules WHERE server_id=?2), (SELECT COUNT(*) FROM schedules WHERE server_id=?1)`, from.ID, to.ID).
			Scan(&active, &existing, &moving); err != nil {
			return err
		}
		if active > 0 {
			return errTakeOver("Wait for the work in progress to finish, then try again.")
		}
		done = views.TakeOverDone{FromName: from.DisplayName, ToID: to.ID, ToName: to.DisplayName,
			SameData: s.sameData(from, to)}
		// A restore that stopped partway is undone only on the binding it
		// stopped, so its journal can't move to other folders.
		var pending int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM operations WHERE server_id=? AND "+jobs.PendingRestore+
			" AND COALESCE(json_extract(recovery_json,'$.fingerprint'),'') <> ?", from.ID, to.BindingFingerprint).Scan(&pending); err != nil {
			return err
		}
		if pending > 0 {
			return errTakeOver("A restore of " + from.DisplayName + " stopped partway, and only its old data folders can finish it. " +
				"Bring back its old container and undo the restore from Needs attention, then try again.")
		}

		// A grant someone already has on the new server is theirs to keep.
		rows, err := tx.QueryContext(ctx, `SELECT u.username, EXISTS(SELECT 1 FROM server_grants t WHERE t.server_id=?2 AND t.user_id=g.user_id)
      FROM server_grants g JOIN users u ON u.id=g.user_id WHERE g.server_id=?1 ORDER BY u.username`, from.ID, to.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var name string
			var kept bool
			if err := rows.Scan(&name, &kept); err != nil {
				rows.Close()
				return err
			}
			if kept {
				done.KeptPeople = append(done.KeptPeople, name)
			} else {
				done.People++
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE server_grants SET server_id=?2
      WHERE server_id=?1 AND user_id NOT IN (SELECT user_id FROM server_grants WHERE server_id=?2)`, from.ID, to.ID); err != nil {
			return err
		}
		// Access given for other data folders, or for a change nobody
		// confirmed, waits for an administrator as a change in Docker does.
		// A new binding revision voids confirmations opened before the move.
		revision := to.BindingRevision
		if done.People > 0 && (!done.SameData || from.SharingPaused) {
			done.Paused, revision = true, revision+1
			if _, err := tx.ExecContext(ctx, "UPDATE logical_servers SET sharing_paused=1, binding_revision=? WHERE id=?", revision, to.ID); err != nil {
				return err
			}
		}

		taken := map[string]bool{}
		type entry struct {
			id    string
			input schedules.Input
		}
		var list []entry
		rows, err = tx.QueryContext(ctx, "SELECT id, server_id, input_json FROM schedules WHERE server_id IN (?,?) ORDER BY created_at, id", from.ID, to.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, server, raw string
			var input schedules.Input
			if err := rows.Scan(&id, &server, &raw); err != nil {
				rows.Close()
				return err
			}
			json.Unmarshal([]byte(raw), &input)
			if server == to.ID {
				taken[scheduleKey(input)] = true
			} else {
				list = append(list, entry{id, input})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, schedule := range list {
			if taken[scheduleKey(schedule.input)] {
				done.LeftSchedules = append(done.LeftSchedules, schedule.input)
				if _, err := tx.ExecContext(ctx, "DELETE FROM schedules WHERE id=?", schedule.id); err != nil {
					return err
				}
				continue
			}
			done.Schedules++
			taken[scheduleKey(schedule.input)] = true
		}
		if existing+done.Schedules > schedules.MaxPerServer {
			return errTakeOver(fmt.Sprintf("%s would have more than %d schedules. Delete some, then try again.", to.DisplayName, schedules.MaxPerServer))
		}
		// changed_at keeps a run missed while the server was missing from
		// starting late on the new one, and a run waiting for players ends.
		if _, err := tx.ExecContext(ctx, `UPDATE schedules SET server_id=?, binding_revision=?, changed_at=?, revision=revision+1,
      last_result=CASE WHEN 1=1`+schedules.StillWaiting+` THEN 'Ended: the server moved while it waited for players to leave' ELSE last_result END
      WHERE server_id=?`, to.ID, revision, db.Now(), from.ID); err != nil {
			return err
		}

		// Monitoring choices move only onto a server that still has the
		// defaults.
		policies := map[string]monitor.Policy{from.ID: monitor.DefaultPolicy(), to.ID: monitor.DefaultPolicy()}
		raw := map[string]string{}
		for id := range policies {
			var value string
			err := tx.QueryRowContext(ctx, "SELECT policy_json FROM availability WHERE server_id=?", id).Scan(&value)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if policies[id], err = monitor.ParsePolicy([]byte(value)); err != nil {
				return err
			}
			raw[id] = value
		}
		if policies[from.ID] != monitor.DefaultPolicy() {
			done.Monitoring = policies[to.ID] == monitor.DefaultPolicy()
			done.KeptMonitoring = !done.Monitoring
		}
		if done.Monitoring {
			if _, err := tx.ExecContext(ctx, `INSERT INTO availability(server_id,policy_json,updated_at) VALUES(?,?,?)
        ON CONFLICT(server_id) DO UPDATE SET policy_json=excluded.policy_json, updated_at=excluded.updated_at,
        outage_started_at=NULL, notified=0, disk_json=NULL, memory_json=NULL, recovery_json=NULL`, to.ID, raw[from.ID], db.Now()); err != nil {
				return err
			}
		}

		// Activity stays one story: a container change under one server
		// keeps its history too. The archives are named by backup ID, so
		// nothing on disk moves.
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM backups WHERE server_id=? AND state='complete'", from.ID).
			Scan(&done.Backups); err != nil {
			return err
		}
		for _, statement := range []string{
			"UPDATE backups SET server_id=?1 WHERE server_id=?2",
			"UPDATE operations SET server_id=?1 WHERE server_id=?2",
			"UPDATE audit_log SET target_id=?1 WHERE target_type='server' AND target_id=?2",
			// Startup removes an interrupted unpack's staging folder by its
			// server's ID.
			"UPDATE settings SET value_json=json_set(value_json,'$.server',?1) WHERE key LIKE 'unpack-stage:%' AND json_valid(value_json) AND json_extract(value_json,'$.server')=?2",
			"DELETE FROM availability WHERE server_id=?2",
			"DELETE FROM logical_servers WHERE id=?2",
		} {
			if _, err := tx.ExecContext(ctx, statement, to.ID, from.ID); err != nil {
				return err
			}
		}
		return s.App.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: admin, Action: "server.taken-over", TargetType: "server", TargetID: to.ID,
			IPAddress: request.Client.IP, Details: map[string]any{"from": from.DisplayName, "to": to.DisplayName, "backups": done.Backups,
				"schedules": done.Schedules, "people": done.People, "schedulesLeft": len(done.LeftSchedules), "peopleKept": len(done.KeptPeople),
				"sharingPaused": done.Paused}})
	})
	if err != nil {
		return nil, err
	}
	return &done, nil
}
