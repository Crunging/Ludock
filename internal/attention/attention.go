// Package attention lists what needs a viewer's attention: unavailable
// bindings, outages, disk and memory alerts, suspended schedules, recent
// failed work, and, for administrators, alerts that aren't being delivered.
package attention

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sort"
	"strings"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/monitor"
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/schedules"
	"github.com/Crunging/Ludock/internal/textutil"
)

type Item struct {
	ID         string
	Kind       string // binding, sharing, availability, disk, memory, schedule, operation, restore, notifications
	ServerID   string
	ServerName string
	// binding
	BindingStatus identity.Status
	// availability
	State string
	// OutageStartedAt is when the outage began as Docker reports it.
	OutageStartedAt int64
	// CanStart: the server is stopped and the viewer may start it.
	CanStart bool
	// disk and memory; Path is empty for a viewer who can't read files.
	Path    string
	Percent float64
	Since   int64
	// schedule
	ScheduleID string
	Action     string
	Reason     string
	// operation
	OperationID   string
	OperationKind string
	Status        string
	UpdatedAt     int64
	ForceStopped  bool
	// Recovery: an automatic restart did the work.
	Recovery bool
	// Undone: a restore that ended without finishing had moved live data,
	// and its undo put all of the server's previous data back.
	Undone bool
	// UndoBlocked: a restore that stopped partway can't be undone until the
	// server's data folders and game are back as it found them.
	UndoBlocked bool
	// notifications, with Since and Reason
	Destination notify.Destination
}

type Result struct {
	Items []Item
	// DiscoveryUnavailable: persisted issues are shown, but live discovery
	// could not be refreshed.
	DiscoveryUnavailable bool
}

type Service struct {
	DB        *db.DB
	Authz     *authz.Service
	Identity  *identity.Store
	Monitor   *monitor.Service
	Schedules *schedules.Service
	// Notify, when set, raises destinations that are failing.
	Notify *notify.Service
	// Refresh reconciles discovery first.
	Refresh func(ctx context.Context) error
}

var priority = map[string]int{"notifications": 0, "binding": 0, "sharing": 0, "restore": 1, "availability": 1, "disk": 2, "memory": 2, "schedule": 3, "operation": 4}

