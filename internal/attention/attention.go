// Package attention lists what needs a viewer's attention: unavailable
// bindings, outages, suspended schedules, and recent failed work.
package attention

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/monitor"
	"github.com/Crunging/Ludock/internal/schedules"
	"github.com/Crunging/Ludock/internal/textutil"
)

// Item is one attention entry.
type Item struct {
	ID         string
	Kind       string // binding, availability, schedule, operation
	ServerID   string
	ServerName string
	// binding
	BindingStatus identity.Status
	// availability
	State           string
	OutageStartedAt int64
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
}

// Result is the attention list.
type Result struct {
	Items []Item
	// DiscoveryUnavailable: persisted issues are shown, but live discovery
	// could not be refreshed.
	DiscoveryUnavailable bool
}

// Service builds attention lists.
type Service struct {
	DB        *db.DB
	Authz     *authz.Service
	Identity  *identity.Store
	Monitor   *monitor.Service
	Schedules *schedules.Service
	// Refresh reconciles discovery first.
	Refresh func(ctx context.Context) error
}

var priority = map[string]int{"binding": 0, "availability": 1, "schedule": 2, "operation": 3}

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
	for _, server := range servers {
		permissions, err := s.Authz.Effective(ctx, actor, server.ID)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(permissions, authz.ServerView) {
			continue
		}
		hasVisible = true
		base := Item{ServerID: server.ID, ServerName: server.DisplayName}
		if server.Status != identity.StatusActive {
			item := base
			item.ID, item.Kind, item.BindingStatus = "binding:"+server.ID, "binding", server.Status
			result.Items = append(result.Items, item)
		}
		if problem, err := s.Monitor.Problem(ctx, server.ID, now); err == nil && problem != nil {
			item := base
			item.ID, item.Kind, item.State, item.OutageStartedAt = "availability:"+server.ID, "availability", problem.State, problem.OutageStartedAt
			result.Items = append(result.Items, item)
		}
		if slices.Contains(permissions, authz.SchedulesManage) && s.Schedules != nil {
			list, err := s.Schedules.List(ctx, actor, server.ID)
			if err == nil {
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
		}
		// Limit recent work before filtering issues; read summary fields only.
		rows, err := s.DB.QueryContext(ctx, `SELECT id,kind,status,updated_at,
      CASE WHEN json_valid(result_json) THEN json_extract(result_json,'$.forceStopped') = 1 ELSE 0 END
      FROM (SELECT id,kind,status,updated_at,result_json FROM operations WHERE server_id=? ORDER BY created_at DESC,id DESC LIMIT 100)
      WHERE status IN ('failed','interrupted') OR (status='succeeded' AND kind IN ('stop','restart') AND
        CASE WHEN json_valid(result_json) THEN json_extract(result_json,'$.forceStopped') = 1 ELSE 0 END)`, server.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			item := base
			var forced int
			if err := rows.Scan(&item.OperationID, &item.OperationKind, &item.Status, &item.UpdatedAt, &forced); err != nil {
				rows.Close()
				return nil, err
			}
			item.ID, item.Kind, item.ForceStopped = "operation:"+item.OperationID, "operation", forced == 1
			result.Items = append(result.Items, item)
		}
		rows.Close()
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
