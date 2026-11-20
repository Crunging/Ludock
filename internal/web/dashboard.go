package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/events"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/jobs"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/update"
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
		Search: r.URL.Query().Get("search"), State: r.URL.Query().Get("state"), AddOpen: r.URL.Query().Has("add")}
	all, err := s.App.Servers.List(ctx, request.User())
	if err != nil {
		list.DiscoveryUnavailable = true
		all, _ = s.App.Servers.ListSaved(ctx, request.User())
	}
	list.Total = len(all)
	down := s.downServers(ctx, all)
	search := strings.ToLower(strings.TrimSpace(list.Search))
	states := map[string]bool{}
	for index := range all {
		view := &all[index]
		status := views.ServerRow{Server: *view, Down: down[view.ID]}.Status()
		list.Fleet = append(list.Fleet, views.FleetBlock{Name: view.DisplayName, Label: status.Label, Tone: status.Tone})
		states[view.State] = true
		if backupSummary(request.User(), view) {
			list.ShowBackup = true
		}
		if (view.Console != nil && view.Console.ListsPlayers) || view.Query != nil {
			list.ShowPlayers = true
		}
	}
	if list.State != "" && list.State != "all" {
		states[list.State] = true
	}
	for state := range states {
		list.States = append(list.States, state)
	}
	sort.Strings(list.States)
	checks := list.Admin && s.App.Checks.Enabled(ctx)
	for index := range all {
		view := all[index]
		if list.State != "" && list.State != "all" && view.State != list.State {
			continue
		}
		if !strings.Contains(strings.ToLower(view.DisplayName+" "+view.GameName+" "+view.GameType+" "+view.Image), search) {
			continue
		}
		row := s.serverRow(r, request, view, list.ShowBackup, list.DiscoveryUnavailable)
		row.ShowPlayers, row.Down = list.ShowPlayers, down[view.ID]
		if check := s.App.Checks.Status(ctx, view.ID, view.Container.ID); checks && !list.DiscoveryUnavailable && check != nil {
			row.ImageUpdate = check.State == update.CheckAvailable
		}
		list.Rows = append(list.Rows, row)
	}
	return list
}

// downServers are the servers that stopped without anyone stopping them and
// whose outage alert is due, so pages call them down as the alert does.
func (s *Server) downServers(ctx context.Context, list []servers.View) map[string]bool {
	problems, err := s.App.Monitor.Problems(ctx, db.Now())
	if err != nil {
		log.Warn("Could not read server outages")
		return nil
	}
	down := map[string]bool{}
	for _, view := range list {
		// The saved state must still be the one the outage recorded.
		if problem := problems[view.ID]; problem != nil && problem.State == view.State && views.LifecycleAction(view.State) == "start" {
			down[view.ID] = true
		}
	}
	return down
}