// List reads current authority after discovery: a stale principal never
// exposes a revoked server's saved history.
func (s *Service) List(ctx context.Context, actor *db.SessionUser, now int64) (*Result, error) {
	unavailable := s.Refresh != nil && s.Refresh(ctx) != nil
	servers, err := s.Identity.List(ctx, "")
	if err != nil {
		return nil, err
	}
	result := &Result{Items: []Item{}}
	hasVisible := false
	current, visible, err := s.Authz.EffectiveAll(ctx, actor, servers)
	if err != nil {
		return nil, err
	}
	// An unreadable source fails the list instead of hiding its items.
	problems, err := s.Monitor.Problems(ctx, now)
	if err != nil {
		return nil, err
	}
	resources, err := s.Monitor.ResourceProblems(ctx)
	if err != nil {
		return nil, err
	}
	for _, server := range servers {
		permissions := visible[server.ID]
		if !slices.Contains(permissions, authz.ServerView) {
			continue
		}
		if testHook != nil {
			testHook("checked")
		}
		start := len(result.Items)
		base := Item{ServerID: server.ID, ServerName: server.DisplayName}
		if server.Status != identity.StatusActive {
			item := base
			item.ID, item.Kind, item.BindingStatus = "binding:"+server.ID, "binding", server.Status
			result.Items = append(result.Items, item)
		}
		if server.SharingPaused && current != nil && current.Role == db.RoleAdmin {
			item := base
			item.ID, item.Kind = "sharing:"+server.ID, "sharing"
			result.Items = append(result.Items, item)
		}
		if problem := problems[server.ID]; problem != nil {
			item := base
			item.ID, item.Kind, item.State, item.OutageStartedAt = "availability:"+server.ID, "availability", problem.State, problem.Since
			item.CanStart = server.Status == identity.StatusActive && slices.Contains(permissions, authz.ServerStart) &&
				(problem.State == "exited" || problem.State == "created")
			result.Items = append(result.Items, item)
		}
		if server.Status == identity.StatusActive {
			for _, resource := range resources[server.ID] {
				item := base
				item.ID, item.Kind, item.Percent, item.Since = resource.Kind+":"+server.ID+":"+resource.Path, resource.Kind, resource.Percent, resource.Since
				if slices.Contains(permissions, authz.FilesRead) {
					item.Path = resource.Path
				}
				result.Items = append(result.Items, item)
			}
		}
		if slices.Contains(permissions, authz.SchedulesManage) && s.Schedules != nil {
			list, err := s.Schedules.List(ctx, actor, server.ID)
			if _, public := apperr.As(err); err != nil && !public {
				return nil, err
			}
			// Access changed since the check above: losing schedules.manage
			// only hides the schedules, but losing the server hides it all.
			if err != nil {
				if visible, err := s.Authz.Has(ctx, actor, server.ID, authz.ServerView); err != nil {
					return nil, err
				} else if !visible {
					result.Items = result.Items[:start]
					continue
				}
			}
			for _, schedule := range list {
				if !schedule.Enabled || schedule.NextRunUnavailableReason == "" {
					continue
				}
				item := base
				item.ID, item.Kind, item.ScheduleID, item.Action, item.Reason = "schedule:"+schedule.ID, "schedule", schedule.ID,
					schedule.Action, schedule.NextRunUnavailableReason
				result.Items = append(result.Items, item)
			}
		}
		// A restore that stopped partway with live data moved has the
		// server's previous data out of place until it is undone, which only
		// an administrator can do.
		stranded := ""
		if current != nil && current.Role == db.RoleAdmin && server.Status == identity.StatusActive {
			item := base
			err := s.DB.QueryRowContext(ctx, `SELECT id,updated_at FROM operations WHERE server_id=? AND kind='restore' AND status IN ('failed','interrupted')
        AND json_valid(recovery_json) AND json_extract(recovery_json,'$.restore.status') IN ('replacing','rolling_back')
        AND EXISTS (SELECT 1 FROM json_each(recovery_json,'$.restore.roots') WHERE json_extract(value,'$.phase') NOT IN ('staging','rolled_back'))
        ORDER BY created_at DESC,id DESC LIMIT 1`, server.ID).Scan(&item.OperationID, &item.UpdatedAt)
			if err == nil {
				item.ID, item.Kind, item.OperationKind, stranded = "restore:"+server.ID, "restore", "restore", item.OperationID
				result.Items = append(result.Items, item)
			} else if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
		// Limit recent work before filtering issues; read summary fields only.
		// Work that failed is settled once the same kind of work succeeds
		// later, unless it may have left something only a person can sort
		// out: a force-stop, or work marked as needing an administrator. A
		// restore is settled by a later backup, restore or update too, but
		// only once its journal is final or if it never had one; one whose
		// undo found a stage folder gone may have left the wrong data, which
		// only a later restore settles. SQLite may evaluate both sides of AND,
		// so JSON is read only under CASE.
		forced := `COALESCE(CASE WHEN json_valid(result_json) THEN json_extract(result_json,'` + ops.PathForceStopped + `') = 1 END, 0)`
		needsAdministrator := `COALESCE(CASE WHEN json_valid(result_json) THEN json_extract(result_json,'` + ops.PathNeedsAdministrator + `') = 1 END, 0)`
		rows, err := s.DB.QueryContext(ctx, `SELECT id,kind,status,updated_at,actor_id=?,`+forced+`,
      kind='restore' AND COALESCE(CASE WHEN json_valid(recovery_json) THEN json_extract(recovery_json,'$.restore.status') = 'rolled_back' AND
        json_extract(recovery_json,'$.restore.putBack') = 1 END, 0)
      FROM (SELECT id,kind,status,updated_at,actor_id,result_json,recovery_json,created_at,error FROM operations WHERE server_id=? ORDER BY created_at DESC,id DESC LIMIT 100) o
      WHERE (status IN ('failed','interrupted') AND (`+needsAdministrator+` OR `+forced+` OR
        (kind='restore' AND NOT COALESCE(CASE WHEN json_valid(recovery_json) THEN json_type(recovery_json,'$.restore') IS NULL OR
          json_extract(recovery_json,'$.restore.status') IN ('rolled_back','cleaned') END, 0)) OR
        NOT EXISTS (SELECT 1 FROM operations later WHERE later.server_id=? AND later.status='succeeded' AND
          (later.kind=o.kind OR (o.kind='restore' AND later.kind IN ('backup','update') AND
            NOT COALESCE(CASE WHEN json_valid(o.recovery_json) THEN json_extract(o.recovery_json,'$.restore.stageMissing') = 1 END, 0))) AND
          (later.created_at > o.created_at OR (later.created_at = o.created_at AND later.id > o.id)))))
        OR (status='succeeded' AND kind IN ('stop','restart') AND `+forced+`)`,
			ops.RecoveryActor, server.ID, server.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			item := base
			if err := rows.Scan(&item.OperationID, &item.OperationKind, &item.Status, &item.UpdatedAt, &item.Recovery, &item.ForceStopped, &item.Undone); err != nil {
				rows.Close()
				return nil, err
			}
			item.ID, item.Kind = "operation:"+item.OperationID, "operation"
			if item.OperationID != stranded {
				result.Items = append(result.Items, item)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		hasVisible = true
	}
	if s.Notify != nil && current != nil && current.Role == db.RoleAdmin {
		problems, err := s.Notify.Problems(ctx)
		if err != nil {
			return nil, err
		}
		for _, problem := range problems {
			result.Items = append(result.Items, Item{ID: "notifications:" + string(problem.Destination), Kind: "notifications",
				Destination: problem.Destination, Since: problem.LastTried, Reason: problem.Reason})
		}
	}
	sort.SliceStable(result.Items, func(i, j int) bool {
		left, right := result.Items[i], result.Items[j]
		if priority[left.Kind] != priority[right.Kind] {
			return priority[left.Kind] < priority[right.Kind]
		}
		if left.Kind == "operation" && left.UpdatedAt != right.UpdatedAt {
			return left.UpdatedAt > right.UpdatedAt
		}
		if order := textutil.Compare(left.ServerName, right.ServerName); order != 0 {
			return order < 0
		}
		return strings.Compare(left.ID, right.ID) < 0
	})
	if unavailable {
		current, _ := s.Authz.CurrentActor(ctx, actor)
		result.DiscoveryUnavailable = hasVisible || (current != nil && current.Role == db.RoleAdmin)
	}
	return result, nil
}

// testHook, set only by tests, runs at named points in List so a test can
// force an interleaving.
var testHook func(point string)
