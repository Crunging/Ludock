package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/events"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/jobs"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/web/views"
)

func html(w http.ResponseWriter) { w.Header().Set("Content-Type", "text/html; charset=utf-8") }

func can(view *servers.View, capability authz.Capability) bool {
	return view.Can(authz.ServerView) && view.Can(capability)
}

// backupSummary: a create grant shows the latest backup's time and size,
// never the archives.
func backupSummary(user *db.SessionUser, view *servers.View) bool {
	return (user.Role == db.RoleAdmin && view.Can(authz.ServerView)) || can(view, authz.BackupsRead) || can(view, authz.BackupsCreate)
}

func (s *Server) serverList(r *http.Request, request *Request) views.ServerList {
	ctx := r.Context()
	list := views.ServerList{Admin: request.User().Role == db.RoleAdmin, Username: request.User().Username,
		Search: r.URL.Query().Get("search"), State: r.URL.Query().Get("state")}
	all, err := s.App.Servers.List(ctx, request.User())
	if err != nil {
		list.DiscoveryUnavailable = true
		all, _ = s.App.Servers.ListSaved(ctx, request.User())
	}
	list.Total = len(all)
	search := strings.ToLower(strings.TrimSpace(list.Search))
	states := map[string]bool{}
	for index := range all {
		view := &all[index]
		states[view.State] = true
		if backupSummary(request.User(), view) {
			list.ShowBackup = true
		}
	}
	if list.State != "" && list.State != "all" {
		states[list.State] = true
	}
	for state := range states {
		list.States = append(list.States, state)
	}
	sort.Strings(list.States)
	for index := range all {
		view := all[index]
		if list.State != "" && list.State != "all" && view.State != list.State {
			continue
		}
		if !strings.Contains(strings.ToLower(view.DisplayName+" "+view.GameName+" "+view.GameType+" "+view.Image), search) {
			continue
		}
		list.Rows = append(list.Rows, s.serverRow(r, request, view, list.ShowBackup, list.DiscoveryUnavailable))
	}
	return list
}

func (s *Server) serverRow(r *http.Request, request *Request, view servers.View, showBackup, stale bool) views.ServerRow {
	action := views.LifecycleAction(view.State)
	active := view.BindingStatus == "active"
	consoleAvailable := (can(&view, authz.ConsoleExecute) && view.Console != nil) || can(&view, authz.ConsoleShell)
	row := views.ServerRow{Server: view, Address: views.ConnectAddress(view.Connection, r.Host), ShowBackup: showBackup,
		Disabled: stale || view.ActiveOperation != nil || !active,
		CanStart: action == "start" && can(&view, authz.ServerStart), CanStop: action == "stop" && can(&view, authz.ServerStop),
		CanRestart: view.State == "running" && can(&view, authz.ServerRestart), CanBackup: can(&view, authz.BackupsCreate),
		CanFiles: can(&view, authz.FilesRead) && len(view.FileRoots) > 0, BackupSummary: backupSummary(request.User(), &view)}
	if consoleAvailable {
		row.ConsoleLabel = "Console"
	} else if can(&view, authz.LogsRead) {
		row.ConsoleLabel = "Logs"
	}
	return row
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request, request *Request) {
	page := s.page(r, request, "Servers")
	page.ServerTools = true
	html(w)
	views.Dashboard(page, s.serverList(r, request)).Render(r.Context(), w)
}

func (s *Server) serversFragment(w http.ResponseWriter, r *http.Request, request *Request) {
	html(w)
	views.ServerListFragment(s.serverList(r, request)).Render(r.Context(), w)
}

func (s *Server) attentionFragment(w http.ResponseWriter, r *http.Request, request *Request) {
	view := views.AttentionView{Admin: request.User().Role == db.RoleAdmin}
	result, err := s.App.Attention.List(r.Context(), request.User(), time.Now().UnixMilli())
	if err != nil {
		view.Error = "The attention check failed. Try again."
	} else {
		view.DiscoveryUnavailable = result.DiscoveryUnavailable
		for _, item := range result.Items {
			// Role ceilings stay applied to administrator-only workflows.
			if item.Kind == "binding" && request.User().Role != db.RoleAdmin {
				continue
			}
			if item.Kind == "schedule" && request.User().Role == db.RoleViewer {
				continue
			}
			view.Items = append(view.Items, item)
		}
	}
	html(w)
	views.NeedsAttention(view).Render(r.Context(), w)
}

// changed tells htmx lists to refresh and clears the action error area.
func changed(w http.ResponseWriter) {
	w.Header().Set("HX-Trigger", "servers-changed")
	html(w)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) startServer(w http.ResponseWriter, r *http.Request, request *Request) {
	action, err := s.startAction(r, request, r.PathValue("id"), authz.ServerStart)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	defer action.Done()
	app := s.App
	app.Monitor.Suppress(action.Ctx, action.Logical.ID)
	err = app.Observer.ChangeState(action.Ctx, action.Container.ID, servers.Start, func(observation identity.Observation, _ *docker.ContainerInspect) error {
		return action.AssertObserved(observation)
	})
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	app.Monitor.SetIntentionalStop(action.Ctx, action.Logical.ID, false)
	s.audit(r, request, "server.start", action.Logical.ID, nil)
	if fromDetail(r) {
		w.Header().Set("HX-Trigger", "servers-changed")
		render(w, r, http.StatusOK, views.Alert("success", "Server started."))
		return
	}
	changed(w)
}

