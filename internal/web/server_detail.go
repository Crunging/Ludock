package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/history"
	"github.com/Crunging/Ludock/internal/monitor"
	"github.com/Crunging/Ludock/internal/web/views"
)

// detail reads one server for its detail page. Tabs follow the viewer's
// capabilities; an unknown or hidden tab shows the overview. Only the
// overview shows resource statistics, which take a Docker sample to read.
func (s *Server) detail(r *http.Request, request *Request, id, tab string, withStats bool) (*views.ServerDetail, error) {
	ctx := r.Context()
	read := s.App.Servers.Summary
	if withStats {
		read = s.App.Servers.Snapshot
	}
	snapshot, err := read(ctx, request.User(), id)
	if err != nil {
		return nil, err
	}
	view := snapshot.Server
	admin := request.User().Role == db.RoleAdmin
	d := &views.ServerDetail{Row: s.serverRow(r, request, *view, false, snapshot.DiscoveryUnavailable), Stats: snapshot.Stats,
		DiscoveryUnavailable: snapshot.DiscoveryUnavailable, Admin: admin, Now: db.Now()}
	manageSchedules := can(view, authz.SchedulesManage) || (admin && view.Can(authz.ServerView))
	d.Tabs = []views.DetailTab{{ID: "overview", Label: "Overview"}, {ID: "activity", Label: "Activity"}}
	if admin || can(view, authz.BackupsCreate) {
		d.Tabs = append(d.Tabs, views.DetailTab{ID: "backups", Label: "Backups"})
	}
	if manageSchedules {
		d.Tabs = append(d.Tabs, views.DetailTab{ID: "schedules", Label: "Schedules"})
	}
	if admin || can(view, authz.ServerRestart) {
		d.Tabs = append(d.Tabs, views.DetailTab{ID: "update", Label: "Update"})
	}
	d.Tab = "overview"
	if slices.ContainsFunc(d.Tabs, func(item views.DetailTab) bool { return item.ID == tab }) {
		d.Tab = tab
	}
	return d, nil
}

// overview adds the overview's monitoring and next schedule.
func (s *Server) overview(r *http.Request, request *Request, d *views.ServerDetail) error {
	ctx := r.Context()
	id := d.Row.Server.ID
	availability, err := s.App.Monitor.Get(ctx, id)
	if err != nil {
		return err
	}
	d.Availability = availability
	if slices.ContainsFunc(d.Tabs, func(tab views.DetailTab) bool { return tab.ID == "schedules" }) {
		d.ShowSchedule = true
		if list, err := s.App.Schedules.List(ctx, request.User(), id); err == nil {
			d.SchedulesReady = true
			for index := range list {
				item := &list[index]
				if item.Enabled && item.NextRunAt != 0 && (d.NextSchedule == nil || item.NextRunAt < d.NextSchedule.NextRunAt) {
					d.NextSchedule = item
				}
			}
		}
	}
	return nil
}

// activity reads the recent operations of a server, filtered in the page.
func (s *Server) activity(r *http.Request, request *Request, id string, query url.Values) views.ActivityView {
	a := views.ActivityView{ServerID: id, Status: query.Get("status"), Kind: query.Get("kind"), SelectedID: query.Get("operation"),
		Admin: request.User().Role == db.RoleAdmin, Tab: "activity"}
	operations, _, err := s.App.History.Operations(r.Context(), request.User(), history.Query{ServerID: id, From: -1, To: -1})
	if err != nil {
		a.Error = publicMessage(err)
	}
	a.Total = len(operations)
	kinds := map[string]bool{}
	if a.Kind != "" {
		kinds[a.Kind] = true
	}
	for _, operation := range operations {
		kinds[operation.Kind] = true
		active := operation.Status == "queued" || operation.Status == "running"
		a.Active = a.Active || active
		if operation.Kind == "update" && operation.Status == "already_current" {
			a.AlreadyCurrent = true
		}
		if (a.Status == "" || operation.Status == a.Status) && (a.Kind == "" || operation.Kind == a.Kind) {
			a.Visible = append(a.Visible, operation)
		} else if active {
			a.HiddenActive = true
		}
	}
	for kind := range kinds {
		a.Kinds = append(a.Kinds, kind)
	}
	slices.Sort(a.Kinds)
	if a.SelectedID != "" {
		s.selectOperation(r, &a)
	}
	return a
}

