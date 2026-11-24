package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/monitor"
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/schedtime"
	"github.com/Crunging/Ludock/internal/schedules"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/web/views"
)

// The "Finish setting up" list on the server list. Each item is worked out
// from the product's state on every load; only dismissals are stored.

const checklistDismissedSetting = "checklist.dismissed"

var checklistKeys = []string{views.ChecklistAlerts, views.ChecklistBackups, views.ChecklistRecovery, views.ChecklistAddress,
	views.ChecklistServers, views.ChecklistTwoFactor}

// checklistState is what decides which items show.
type checklistState struct {
	// AlertsOn: a destination is on and hasn't only failed. Otherwise
	// Unproven is one that is on but has never delivered, and Reason why.
	AlertsOn       bool
	Unproven       notify.Destination
	UnprovenReason string
	// Unbacked names the servers a nightly backup would cover; of those,
	// UnbackedWarn can warn players and UnbackedWait can wait for them.
	// NoData are the IDs of servers it leaves out for having no data folders.
	Unbacked, UnbackedIDs, NoData []string
	UnbackedWarn, UnbackedWait    int
	// Unrecovered names the servers automatic restart could be turned on for.
	Unrecovered, UnrecoveredIDs []string
	// LocalHost is the loopback or private host players are shown, or "".
	LocalHost string
	// Unmanaged names containers that look like games Ludock doesn't manage.
	Unmanaged []string
	TwoFactor bool
	Dismissed []string
}

// alertsWorking reports whether a destination is on and hasn't only
// failed; if none is, it names one that is on but has never delivered.
func alertsWorking(configurations []notify.Configuration, healths map[notify.Destination]notify.Health) (bool, notify.Destination, string) {
	var unproven notify.Destination
	reason := ""
	for _, c := range configurations {
		if !c.Ready() {
			continue
		}
		h := healths[c.Destination]
		if !h.Failed || h.Delivered {
			return true, "", ""
		}
		if unproven == "" {
			unproven, reason = c.Destination, h.Reason
		}
	}
	return false, unproven, reason
}

func checklistItems(state checklistState) []views.ChecklistItem {
	var items []views.ChecklistItem
	add := func(item views.ChecklistItem) {
		if !slices.Contains(state.Dismissed, item.Key) {
			items = append(items, item)
		}
	}
	if !state.AlertsOn {
		add(views.ChecklistItem{Key: views.ChecklistAlerts, Destination: state.Unproven, Reason: state.UnprovenReason})
	}
	if len(state.Unbacked) > 0 {
		add(views.ChecklistItem{Key: views.ChecklistBackups, Names: state.Unbacked, IDs: state.UnbackedIDs,
			Warn: state.UnbackedWarn, Wait: state.UnbackedWait, NoData: state.NoData})
	}
	if len(state.Unrecovered) > 0 {
		add(views.ChecklistItem{Key: views.ChecklistRecovery, Names: state.Unrecovered, IDs: state.UnrecoveredIDs})
	}
	if state.LocalHost != "" {
		add(views.ChecklistItem{Key: views.ChecklistAddress, Host: state.LocalHost, Loopback: loopbackHost(state.LocalHost)})
	}
	if len(state.Unmanaged) > 0 {
		add(views.ChecklistItem{Key: views.ChecklistServers, Names: state.Unmanaged})
	}
	if !state.TwoFactor {
		add(views.ChecklistItem{Key: views.ChecklistTwoFactor})
	}
	return items
}

func loopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if address, err := netip.ParseAddr(host); err == nil {
		return address.Unmap().IsLoopback()
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

// localHost reports a host that only reaches players on the same computer or
// network: a loopback, private, or link-local address, or a name that only
// local networks resolve.
func localHost(host string) bool {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if address, err := netip.ParseAddr(host); err == nil {
		address = address.Unmap()
		return address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast() || address.IsUnspecified()
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	if !strings.Contains(host, ".") || loopbackHost(host) {
		return true
	}
	for _, suffix := range []string{".local", ".lan", ".home", ".home.arpa", ".internal"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// backupSlots are the nightly backup times, in the order they're used:
// quarter hours from 04:00 to 05:45, then five and ten minutes past those.
// Common daylight-saving changes happen between midnight and 03:00, so
// every slot exists and keeps its spacing on those nights.
var backupSlots = func() []string {
	var slots []string
	for offset := 0; offset < 15; offset += 5 {
		for quarter := range 8 {
			minute := 4*60 + quarter*15 + offset
			slots = append(slots, fmt.Sprintf("%02d:%02d", minute/60, minute%60))
		}
	}
	return slots
}()

// backupTimes picks count staggered times, skipping those existing backup
// schedules already use while any slot is free.
func backupTimes(taken []string, count int) []string {
	var times []string
	for _, slot := range backupSlots {
		if len(times) == count {
			return times
		}
		if !slices.Contains(taken, slot) {
			times = append(times, slot)
		}
	}
	for index := 0; len(times) < count; index++ {
		times = append(times, backupSlots[index%len(backupSlots)])
	}
	return times
}

// backsUp reports a recurring schedule that takes a backup.
func backsUp(schedule schedules.Schedule) bool {
	return schedule.Kind == "recurring" && (schedule.Action == "backup" || (schedule.Action == "restart" && schedule.CreateBackup))
}

type backupTarget struct {
	ID, Name string
	// Warn and Wait: a schedule can warn this server's players and wait
	// for them to leave.
	Warn, Wait bool
}

// backupTargets lists the servers that can be backed up, which a --rm one
// can't, and have no recurring backup, and the times existing recurring backups use.
func (s *Server) backupTargets(ctx context.Context, user *db.SessionUser, all []servers.View) ([]backupTarget, []string, error) {
	if settings, _, err := s.App.Backups.SavedSettings(ctx); err != nil || settings == nil {
		return nil, nil, err
	}
	var targets []backupTarget
	var taken []string
	for _, view := range all {
		list, err := s.App.Schedules.List(ctx, user, view.ID)
		if err != nil {
			return nil, nil, err
		}
		covered := false
		for _, schedule := range list {
			if backsUp(schedule) {
				covered = true
				taken = append(taken, schedule.Time)
			}
		}
		if !covered && view.BindingStatus == "active" && len(view.FileRoots) > 0 && !view.AutoRemove && view.Can(authz.BackupsCreate) {
			targets = append(targets, backupTarget{ID: view.ID, Name: view.DisplayName, Warn: canWarn(&view),
				Wait: s.App.Players.Source.Lists(view.Container)})
		}
	}
	return targets, taken, nil
}

// recoveryTargets lists the monitored servers, not paused for maintenance,
// whose game can say it is answering and that don't restart automatically yet.
func (s *Server) recoveryTargets(ctx context.Context, all []servers.View) ([]backupTarget, error) {
	var targets []backupTarget
	for _, view := range all {
		if view.BindingStatus != "active" || !monitor.RecoveryOffered(view.Container) {
			continue
		}
		availability, err := s.App.Monitor.Get(ctx, view.ID)
		if err != nil {
			return nil, err
		}
		if availability.Policy.Enabled && !availability.Policy.Maintenance && !availability.Policy.Recover {
			targets = append(targets, backupTarget{ID: view.ID, Name: view.DisplayName})
		}
	}
	return targets, nil
}

func (s *Server) checklistDismissed(ctx context.Context) []string {
	var dismissed []string
	if _, err := s.App.DB.Setting(ctx, checklistDismissedSetting, &dismissed); err != nil {
		return nil
	}
	return dismissed
}

// checklistView reads the state behind every item. A check that fails
// leaves its item out rather than showing one that may be wrong.
func (s *Server) checklistView(r *http.Request, request *Request) views.ChecklistView {
	ctx := r.Context()
	user := request.User()
	state := checklistState{TwoFactor: true, Dismissed: s.checklistDismissed(ctx)}
	if healths, err := s.App.Notify.Health(ctx); err == nil {
		state.AlertsOn, state.Unproven, state.UnprovenReason = alertsWorking(s.App.Notify.Configurations(ctx), healths)
	} else {
		state.AlertsOn = true
	}
	if all, err := s.App.Servers.List(ctx, user); err == nil {
		if targets, _, err := s.backupTargets(ctx, user, all); err == nil {
			for _, target := range targets {
				state.Unbacked = append(state.Unbacked, target.Name)
				state.UnbackedIDs = append(state.UnbackedIDs, target.ID)
				if target.Warn {
					state.UnbackedWarn++
				}
				if target.Wait {
					state.UnbackedWait++
				}
			}
			for _, view := range all {
				if view.BindingStatus == "active" && view.NoDataFolders && view.Can(authz.BackupsCreate) {
					state.NoData = append(state.NoData, view.ID)
				}
			}
		}
		if targets, err := s.recoveryTargets(ctx, all); err == nil {
			for _, target := range targets {
				state.Unrecovered = append(state.Unrecovered, target.Name)
				state.UnrecoveredIDs = append(state.UnrecoveredIDs, target.ID)
			}
		}
		// Players see the page's host until an address is saved.
		if host := pageHost(r); localHost(host) && slices.ContainsFunc(all, func(view servers.View) bool {
			return view.Connection != nil && view.Connection.Host == ""
		}) {
			state.LocalHost = host
		}
	}
	if !slices.Contains(state.Dismissed, views.ChecklistServers) {
		if containers, err := s.App.Observer.Unmanaged(ctx); err == nil {
			for _, container := range containers {
				if container.Game != "unknown" && !container.Chosen && !container.ControlsHost {
					state.Unmanaged = append(state.Unmanaged, container.Name)
				}
			}
		}
	}
	if record, err := s.App.DB.FindUserByID(ctx, user.ID); err == nil && record != nil {
		state.TwoFactor = record.TwoFactor
	}
	return views.ChecklistView{Items: checklistItems(state)}
}

func (s *Server) checklistFragment(w http.ResponseWriter, r *http.Request, request *Request) {
	v := s.checklistView(r, request)
	// Reading Docker takes a moment; the account may have changed meanwhile.
	s.renderAdmin(w, r, request, http.StatusOK, views.Checklist(v))
}

// One click backs up each server once, even when clicked twice.
var checklistBackups sync.Mutex

// scheduleNightlyBackups makes an ordinary nightly backup schedule, owned by
// the administrator who clicked, for each server that has none, as
// considerate to players as a new schedule.
func (s *Server) scheduleNightlyBackups(w http.ResponseWriter, r *http.Request, request *Request) {
	ctx := r.Context()
	timezone := strings.TrimSpace(r.PostFormValue("timezone"))
	if _, ok := schedtime.Location(timezone); !ok || timezone == "" {
		s.fail(w, r, request, apperr.New("INVALID_REQUEST", 400, "Unknown time zone"))
		return
	}
	if err := s.recheckAdmin(ctx, request); err != nil {
		s.fail(w, r, request, err)
		return
	}
	checklistBackups.Lock()
	defer checklistBackups.Unlock()
	all, err := s.App.Servers.List(ctx, request.User())
	var targets []backupTarget
	var taken []string
	if err == nil {
		targets, taken, err = s.backupTargets(ctx, request.User(), all)
	}
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	// Only the servers the confirmation named, which are still eligible.
	confirmed := r.PostForm["server"]
	targets = slices.DeleteFunc(targets, func(target backupTarget) bool { return !slices.Contains(confirmed, target.ID) })
	var scheduled []string
	var failure error
	for index, at := range backupTimes(taken, len(targets)) {
		input := schedules.Input{Kind: "recurring", Action: "backup", Enabled: true, Time: at, Timezone: timezone,
			Days: []int{0, 1, 2, 3, 4, 5, 6}}
		input.Considerate(targets[index].Warn, targets[index].Wait)
		// Create audits each schedule and rechecks the account; the session
		// is checked here, as it may end while earlier ones are made.
		if err := s.recheckAdmin(ctx, request); err != nil {
			s.fail(w, r, request, err)
			return
		}
		if _, err := s.App.Schedules.Create(ctx, request.User(), targets[index].ID, input); err != nil {
			if public, ok := apperr.As(err); !ok || public.Status == http.StatusUnauthorized || public.Status == http.StatusForbidden {
				s.fail(w, r, request, err)
				return
			}
			failure = err
			continue
		}
		scheduled = append(scheduled, targets[index].Name+" at "+at)
	}
	v := s.checklistView(r, request)
	if len(scheduled) > 0 {
		v.Notice = "Nightly backups scheduled: " + strings.Join(scheduled, ", ") + " (" + timezone + ")."
	}
	if failure != nil {
		v.Error = "Some backups couldn’t be scheduled: " + publicMessage(failure)
	}
	w.Header().Set("HX-Trigger", "schedules-changed")
	s.renderAdmin(w, r, request, http.StatusOK, views.Checklist(v))
}

// enableRecovery turns on automatic restart for each server the
// confirmation named that still has it off.
func (s *Server) enableRecovery(w http.ResponseWriter, r *http.Request, request *Request) {
	ctx := r.Context()
	if err := s.recheckAdmin(ctx, request); err != nil {
		s.fail(w, r, request, err)
		return
	}
	all, err := s.App.Servers.List(ctx, request.User())
	var targets []backupTarget
	if err == nil {
		targets, err = s.recoveryTargets(ctx, all)
	}
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	confirmed := r.PostForm["server"]
	var enabled []string
	for _, target := range targets {
		if !slices.Contains(confirmed, target.ID) {
			continue
		}
		if err := s.App.Monitor.SetRecovery(ctx, request.User().ID, request.Principal.TokenHash, target.ID, true); err != nil {
			s.fail(w, r, request, authz.AdminMutationError(err))
			return
		}
		s.audit(r, request, "server.recovery.enabled", target.ID, nil)
		enabled = append(enabled, target.Name)
	}
	v := s.checklistView(r, request)
	if len(enabled) > 0 {
		v.Notice = "Automatic restart is on for " + views.NameList(enabled) + "."
	}
	w.Header().Set("HX-Trigger", "availability-changed")
	s.renderAdmin(w, r, request, http.StatusOK, views.Checklist(v))
}

// dismissChecklistItem remembers that administrators don't want an item.
func (s *Server) dismissChecklistItem(w http.ResponseWriter, r *http.Request, request *Request) {
	ctx := r.Context()
	key := r.PathValue("item")
	if !slices.Contains(checklistKeys, key) {
		s.fail(w, r, request, apperr.New("NOT_FOUND", 404, "Not found"))
		return
	}
	err := s.App.DB.AdminMutation(ctx, request.User().ID, request.Principal.TokenHash, db.Now(), func(tx *sql.Tx) error {
		var dismissed []string
		value, found, err := db.RawSetting(ctx, tx, checklistDismissedSetting)
		if err != nil {
			return err
		}
		if found {
			// A damaged value is replaced.
			_ = json.Unmarshal([]byte(value), &dismissed)
		}
		if slices.Contains(dismissed, key) {
			return nil
		}
		dismissed = append(dismissed, key)
		if err := s.App.DB.SetSetting(ctx, tx, checklistDismissedSetting, dismissed); err != nil {
			return err
		}
		return s.App.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: request.User().ID, Action: "checklist.dismissed",
			TargetType: "settings", Details: map[string]string{"item": key}, IPAddress: request.Client.IP})
	})
	if err != nil {
		s.fail(w, r, request, authz.AdminMutationError(err))
		return
	}
	v := s.checklistView(r, request)
	s.renderAdmin(w, r, request, http.StatusOK, views.Checklist(v))
}