// fromDetail reports an action submitted from the server detail page.
func fromDetail(r *http.Request) bool { return r.Header.Get("HX-Target") == "detail-status" }

func (s *Server) audit(r *http.Request, request *Request, action, serverID string, details map[string]any) {
	entry := db.AuditEntry{UserID: request.User().ID, Action: action, TargetType: "settings", Details: details, IPAddress: request.Client.IP}
	if serverID != "" {
		entry.TargetType, entry.TargetID = "server", serverID
	}
	if details == nil {
		entry.Details = nil
	}
	s.App.DB.WriteAudit(context.WithoutCancel(r.Context()), nil, entry)
}

func (s *Server) queueLifecycle(kind string) func(http.ResponseWriter, *http.Request, *Request) {
	return func(w http.ResponseWriter, r *http.Request, request *Request) {
		capability := authz.Capability("server." + kind)
		createBackup := kind == "restart" && r.PostFormValue("createBackup") == "true"
		resolved, err := s.App.Servers.ResolveAuthorized(r.Context(), request.User(), r.PathValue("id"), capability, -1)
		if err != nil {
			s.fail(w, r, request, err)
			return
		}
		current, err := s.App.Auth.Recheck(r.Context(), request.Principal)
		if err == nil {
			_, err = s.App.Authz.Assert(r.Context(), &current.User, resolved.Logical.ID, capability)
		}
		if err == nil && createBackup {
			if _, err = s.App.Authz.Assert(r.Context(), &current.User, resolved.Logical.ID, authz.BackupsCreate); err == nil {
				err = jobs.AssertRunningForBackupRestart(resolved.Container.State)
			}
		}
		if err != nil {
			s.fail(w, r, request, err)
			return
		}
		var input map[string]any
		if createBackup {
			input = map[string]any{"createBackup": true}
		}
		job, err := s.App.Runner.Enqueue(r.Context(), nil, ops.EnqueueOptions{ServerID: resolved.Logical.ID, ActorID: current.User.ID,
			Kind: kind, BindingRevision: resolved.Logical.BindingRevision, Input: input, IdempotencyKey: requestKey(r)})
		if err != nil {
			s.fail(w, r, request, err)
			return
		}
		if fromDetail(r) {
			// Follow the queued work in the server's activity.
			w.Header().Set("HX-Redirect", "/servers/"+resolved.Logical.ID+"?tab=activity&operation="+job.ID)
		}
		changed(w)
	}
}

// requestKey is the optional idempotency key of a form submission.
func requestKey(r *http.Request) string {
	key := r.PostFormValue("requestKey")
	if key == "" || len(key) > 128 || strings.IndexFunc(key, func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-')
	}) >= 0 {
		return ""
	}
	return key
}

// events streams server changes as SSE. Only logical IDs are sent, after a
// fresh discovery snapshot and grant check; the stream ends when the session
// does.
func (s *Server) events(w http.ResponseWriter, r *http.Request, request *Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	messages := make(chan string, 64)
	closed := make(chan struct{})
	var once = make(chan struct{}, 1)
	subscriber := &events.Subscriber{
		Validate: func(ctx context.Context) *db.SessionUser {
			principal, err := s.App.Auth.Session(ctx, request.Principal.Token)
			if err != nil || principal == nil {
				return nil
			}
			return &principal.User
		},
		Send: func(event events.Event) {
			id := event.ServerID
			if id == "" {
				id = "refresh"
			}
			select {
			case messages <- id:
			default:
			}
		},
		Close: func(string) {
			select {
			case once <- struct{}{}:
				close(closed)
			default:
			}
		},
	}
	unsubscribe := s.App.Events.Subscribe(subscriber)
	defer unsubscribe()
	fmt.Fprint(w, "retry: 5000\n\n")
	flusher.Flush()
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-closed:
			fmt.Fprint(w, "event: expired\ndata: signin\n\n")
			flusher.Flush()
			return
		case id := <-messages:
			// "server" refreshes lists; "server-<id>" or "refresh" refreshes
			// one server's page. IDs are UUIDs, safe as event names.
			name := "server-" + id
			if id == "refresh" {
				name = "refresh"
			}
			fmt.Fprintf(w, "event: server\ndata: %s\n\nevent: %s\ndata: %s\n\n", id, name, id)
			flusher.Flush()
		case <-heartbeat.C:
			if principal, _ := s.App.Auth.Session(r.Context(), request.Principal.Token); principal == nil {
				fmt.Fprint(w, "event: expired\ndata: signin\n\n")
				flusher.Flush()
				return
			}
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		}
	}
}
