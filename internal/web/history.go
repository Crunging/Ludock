package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/history"
	"github.com/Crunging/Ludock/internal/web/views"
)

var historyFilters = []string{"show", "serverId", "actor", "action", "status", "from", "to", "operationId"}

// historyQuery reads the applied filters and cursor from the URL. Everything,
// the audit log, is for administrators; others always see server work.
func historyQuery(r *http.Request, admin bool) (history.Query, url.Values, error) {
	source := r.URL.Query()
	everything := admin && source.Get("show") == views.ShowEverything
	filters := url.Values{}
	for _, name := range historyFilters {
		if value := strings.TrimSpace(source.Get(name)); value != "" && ((name != "operationId" && name != "show") || everything) {
			filters.Set(name, value)
		}
	}
	q := history.Query{Cursor: source.Get("cursor"), ServerID: filters.Get("serverId"), Actor: filters.Get("actor"),
		Action: filters.Get("action"), Status: filters.Get("status"), OperationID: filters.Get("operationId"), From: -1, To: -1}
	for name, target := range map[string]*int64{"from": &q.From, "to": &q.To} {
		if value := filters.Get(name); value != "" {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed < 0 {
				return q, filters, apperr.New("INVALID_REQUEST", 400, "Invalid request")
			}
			*target = parsed
		}
	}
	if q.From >= 0 && q.To >= 0 && q.From > q.To {
		return q, filters, apperr.New("INVALID_REQUEST", 400, "The start date must be before the end date.")
	}
	return q, filters, nil
}

// historyServers names the servers the viewer can see, from saved metadata.
func (s *Server) historyServers(r *http.Request, request *Request, h *views.HistoryView) {
	h.Names = map[string]string{}
	list, err := s.App.Servers.Names(r.Context(), request.User())
	if err != nil {
		h.NamesError = true
		return
	}
	for _, server := range list {
		h.Servers = append(h.Servers, views.HistoryServer{ID: server.ID, Name: server.DisplayName})
		h.Names[server.ID] = server.DisplayName
	}
}

// activityPage lists work on the servers the viewer can see, or for
// administrators who ask for everything, the whole audit log.
func (s *Server) activityPage(w http.ResponseWriter, r *http.Request, request *Request) {
	admin := request.User().Role == db.RoleAdmin
	q, filters, err := historyQuery(r, admin)
	h := views.HistoryView{Admin: admin, Everything: filters.Get("show") == views.ShowEverything, Filters: filters, Cursor: q.Cursor}
	s.historyServers(r, request, &h)
	if err == nil {
		if h.Everything {
			q.ActionKeys = views.AuditKeys(q.Action)
			h.Entries, h.Next, err = s.App.History.Audit(r.Context(), q)
			h.Users = map[string]string{}
			if users, err := s.App.DB.ListUsers(r.Context()); err == nil {
				for _, user := range users {
					h.Users[user.ID] = user.Username
				}
			}
		} else {
			h.Operations, h.Next, err = s.App.History.Operations(r.Context(), request.User(), q)
			if err == nil {
				h.Cancellable = s.cancellable(r, request.User(), h.Operations)
			}
		}
	}
	if h.Everything {
		// The session may have lost the administrator role while the log was read.
		if err := s.recheckAdmin(r.Context(), request); err != nil {
			s.fail(w, r, request, err)
			return
		}
	}
	if err != nil {
		h.Error = publicMessage(err)
	}
	render(w, r, http.StatusOK, views.ActivityPage(s.page(r, request, "Activity"), h))
}

// operationDetail reads one operation the viewer may see. A hidden server's
// operation looks the same as a missing one.
func (s *Server) operationDetail(r *http.Request, request *Request) views.OperationDetailView {
	o := views.OperationDetailView{Admin: request.User().Role == db.RoleAdmin}
	notFound := "This activity doesn’t exist, or you can’t see its server."
	job, err := s.App.Runner.Get(r.Context(), r.PathValue("id"))
	if err != nil || job == nil {
		o.Error = notFound
		return o
	}
	if _, err := s.App.Authz.Assert(r.Context(), request.User(), job.ServerID, authz.ServerView); err != nil {
		o.Error = notFound
		return o
	}
	o.Operation = job
	name := ""
	if user, err := s.App.DB.FindUserByID(r.Context(), job.ActorID); err == nil && user != nil {
		name = user.Username
	}
	o.Actor = history.PublicActor(job.ActorID, name)
	o.CancelledBy = s.canceller(r, job)
	o.CanCancel = s.App.Jobs.CanCancel(r.Context(), request.User(), job)
	if server, err := s.App.Identity.Get(r.Context(), job.ServerID); err == nil && server != nil {
		o.ServerName = server.DisplayName
	}
	return o
}

func (s *Server) operationPage(w http.ResponseWriter, r *http.Request, request *Request) {
	o := s.operationDetail(r, request)
	status := http.StatusOK
	if o.Error != "" {
		status = http.StatusNotFound
	}
	page := s.page(r, request, "Activity details")
	page.Path = "/activity"
	render(w, r, status, views.OperationDetailPage(page, o))
}

func (s *Server) operationFragment(w http.ResponseWriter, r *http.Request, request *Request) {
	o := s.operationDetail(r, request)
	if o.Error != "" {
		s.fail(w, r, request, apperr.New("NOT_FOUND", 404, o.Error))
		return
	}
	render(w, r, http.StatusOK, views.OperationSummary(o))
}
