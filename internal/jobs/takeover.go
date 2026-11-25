package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/monitor"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/schedtime"
	"github.com/Crunging/Ludock/internal/schedules"
	"github.com/Crunging/Ludock/internal/servers"
)

// TakeOver is what moved, and what stayed behind because the new server
// already had its own.
type TakeOver struct {
	FromName, ToID, ToName     string
	Backups, Schedules, People int
	// SameData: the same game and data folders, kept in the same place.
	SameData                   bool
	Monitoring, KeptMonitoring bool
	LeftSchedules              []schedules.Input
	// SameTime are moved schedules that run when one of the new server's own
	// does something else; both are kept.
	SameTime   []schedules.Input
	KeptPeople []string
	// Paused: the moved sharing waits for an administrator to confirm.
	Paused bool
}

// SameGame: "unknown" is every unrecognized image, so it shows nothing.
func SameGame(a, b string) bool {
	return !strings.EqualFold(a, "unknown") && strings.EqualFold(games.Canonical(a), games.Canonical(b))
}

// SameData: the same game and data folders, kept in the same place. Servers
// without data folders share one fingerprint per game, which shows nothing.
func (j *Jobs) SameData(from, to *identity.Server) bool {
	none, err := j.Identity.BindingFingerprint(identity.Observation{GameType: from.GameType})
	return err == nil && from.BindingFingerprint != "" && from.BindingFingerprint != none && from.BindingFingerprint == to.BindingFingerprint
}

// adoptRestores rebinds each restore of from that stopped partway to the
// target's container and locks, so startup and the next backup finish it
// there. Only a journal for the target's binding and exactly its data
// folders moves: recovery undoes a restore only on the data it stopped.
func adoptRestores(ctx context.Context, tx *sql.Tx, from *identity.Server, target *servers.Context) error {
	rows, err := tx.QueryContext(ctx, "SELECT id, server_id, recovery_json FROM operations WHERE server_id IN (?,?) AND "+PendingRestore,
		from.ID, target.Logical.ID)
	if err != nil {
		return err
	}
	raw := map[string]map[string]string{from.ID: {}, target.Logical.ID: {}}
	for rows.Next() {
		var id, server, value string
		if err := rows.Scan(&id, &server, &value); err != nil {
			rows.Close()
			return err
		}
		raw[server][id] = value
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(raw[from.ID]) == 0 {
		return err
	}
	saved, theirs := map[string]ops.Recovery{}, map[string]ops.Recovery{}
	for server, decoded := range map[string]map[string]ops.Recovery{from.ID: saved, target.Logical.ID: theirs} {
		for id, value := range raw[server] {
			var recovery ops.Recovery
			if json.Unmarshal([]byte(value), &recovery) != nil {
				return errJournal
			}
			decoded[id] = recovery
		}
	}
	fits := func(recovery ops.Recovery) bool {
		state, err := restoreState(&ops.Context{Job: &ops.Job{Recovery: recovery}})
		return err == nil && state != nil && recovery.Fingerprint == target.Logical.BindingFingerprint && journalMatches(state.Roots, target)
	}
	refused := errTakeOver("A restore of " + from.DisplayName + " stopped partway, and only its old data folders can finish it. " +
		"Bring back its old container and back it up once, which finishes the restore, then try again.")
	// Recovery finishes a server's restores in turn, so one of the target's
	// own that can't be finished there would hold back the moved ones.
	for _, recovery := range theirs {
		if !fits(recovery) || recovery.ContainerID != target.Logical.ContainerID {
			return errTakeOver("A restore of " + target.Logical.DisplayName + " stopped partway and can’t be finished on it as it is now. " +
				"Sort that out first, then try again.")
		}
	}
	for id, recovery := range saved {
		if !fits(recovery) {
			return refused
		}
		recovery.ContainerID, recovery.LockKeys = target.Logical.ContainerID, target.LockKeys
		encoded, err := json.Marshal(recovery)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE operations SET recovery_json=? WHERE id=?", string(encoded), id); err != nil {
			return err
		}
	}
	return nil
}

func errTakeOver(message string) error { return apperr.New("TAKE_OVER_REFUSED", 409, message) }

// sameTime reports whether two enabled schedules run at the same instant,
// whatever time zones they name: a one-time schedule's run against the
// other's, or two recurring ones on any of the next runs of the first, now
// and half a year on, which reach every weekday they share in both seasons.
func sameTime(a, b schedules.Input, now int64) bool {
	if !a.Enabled || !b.Enabled {
		return false
	}
	runsAt := func(input schedules.Input, at int64) bool {
		next, ok := schedtime.NextRun(input.Clock(), at, "")
		return ok && next == at
	}
	for _, pair := range [][2]schedules.Input{{a, b}, {b, a}} {
		if pair[0].Kind == "once" {
			at, ok := schedtime.NextRun(pair[0].Clock(), now, "")
			return ok && runsAt(pair[1], at)
		}
	}
	// Zones that agree in one half of the year can differ in the other.
	for _, from := range []int64{now, now + 26*7*24*time.Hour.Milliseconds()} {
		for at, n := from, 0; n < 8; n++ {
			next, ok := schedtime.NextRun(a.Clock(), at, "")
			if !ok {
				break
			}
			if runsAt(b, next) {
				return true
			}
			at = next + 60_000
		}
	}
	return false
}

