package web

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/schedtime"
	"github.com/Crunging/Ludock/internal/schedules"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/web/views"
)

var scheduleActionCapabilities = map[string]authz.Capability{"start": authz.ServerStart, "stop": authz.ServerStop,
	"restart": authz.ServerRestart, "backup": authz.BackupsCreate}

// scheduleActions are the actions the viewer may schedule.
func scheduleActions(view *servers.View) []string {
	var actions []string
	for _, action := range schedules.Actions {
		if can(view, scheduleActionCapabilities[action]) {
			actions = append(actions, action)
		}
	}
	return actions
}

// canRunSchedule: saving or resuming needs schedule access and every
// capability the schedule uses. The service checks again.
func canRunSchedule(view *servers.View, input schedules.Input) bool {
	if !can(view, authz.SchedulesManage) {
		return false
	}
	return !slices.ContainsFunc(input.Capabilities(), func(capability authz.Capability) bool { return !can(view, capability) })
}

func (s *Server) scheduleForm(d *views.ServerDetail) views.ScheduleForm {
	view := &d.Row.Server
	return views.ScheduleForm{ServerID: view.ID, Actions: scheduleActions(view), CanCreateBackup: can(view, authz.BackupsCreate),
		CanAnnounce: view.Console != nil && view.Console.SupportsBroadcast && can(view, authz.ConsoleExecute),
		Blocked:     view.BindingStatus != "active" || d.DiscoveryUnavailable,
		Input:       schedules.Input{Kind: "recurring", Action: "start", Enabled: true, Time: "08:00", Days: []int{0, 1, 2, 3, 4, 5, 6}},
		LeadText:    "10, 5, 1"}
}

// schedulesView reads the schedules tab; edit= opens a saved schedule in
// the form.
func (s *Server) schedulesView(r *http.Request, request *Request, d *views.ServerDetail) views.SchedulesView {
	query := r.URL.Query()
	view := &d.Row.Server
	v := views.SchedulesView{ServerID: view.ID, Selected: query.Get("schedule"), Editing: query.Get("edit"),
		Blocked: view.BindingStatus != "active" || d.DiscoveryUnavailable, Now: db.Now(), CanResume: map[string]bool{},
		Form: s.scheduleForm(d)}
	switch query.Get("saved") {
	case "created":
		v.Message = "Schedule created."
	case "saved":
		v.Message = "Schedule saved."
	}
	list, err := s.App.Schedules.List(r.Context(), request.User(), view.ID)
	if err != nil {
		v.Error = publicMessage(err)
	}
	v.Schedules = list
	for _, item := range list {
		v.CanResume[item.ID] = canRunSchedule(view, item.Input)
	}
	if v.Selected != "" && err == nil && !slices.ContainsFunc(list, func(item schedules.Schedule) bool { return item.ID == v.Selected }) {
		v.SelectedMissing = true
	}
	if v.Editing != "" {
		v.Form.EditID = v.Editing
		index := slices.IndexFunc(list, func(item schedules.Schedule) bool { return item.ID == v.Editing })
		if index < 0 {
			v.Form.Missing = true
		} else {
			v.Form.Input, v.Form.Revision = list[index].Input, list[index].Revision
			if v.Form.Input.Announcements != nil {
				v.Form.LeadText = views.LeadText(v.Form.Input.Announcements.LeadMinutes)
			}
		}
	}
	return v
}

func (s *Server) scheduleList(w http.ResponseWriter, r *http.Request, request *Request) {
	d, err := s.detail(r, request, r.PathValue("id"), "schedules")
	if err == nil && d.Tab != "schedules" {
		err = apperr.New("FORBIDDEN", 403, "Insufficient permissions")
	}
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	render(w, r, http.StatusOK, views.ScheduleList(s.schedulesView(r, request, d)))
}

// parseSchedule reads the form's draft; restart-only settings are dropped
// for other actions, as the form hides them.
func parseSchedule(r *http.Request) (schedules.Input, string, error) {
	input := schedules.Input{Kind: r.PostFormValue("kind"), Action: r.PostFormValue("action"), Time: r.PostFormValue("time"),
		Timezone: strings.TrimSpace(r.PostFormValue("timezone")), CreateBackup: r.PostFormValue("createBackup") == "true",
		Enabled: r.PostFormValue("paused") != "true"}
	if value := r.PostFormValue("enabled"); value != "" {
		input.Enabled = value == "true"
	}
	// The draft keeps both the days and the date, so switching kinds and
	// back loses neither; draftForSave drops the unused one.
	input.Date = r.PostFormValue("date")
	input.Days = []int{}
	for _, value := range r.PostForm["days"] {
		day, err := strconv.Atoi(value)
		if err != nil || day < 0 || day > 6 {
			return input, "", apperr.New("INVALID_REQUEST", 400, "Choose at least one day")
		}
		if !slices.Contains(input.Days, day) {
			input.Days = append(input.Days, day)
		}
	}
	slices.Sort(input.Days)
	leads := r.PostFormValue("leadMinutes")
	if input.Action != "restart" {
		input.CreateBackup = false
		return input, leads, nil
	}
	if r.PostFormValue("announce") == "true" {
		var minutes []int
		for _, part := range strings.Split(leads, ",") {
			minute, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil {
				return input, leads, apperr.New("INVALID_REQUEST", 400, "Enter 1–10 unique whole numbers between 1 and 60.")
			}
			minutes = append(minutes, minute)
		}
		input.Announcements = &schedules.Announcements{LeadMinutes: minutes}
	}
	return input, leads, nil
}