// detailTabs are the tabs other than the overview.
var detailTabs = []string{"activity", "backups", "schedules", "update"}

func (s *Server) serverDetail(w http.ResponseWriter, r *http.Request, request *Request) {
	tab := r.URL.Query().Get("tab")
	d, err := s.detail(r, request, r.PathValue("id"), tab, !slices.Contains(detailTabs, tab))
	if err == nil && d.Tab != tab && r.URL.Query().Get("operation") != "" {
		// Work linked to a tab the viewer can't open shows in Activity.
		tab = "activity"
		d, err = s.detail(r, request, r.PathValue("id"), tab, false)
	}
	if err == nil && d.Tab == "overview" && slices.Contains(detailTabs, tab) {
		// A hidden tab falls back to the overview, which shows statistics.
		d, err = s.detail(r, request, r.PathValue("id"), "overview", true)
	}
	if err == nil {
		switch d.Tab {
		case "activity":
			d.Activity = s.activity(r, request, d.Row.Server.ID, r.URL.Query())
			d.Activity.Archives = d.Admin && can(&d.Row.Server, authz.BackupsRead)
		case "backups":
			d.Backups = s.backupsView(r, request, d)
			d.Operation = d.Backups.Operation.SelectedID
		case "schedules":
			d.Schedules = s.schedulesView(r, request, d)
		case "update":
			d.Recreate = r.URL.Query().Get("recreate") == "1"
		case "overview":
			err = s.overview(r, request, d)
		}
	}
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	if d.Operation == "" {
		d.Operation = r.URL.Query().Get("operation")
	}
	page := s.page(r, request, d.Row.Server.DisplayName)
	page.Path = "/"
	render(w, r, http.StatusOK, views.ServerDetailPage(page, *d))
}

func (s *Server) detailLive(w http.ResponseWriter, r *http.Request, request *Request) {
	d, err := s.detail(r, request, r.PathValue("id"), r.URL.Query().Get("tab"), false)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	d.Operation = r.URL.Query().Get("operation")
	// The work this header last showed has ended: tabs paused by it, such
	// as Backups, catch up.
	if was := r.URL.Query().Get("active"); was != "" && (d.Row.Server.ActiveOperation == nil || d.Row.Server.ActiveOperation.ID != was) {
		w.Header().Set("HX-Trigger", "operation-finished")
	}
	render(w, r, http.StatusOK, views.DetailLive(*d))
}

func (s *Server) detailOverview(w http.ResponseWriter, r *http.Request, request *Request) {
	d, err := s.detail(r, request, r.PathValue("id"), "overview", true)
	if err == nil {
		err = s.overview(r, request, d)
	}
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	render(w, r, http.StatusOK, views.OverviewLive(*d))
}

func (s *Server) detailOperation(w http.ResponseWriter, r *http.Request, request *Request) {
	id := r.PathValue("id")
	if _, err := s.App.Authz.Assert(r.Context(), request.User(), id, authz.ServerView); err != nil {
		s.fail(w, r, request, err)
		return
	}
	a := views.ActivityView{ServerID: id, SelectedID: r.PathValue("operation"), Tab: "activity"}
	if request.User().Role == db.RoleAdmin {
		_, err := s.App.Authz.Assert(r.Context(), request.User(), id, authz.BackupsRead)
		a.Archives = err == nil
	}
	if r.URL.Query().Get("tab") == "backups" {
		a.Tab = "backups"
	}
	s.selectOperation(r, &a)
	// The panel polls only while the work is active, so this is the first
	// answer since it finished: the tab and the server's state catch up.
	if a.Selected != nil && !views.ActiveStatus(a.Selected.Status) {
		w.Header().Set("HX-Trigger", "operation-finished, servers-changed")
	}
	render(w, r, http.StatusOK, views.SelectedOperation(a))
}

// selectOperation reads one operation of the server on its own: a link can
// point beyond the recent list.
func (s *Server) selectOperation(r *http.Request, a *views.ActivityView) {
	job, err := s.App.Runner.Get(r.Context(), a.SelectedID)
	if err != nil || job == nil || job.ServerID != a.ServerID {
		a.SelectedError = "Operation not found"
		return
	}
	a.Selected = job
}

