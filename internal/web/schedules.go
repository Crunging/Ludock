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
	"restart": authz.ServerRestart, "backup": authz.BackupsCreate, "command": authz.ConsoleExecute}

func scheduleActions(view *servers.View) []string {
	var actions []string
	for _, action := range schedules.Actions {
		if (action == "command" && view.Console == nil) || view.Refuses(action) {
			continue
		}
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

// scheduleForm opens on a daily restart, backing up first when this person
// may and the server can be backed up, and as considerate to players as the
// game allows.
func (s *Server) scheduleForm(r *http.Request, d *views.ServerDetail) views.ScheduleForm {
	view := &d.Row.Server
	actions := scheduleActions(view)
	input := schedules.Input{Kind: "recurring", Enabled: true, Time: "08:00", Days: []int{0, 1, 2, 3, 4, 5, 6}}
	if slices.Contains(actions, "restart") {
		input.Action = "restart"
	} else if len(actions) > 0 {
		input.Action = actions[0]
	}
	canBackup := can(view, authz.BackupsCreate)
	if input.Action == "restart" && canBackup {
		input.CreateBackup = s.backupReady(r, view)
	}
	form := views.ScheduleForm{ServerID: view.ID, Actions: actions, CanCreateBackup: canBackup,
		CanAnnounce: canWarn(view), CanWait: s.App.Players.Source.Lists(view.Container),
		Blocked: view.BindingStatus != "active" || d.DiscoveryUnavailable}
	// The form keeps these for whichever disconnecting action is chosen.
	considerate := input
	considerate.Action = "restart"
	considerate.Considerate(form.CanAnnounce, form.CanWait)
	input.WaitForEmpty, input.Announcements = considerate.WaitForEmpty, considerate.Announcements
	form.Input, form.LeadText = input, views.LeadText(schedules.DefaultLeadMinutes)
	return form
}

// canWarn: schedules can warn players on this server, which takes the
// owner's console access.
func canWarn(view *servers.View) bool {
	return broadcasts(view) && can(view, authz.ConsoleExecute)
}

// broadcasts: the server's game has a command that messages every player.
func broadcasts(view *servers.View) bool {
	return view.Console != nil && view.Console.SupportsBroadcast
}

// backupReady reports whether backup storage is set up and the server has
// data folders to back up. The view hides folders from people who can't
// read files, so they are read from discovery.
func (s *Server) backupReady(r *http.Request, view *servers.View) bool {
	if settings, _, err := s.App.Backups.SavedSettings(r.Context()); err != nil || settings == nil {
		return false
	}
	current, err := s.App.Servers.Refresh(r.Context())
	return err == nil && len(current[view.Container.ID].FileRoots) > 0
}

// schedulesView reads the schedules tab; edit= opens a saved schedule in
// the form.
func (s *Server) schedulesView(r *http.Request, request *Request, d *views.ServerDetail) views.SchedulesView {
	query := r.URL.Query()
	view := &d.Row.Server
	v := views.SchedulesView{ServerID: view.ID, Selected: query.Get("schedule"), Editing: query.Get("edit"),
		Blocked: view.BindingStatus != "active" || d.DiscoveryUnavailable, Now: db.Now(), CanResume: map[string]bool{}, CanCancelWait: map[string]bool{},
		Viewer: request.User().ID, Admin: request.User().Role == db.RoleAdmin,
		ShowReplies: can(view, authz.ConsoleExecute),
		Form:        s.scheduleForm(r, d)}
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
		v.CanCancelWait[item.ID] = item.WaitingUntil != 0 && s.App.Schedules.CanCancelWait(r.Context(), request.User(), &item)
	}
	if v.Selected != "" && err == nil && !slices.ContainsFunc(list, func(item schedules.Schedule) bool { return item.ID == v.Selected }) {
		v.SelectedMissing = true
	}
	if v.Editing != "" {
		v.Form.EditID = v.Editing
		index := slices.IndexFunc(list, func(item schedules.Schedule) bool { return item.ID == v.Editing && v.Editable(item) })
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
	d, err := s.detail(r, request, r.PathValue("id"), "schedules", false)
	if err == nil && d.Tab != "schedules" {
		err = apperr.New("FORBIDDEN", 403, "You can’t do that")
	}
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	render(w, r, http.StatusOK, views.ScheduleList(s.schedulesView(r, request, d)))
}

// parseSchedule reads the form's draft; settings an action can't use are
// dropped, as the form hides them.
func parseSchedule(r *http.Request) (schedules.Input, string, error) {
	input := schedules.Input{Kind: r.PostFormValue("kind"), Action: r.PostFormValue("action"), Time: r.PostFormValue("time"),
		Timezone: strings.TrimSpace(r.PostFormValue("timezone")), CreateBackup: r.PostFormValue("createBackup") == "true",
		Enabled: r.PostFormValue("enabled") != "false"}
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
	if input.Action == "command" {
		input.Command = r.PostFormValue("command")
	}
	if schedules.Disconnects(input.Action) && r.PostFormValue("waitForEmpty") == "true" {
		minutes, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("waitMinutes")))
		input.WaitForEmpty = &schedules.WaitForEmpty{Minutes: minutes, Then: r.PostFormValue("waitThen")}
		if err != nil {
			return input, r.PostFormValue("leadMinutes"), apperr.Newf("INVALID_REQUEST", 400,
				"Wait between 1 and %d minutes for players to leave", schedules.MaxWaitMinutes)
		}
	}
	leads := r.PostFormValue("leadMinutes")
	if input.Action != "restart" {
		input.CreateBackup = false
	}
	if schedules.Disconnects(input.Action) && r.PostFormValue("announce") == "true" {
		var minutes []int
		for _, part := range strings.Split(leads, ",") {
			minute, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil {
				// Kept on, so the form reopens on the field to correct.
				input.Announcements = &schedules.Announcements{}
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
	d, err := s.detail(r, request, r.PathValue("id"), "schedules", false)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	serverID := d.Row.Server.ID
	form := s.scheduleForm(r, d)
	form.EditID = scheduleID
	input, leads, parseErr := parseSchedule(r)
	form.Input, form.LeadText = input, leads
	if leads == "" {
		form.LeadText = views.LeadText(schedules.DefaultLeadMinutes)
	}
	form.Revision, _ = strconv.ParseInt(r.PostFormValue("revision"), 10, 64)
	latest := func() (*schedules.Schedule, error) {
		list, err := s.App.Schedules.List(ctx, request.User(), serverID)
		for index := range list {
			if list[index].ID == scheduleID {
				return &list[index], nil
			}
		}
		return nil, err
	}
	// "Keep my draft" adopts the latest revision without saving.
	if keep := r.PostFormValue("keepDraft"); keep != "" && scheduleID != "" {
		saved, err := latest()
		if err != nil {
			s.fail(w, r, request, err)
			return
		}
		if saved != nil {
			form.Revision, form.Input.Enabled = saved.Revision, saved.Enabled
		} else {
			form.Missing = true
		}
		render(w, r, http.StatusOK, views.ScheduleEditor(form))
		return
	}
	var saved *schedules.Schedule
	err = parseErr
	if err == nil && input.Action == "command" && d.Row.Server.Console == nil {
		err = apperr.New("INVALID_REQUEST", 400, "This server's game console doesn't accept commands")
	}
	if err == nil {
		err = d.Row.Server.Refusal(input.Action)
	}
	if err == nil {
		if scheduleID == "" {
			saved, err = s.App.Schedules.Create(ctx, request.User(), request.Principal.TokenHash, serverID, draftForSave(input))
		} else {
			saved, err = s.App.Schedules.Update(ctx, request.User(), request.Principal.TokenHash, serverID, scheduleID, form.Revision, draftForSave(input))
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
			current, err := latest()
			if err != nil {
				s.fail(w, r, request, err)
				return
			}
			if current == nil {
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
	_, err = s.App.Schedules.SetEnabled(r.Context(), request.User(), request.Principal.TokenHash, r.PathValue("id"), r.PathValue("schedule"), revision, enabled)
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
	s.scheduleOutcome(w, r, request, message)
}

// scheduleOutcome sends a change's notice with the refreshed list, so no row
// from before the change stays on screen after the notice.
func (s *Server) scheduleOutcome(w http.ResponseWriter, r *http.Request, request *Request, message string) {
	d, err := s.detail(r, request, r.PathValue("id"), "schedules", false)
	if err != nil || d.Tab != "schedules" {
		render(w, r, http.StatusOK, views.Alert("success", message))
		return
	}
	w.Header().Del("HX-Trigger")
	render(w, r, http.StatusOK, views.ScheduleOutcome("success", message, s.schedulesView(r, request, d)))
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request, request *Request) {
	err := s.App.Schedules.Delete(r.Context(), request.User(), request.Principal.TokenHash, r.PathValue("id"), r.PathValue("schedule"))
	w.Header().Set("HX-Trigger", "schedules-changed")
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	s.scheduleOutcome(w, r, request, "Schedule deleted.")
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
	players := ""
	if err == nil {
		players = views.PlayersPlan(input)
	}
	text := "Choose a valid time, time zone, and at least one day."
	if input.Kind == "once" {
		text = "Choose a future date and time that exists in this time zone."
	}
	if err == nil {
		if input.Announcements != nil && len(input.Announcements.LeadMinutes) == 0 {
			input.Announcements = nil
		}
		// The preview shows timing, which a command not yet typed doesn't change.
		if input.Action == "command" {
			input.Command = "preview"
		}
		// Nor does waiting for an empty server, which starts at that time.
		input.WaitForEmpty = nil
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
	render(w, r, http.StatusOK, views.SchedulePreview(input.Enabled, text, players))
}