// draftForSave keeps only the fields of the draft's kind.
func draftForSave(input schedules.Input) schedules.Input {
	if input.Kind == "once" {
		input.Days = nil
	} else {
		input.Date = ""
	}
	return input
}

// saveSchedule creates or updates a schedule. Errors and conflicts render
// the form again with the draft.
func (s *Server) saveSchedule(w http.ResponseWriter, r *http.Request, request *Request) {
	ctx := r.Context()
	scheduleID := r.PathValue("schedule")
	d, err := s.detail(r, request, r.PathValue("id"), "schedules")
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	serverID := d.Row.Server.ID
	form := s.scheduleForm(d)
	form.EditID = scheduleID
	input, leads, parseErr := parseSchedule(r)
	form.Input, form.LeadText = input, leads
	if leads == "" {
		form.LeadText = "10, 5, 1"
	}
	form.Revision, _ = strconv.ParseInt(r.PostFormValue("revision"), 10, 64)
	latest := func() *schedules.Schedule {
		list, _ := s.App.Schedules.List(ctx, request.User(), serverID)
		for index := range list {
			if list[index].ID == scheduleID {
				return &list[index]
			}
		}
		return nil
	}
	// "Keep my draft" adopts the latest revision without saving.
	if keep := r.PostFormValue("keepDraft"); keep != "" && scheduleID != "" {
		if saved := latest(); saved != nil {
			form.Revision, form.Input.Enabled = saved.Revision, saved.Enabled
		} else {
			form.Missing = true
		}
		render(w, r, http.StatusOK, views.ScheduleEditor(form))
		return
	}
	var saved *schedules.Schedule
	err = parseErr
	if err == nil {
		if scheduleID == "" {
			saved, err = s.App.Schedules.Create(ctx, request.User(), serverID, draftForSave(input))
		} else {
			saved, err = s.App.Schedules.Update(ctx, request.User(), serverID, scheduleID, form.Revision, draftForSave(input))
		}
	}
	if err != nil {
		public, ok := apperr.As(err)
		if !ok || public.Status == http.StatusUnauthorized {
			s.fail(w, r, request, err)
			return
		}
		form.Error = public.Message
		if scheduleID != "" && (public.Code == "SCHEDULE_CHANGED" || public.Status == http.StatusNotFound) {
			form.Error = ""
			if current := latest(); current == nil {
				form.Missing = true
			} else {
				form.Conflict = current
			}
		}
		w.Header().Set("HX-Trigger", "schedules-changed")
		render(w, r, public.Status, views.ScheduleEditor(form))
		return
	}
	result := "created"
	if scheduleID != "" {
		result = "saved"
	}
	w.Header().Set("HX-Redirect", "/servers/"+serverID+"?tab=schedules&schedule="+saved.ID+"&saved="+result)
	changed(w)
}

func (s *Server) toggleSchedule(w http.ResponseWriter, r *http.Request, request *Request) {
	revision, err := strconv.ParseInt(r.PostFormValue("revision"), 10, 64)
	if err != nil {
		s.fail(w, r, request, apperr.New("INVALID_REQUEST", 400, "Invalid request"))
		return
	}
	enabled := r.PostFormValue("enabled") == "true"
	_, err = s.App.Schedules.SetEnabled(r.Context(), request.User(), r.PathValue("id"), r.PathValue("schedule"), revision, enabled)
	// A stale list reloads either way.
	w.Header().Set("HX-Trigger", "schedules-changed")
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	message := "Schedule paused."
	if enabled {
		message = "Schedule resumed."
	}
	render(w, r, http.StatusOK, views.Alert("success", message))
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request, request *Request) {
	err := s.App.Schedules.Delete(r.Context(), request.User(), r.PathValue("id"), r.PathValue("schedule"))
	w.Header().Set("HX-Trigger", "schedules-changed")
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	render(w, r, http.StatusOK, views.Alert("success", "Schedule deleted."))
}

// previewSchedule shows a draft's next run as the form changes.
func (s *Server) previewSchedule(w http.ResponseWriter, r *http.Request, request *Request) {
	serverID := r.PathValue("id")
	if _, err := s.App.Authz.Assert(r.Context(), request.User(), serverID, authz.ServerView); err != nil {
		s.fail(w, r, request, err)
		return
	}
	input, _, err := parseSchedule(r)
	input = draftForSave(input)
	text := "Choose a valid time, time zone, and at least one day."
	if input.Kind == "once" {
		text = "Choose a future date and time that exists in this time zone."
	}
	if err == nil {
		if input.Announcements != nil && len(input.Announcements.LeadMinutes) == 0 {
			input.Announcements = nil
		}
		if input.Validate() == nil {
			lastSlot := ""
			if id := r.PostFormValue("scheduleId"); id != "" {
				list, _ := s.App.Schedules.List(r.Context(), request.User(), serverID)
				for _, item := range list {
					if item.ID == id {
						lastSlot = item.LastSlot
					}
				}
			}
			clock := input.Clock()
			clock.Enabled = true
			now := db.Now()
			minute := now / 60_000 * 60_000
			if next, ok := schedtime.NextRun(clock, minute, lastSlot); ok && (input.Kind == "recurring" || next >= minute) {
				text = views.ScheduleRun(next, input.Timezone, now)
			}
		}
	}
	render(w, r, http.StatusOK, views.SchedulePreview(input.Enabled, text))
}