// TakeOver moves a missing server's backups, schedules, sharing, monitoring
// choices, and history to the server an administrator says it now is, and
// then removes the missing one. expected is the target's binding revision
// the administrator saw, or -1 when they picked it from a list.
func (j *Jobs) TakeOver(ctx context.Context, actor *db.SessionUser, tokenHash, fromID, toID string, expected int64, ip string) (*TakeOver, error) {
	// The move is for the servers as Docker has them now.
	target, err := j.Servers.ResolveChecked(ctx, toID, -1, func(context.Context) error { return nil })
	if err != nil {
		return nil, err
	}
	release, err := j.Locks.AcquireYielding(ctx, []string{locks.ServerKey(fromID), locks.ServerKey(toID)})
	if err != nil {
		return nil, err
	}
	defer release()
	admin := actor.ID
	var done TakeOver
	err = j.DB.AdminMutation(ctx, admin, tokenHash, db.Now(), func(tx *sql.Tx) error {
		from, err := j.Identity.GetIn(ctx, tx, fromID)
		if err != nil {
			return err
		}
		to, err := j.Identity.GetIn(ctx, tx, toID)
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
		case from.BindingFingerprint != to.BindingFingerprint && !SameGame(from.GameType, to.GameType):
			return errTakeOver(to.DisplayName + " runs another game, so this server’s backups can’t move there.")
		}
		if err := j.Authz.AssertIn(ctx, tx, admin, tokenHash, to.ID, authz.All...); err != nil {
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
		done = TakeOver{FromName: from.DisplayName, ToID: to.ID, ToName: to.DisplayName,
			SameData: j.SameData(from, to)}
		// The target resolved before the transaction is the one it holds now.
		if _, err := j.Identity.AssertObservedBindingIn(ctx, tx, to.ID, target.Observation, target.Logical.BindingRevision); err != nil {
			return err
		}
		if err := adoptRestores(ctx, tx, from, target); err != nil {
			return err
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
		// Other data folders, or moved access for a change nobody confirmed,
		// wait for an administrator as a change in Docker does: its members
		// would see the other folders' history. A new binding revision voids
		// confirmations opened before the move.
		var shared bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM server_grants WHERE server_id=?)", to.ID).Scan(&shared); err != nil {
			return err
		}
		revision := to.BindingRevision
		if shared && (!done.SameData || (done.People > 0 && from.SharingPaused)) {
			done.Paused, revision = true, revision+1
			if _, err := tx.ExecContext(ctx, "UPDATE logical_servers SET sharing_paused=1, binding_revision=? WHERE id=?", revision, to.ID); err != nil {
				return err
			}
		}

		// Only a schedule that does exactly what one already there does is
		// left behind. A saved schedule that doesn't parse is never compared.
		taken := map[string]bool{}
		type entry struct {
			id    string
			input schedules.Input
			valid bool
		}
		var list, theirs []entry
		rows, err = tx.QueryContext(ctx, "SELECT id, server_id, input_json FROM schedules WHERE server_id IN (?,?) ORDER BY created_at, id", from.ID, to.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, server, raw string
			if err := rows.Scan(&id, &server, &raw); err != nil {
				rows.Close()
				return err
			}
			input, err := schedules.ParseSaved(raw)
			if server == to.ID {
				theirs = append(theirs, entry{id, input, err == nil})
			} else {
				list = append(list, entry{id, input, err == nil})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, schedule := range theirs {
			if schedule.valid {
				taken[schedule.input.Key()] = true
			}
		}
		for _, schedule := range list {
			if schedule.valid && taken[schedule.input.Key()] {
				done.LeftSchedules = append(done.LeftSchedules, schedule.input)
				if _, err := tx.ExecContext(ctx, "DELETE FROM schedules WHERE id=?", schedule.id); err != nil {
					return err
				}
				continue
			}
			done.Schedules++
			if !schedule.valid {
				continue
			}
			taken[schedule.input.Key()] = true
			if slices.ContainsFunc(theirs, func(their entry) bool { return their.valid && sameTime(their.input, schedule.input, db.Now()) }) {
				done.SameTime = append(done.SameTime, schedule.input)
			}
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
		// Startup removes an interrupted unpack's staging folder by its
		// server's ID, which only the same storage holds.
		if done.SameData {
			if _, err := tx.ExecContext(ctx, `UPDATE settings SET value_json=json_set(value_json,'$.server',?1)
        WHERE key LIKE 'unpack-stage:%' AND json_valid(value_json) AND json_extract(value_json,'$.server')=?2`, to.ID, from.ID); err != nil {
				return err
			}
		}
		for _, statement := range []string{
			"UPDATE backups SET server_id=?1 WHERE server_id=?2",
			"UPDATE operations SET server_id=?1 WHERE server_id=?2",
			"UPDATE audit_log SET target_id=?1 WHERE target_type='server' AND target_id=?2",
			"DELETE FROM availability WHERE server_id=?2",
			"DELETE FROM logical_servers WHERE id=?2",
		} {
			if _, err := tx.ExecContext(ctx, statement, to.ID, from.ID); err != nil {
				return err
			}
		}
		return j.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: admin, Action: "server.taken-over", TargetType: "server", TargetID: to.ID,
			IPAddress: ip, Details: map[string]any{"from": from.DisplayName, "to": to.DisplayName, "backups": done.Backups,
				"schedules": done.Schedules, "people": done.People, "schedulesLeft": len(done.LeftSchedules), "peopleKept": len(done.KeptPeople),
				"sharingPaused": done.Paused}})
	})
	if err != nil {
		return nil, err
	}
	return &done, nil
}