func (s *Server) detailActivity(w http.ResponseWriter, r *http.Request, request *Request) {
	id := r.PathValue("id")
	if _, err := s.App.Authz.Assert(r.Context(), request.User(), id, authz.ServerView); err != nil {
		s.fail(w, r, request, err)
		return
	}
	query := r.URL.Query()
	query.Del("operation")
	a := s.activity(r, request, id, query)
	// Keep the address bar on the page, so reloads and shared links keep
	// the filters.
	page := url.Values{"tab": {"activity"}}
	if a.Status != "" {
		page.Set("status", a.Status)
	}
	if a.Kind != "" {
		page.Set("kind", a.Kind)
	}
	if r.Header.Get("HX-Trigger") != "activity-list" {
		w.Header().Set("HX-Replace-Url", "/servers/"+id+"?"+page.Encode())
	}
	render(w, r, http.StatusOK, views.ActivityList(a))
}

// configureAvailability saves monitoring settings (administrators only).
func (s *Server) configureAvailability(w http.ResponseWriter, r *http.Request, request *Request) {
	id := r.PathValue("id")
	if _, err := s.App.Authz.Assert(r.Context(), request.User(), id, authz.ServerView); err != nil {
		s.fail(w, r, request, err)
		return
	}
	grace, err := strconv.ParseFloat(r.PostFormValue("graceSeconds"), 64)
	if err != nil {
		s.fail(w, r, request, apperr.New("INVALID_REQUEST", 400, "Enter a grace period between 10 and 86400 seconds."))
		return
	}
	raw, _ := json.Marshal(map[string]any{"enabled": r.PostFormValue("enabled") == "true",
		"maintenance": r.PostFormValue("maintenance") == "true", "graceSeconds": grace})
	policy, err := monitor.ParsePolicy(raw)
	if err != nil {
		s.fail(w, r, request, apperr.New("INVALID_REQUEST", 400, "Enter a grace period between 10 and 86400 seconds."))
		return
	}
	if _, err := s.App.Monitor.Configure(r.Context(), request.User().ID, request.Principal.TokenHash, id, policy); err != nil {
		err = authz.AdminMutationError(err)
		s.fail(w, r, request, err)
		return
	}
	s.audit(r, request, "availability.configured", id, nil)
	w.Header().Set("HX-Trigger", "availability-changed")
	render(w, r, http.StatusOK, views.Alert("success", "Monitoring settings saved."))
}

// reviewBinding accepts a changed server identity (administrators only).
func (s *Server) reviewBinding(w http.ResponseWriter, r *http.Request, request *Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if _, err := s.App.Servers.RefreshNow(ctx); err != nil {
		s.fail(w, r, request, err)
		return
	}
	server, err := s.App.Identity.Get(ctx, id)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	revision, revisionErr := strconv.ParseInt(r.PostFormValue("bindingRevision"), 10, 64)
	switch {
	case server == nil || server.PendingFingerprint == "":
		err = apperr.New("NO_REVIEW", 409, "This server has no pending binding review")
	case r.PostFormValue("confirmation") != server.DisplayName:
		err = apperr.New("CONFIRMATION_REQUIRED", 400, "Type the server name to accept the changed binding")
	case revisionErr != nil || revision != server.BindingRevision:
		err = apperr.New("SERVER_REVIEW_CHANGED", 409, "The pending server binding changed; reload the review")
	}
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	release, err := s.App.Locks.Acquire([]string{"server:" + id}, nil)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	err = func() error {
		defer release()
		if err := s.recheckAdmin(ctx, request); err != nil {
			return err
		}
		_, err := s.App.Identity.ReviewBinding(ctx, request.User().ID, request.Principal.TokenHash, id, server.PendingFingerprint, revision)
		return authz.AdminMutationError(err)
	}()
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	s.audit(r, request, "server.binding.reviewed", id, nil)
	w.Header().Set("HX-Refresh", "true")
	render(w, r, http.StatusOK, views.Alert("success", "Server binding reviewed."))
}

// recheckAdmin confirms the session still belongs to an active administrator.
func (s *Server) recheckAdmin(ctx context.Context, request *Request) error {
	current, err := s.App.Auth.Recheck(ctx, request.Principal)
	if err != nil {
		return err
	}
	return auth.RequireAdmin(current)
}