func (s *Server) serverRow(r *http.Request, request *Request, view servers.View, showBackup, stale bool) views.ServerRow {
	action := views.LifecycleAction(view.State)
	active := view.BindingStatus == "active"
	row := views.ServerRow{Server: view, Address: views.ConnectAddress(view.Connection, r.Host), ShowBackup: showBackup,
		Disabled: stale || view.ActiveOperation != nil || !active,
		CanStart: action == "start" && can(&view, authz.ServerStart), CanStop: action == "stop" && can(&view, authz.ServerStop),
		CanRestart: view.State == "running" && can(&view, authz.ServerRestart), CanBackup: can(&view, authz.BackupsCreate),
		CanFiles: can(&view, authz.FilesRead) && len(view.FileRoots) > 0, BackupSummary: backupSummary(request.User(), &view)}
	// A saved state is not live, so it has no players.
	consoleLists := view.Console != nil && view.Console.ListsPlayers
	row.Players = views.PlayersView{Listed: consoleLists || view.Query != nil, Running: view.State == "running" && active && !stale}
	if row.Players.Listed && row.Players.Running {
		if consoleLists {
			row.Players.Console = view.Console.Name
		}
		if view.Query != nil {
			row.Players.Query = view.Query.Name()
		}
		row.Players.Reading = s.App.Players.Get(view.ID, view.Container)
	}
	if row.CanStop || row.CanRestart {
		row.Warn, row.Online = warnOffer(view.Container), s.onlineNow(view.ID, view.Container)
	}
	row.Game = views.GameView{Running: row.Players.Running, Off: view.QueryOff}
	switch {
	case view.Query != nil:
		row.Game.Query = view.Query.Name()
		row.Game.State, row.Game.AnsweredAt = s.App.Players.Game(view.ID, view.Container)
	case view.HealthAnswers:
		row.Game.Query, row.Game.Health = "health check", true
		row.Game.State, row.Game.AnsweredAt = s.App.Players.Game(view.ID, view.Container)
	}
	// The button is named for the page it opens first.
	switch {
	case can(&view, authz.ConsoleExecute) && view.Console != nil:
		row.ConsoleLabel = "Console"
	case can(&view, authz.LogsRead):
		row.ConsoleLabel = "Logs"
	case can(&view, authz.ConsoleShell):
		row.ConsoleLabel = "Shell"
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
	list := s.serverList(r, request)
	// Every list request carries the filters as they are, so each keeps the
	// address bar in step; going back to the list or reloading it keeps them.
	if htmxTarget(r) == "servers" {
		page := url.Values{}
		if list.Search != "" {
			page.Set("search", list.Search)
		}
		if list.State != "" && list.State != "all" {
			page.Set("state", list.State)
		}
		target := "/"
		if len(page) > 0 {
			target += "?" + page.Encode()
		}
		w.Header().Set("HX-Replace-Url", target)
	}
	html(w)
	views.ServerListResponse(list).Render(r.Context(), w)
}

func (s *Server) attentionFragment(w http.ResponseWriter, r *http.Request, request *Request) {
	view := views.AttentionView{Admin: request.User().Role == db.RoleAdmin}
	result, err := s.App.Attention.List(r.Context(), request.User(), time.Now().UnixMilli())
	if err != nil {
		log.Warn("The attention check failed", logging.F("error", err.Error()))
		view.Error = "The attention check failed. Try again."
	} else {
		view.DiscoveryUnavailable = result.DiscoveryUnavailable
		for _, item := range result.Items {
			// Role ceilings stay applied to administrator-only workflows.
			if item.Kind == "binding" && request.User().Role != db.RoleAdmin {
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
	// Pause outage tracking before acting, or the start could alert.
	if err := app.Monitor.Suppress(action.Ctx, action.Logical.ID); err != nil {
		s.fail(w, r, request, err)
		return
	}
	err = app.Observer.ChangeState(action.Ctx, action.Container.ID, servers.Start, func(observation identity.Observation, _ *docker.ContainerInspect) error {
		return action.AssertObserved(observation)
	})
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	if err := app.Monitor.SetIntentionalStop(action.Ctx, action.Logical.ID, false); err != nil {
		log.Warn("Could not save a server's monitoring state; an outage alert may follow")
	}
	s.audit(r, request, "server.start", action.Logical.ID, nil)
	if fromDetail(r) {
		w.Header().Set("HX-Trigger", "servers-changed")
		render(w, r, http.StatusOK, views.Alert("success", "Server started."))
		return
	}
	changed(w)
}

func fromDetail(r *http.Request) bool { return htmxTarget(r) == "detail-status" }

func (s *Server) audit(r *http.Request, request *Request, action, serverID string, details map[string]any) {
	entry := db.AuditEntry{UserID: request.User().ID, Action: action, TargetType: "settings", Details: details, IPAddress: request.Client.IP}
	if serverID != "" {
		entry.TargetType, entry.TargetID = "server", serverID
	}
	if details == nil {
		entry.Details = nil
	}
	// The action already happened; a lost entry is logged, never hidden.
	if err := s.App.DB.WriteAudit(context.WithoutCancel(r.Context()), nil, entry); err != nil {
		log.Error("Could not write an audit entry", logging.F("action", action))
	}
}

func (s *Server) queueLifecycle(kind string) func(http.ResponseWriter, *http.Request, *Request) {
	return func(w http.ResponseWriter, r *http.Request, request *Request) {
		capability := authz.Capability("server." + kind)
		createBackup := kind == "restart" && r.PostFormValue("createBackup") == "true"
		warn := r.PostFormValue(jobs.WarnInput) == "true"
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
		input := map[string]any{}
		if createBackup {
			input["createBackup"] = true
		}
		if warn && warnOffer(resolved.Container) {
			input[jobs.WarnInput] = true
		}
		job, err := s.App.Runner.Enqueue(r.Context(), nil, ops.EnqueueOptions{ServerID: resolved.Logical.ID, ActorID: current.User.ID,
			Kind: kind, BindingRevision: resolved.Logical.BindingRevision, Input: input, IdempotencyKey: requestKey(r)})
		if err != nil {
			s.fail(w, r, request, err)
			return
		}
		if fromDetail(r) {
			followOperation(w, resolved.Logical.ID, job)
		}
		changed(w)
	}
}

// warnOffer is whether work that takes a server down offers to warn players
// first. Whether anyone is online is decided when the work runs, not from
// the page.
func warnOffer(container servers.Container) bool {
	return container.State == "running" && jobs.CanWarn(container)
}

// onlineNow is how many players a running server has, or 0 when none are
// or Ludock doesn't know.
func (s *Server) onlineNow(id string, container servers.Container) int {
	if container.State != "running" {
		return 0
	}
	online, known := s.App.Players.Online(id, container)
	if !known {
		return 0
	}
	return online
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

// Event streams stay open for a session, so like consoles they are limited.
const (
	maxStreams           = 512
	maxStreamsPerUser    = 32
	maxStreamsPerSession = 16
)

type streamCounts struct {
	mu            sync.Mutex
	total         int
	user, session map[string]int
}

func (c *streamCounts) admit(userID, tokenHash string) (int, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.user == nil {
		c.user, c.session = map[string]int{}, map[string]int{}
	}
	switch {
	case c.session[tokenHash] >= maxStreamsPerSession:
		return http.StatusTooManyRequests, "Event stream session limit reached"
	case c.user[userID] >= maxStreamsPerUser:
		return http.StatusTooManyRequests, "Event stream user limit reached"
	case c.total >= maxStreams:
		return http.StatusServiceUnavailable, "Event stream limit reached"
	}
	c.total++
	c.user[userID]++
	c.session[tokenHash]++
	return 0, ""
}

func (c *streamCounts) release(userID, tokenHash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total--
	if c.user[userID]--; c.user[userID] == 0 {
		delete(c.user, userID)
	}
	if c.session[tokenHash]--; c.session[tokenHash] == 0 {
		delete(c.session, tokenHash)
	}
}

// events streams server changes as SSE until the session ends. Only logical
// IDs are sent.
func (s *Server) events(w http.ResponseWriter, r *http.Request, request *Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}
	user := request.User()
	if status, message := s.openStreams.admit(user.ID, request.Principal.TokenHash); status != 0 {
		http.Error(w, message, status)
		return
	}
	defer s.openStreams.release(user.ID, request.Principal.TokenHash)
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
	// A server's page shows only that server's players; a server the viewer
	// can't see is followed by nobody.
	if server := r.URL.Query().Get("server"); server != "" {
		if visible, err := s.App.Authz.Has(r.Context(), user, server, authz.ServerView); err != nil || !visible {
			server = ""
		}
		subscriber.Follows = func(id string) bool { return server != "" && id == server }
	}
	unsubscribe := s.App.Events.Subscribe(subscriber)
	defer unsubscribe()
	fmt.Fprint(w, "retry: 5000\n\n")
	flusher.Flush()
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	shutdown := s.streamsDone()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-shutdown:
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
