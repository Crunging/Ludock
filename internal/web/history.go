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

var historyFilters = []string{"serverId", "actor", "action", "status", "from", "to", "operationId"}

// historyQuery reads the applied filters and cursor from the URL.
func historyQuery(r *http.Request, audit bool) (history.Query, url.Values, error) {
	source := r.URL.Query()
	filters := url.Values{}
	for _, name := range historyFilters {
		if value := strings.TrimSpace(source.Get(name)); value != "" && (name != "operationId" || audit) {
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
		return q, filters, apperr.New("INVALID_REQUEST", 400, "From must be before or equal to To.")
	}
	return q, filters, nil
}

// historyServers names the servers the viewer can see, from saved metadata.
func (s *Server) historyServers(r *http.Request, request *Request, h *views.HistoryView) {
	h.Names = map[string]string{}
	list, err := s.App.Servers.ListSaved(r.Context(), request.User())
	if err != nil {
		h.NamesError = true
		return
	}
	for _, server := range list {
		h.Servers = append(h.Servers, views.HistoryServer{ID: server.ID, Name: server.DisplayName})
		h.Names[server.ID] = server.DisplayName
	}
}

func (s *Server) operationsPage(w http.ResponseWriter, r *http.Request, request *Request) {
	q, filters, err := historyQuery(r, false)
	h := views.HistoryView{Filters: filters, Cursor: q.Cursor}
	s.historyServers(r, request, &h)
	if err == nil {
		h.Operations, h.Next, err = s.App.History.Operations(r.Context(), request.User(), q)
	}
	if err != nil {
		h.Error = publicMessage(err)
	}
	render(w, r, http.StatusOK, views.OperationsPage(s.page(r, request, "Operation history"), h))
}

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request, request *Request) {
	q, filters, err := historyQuery(r, true)
	h := views.HistoryView{Audit: true, Filters: filters, Cursor: q.Cursor}
	s.historyServers(r, request, &h)
	if err == nil {
		h.Entries, h.Next, err = s.App.History.Audit(r.Context(), q)
	}
	if err != nil {
		h.Error = publicMessage(err)
	}
	render(w, r, http.StatusOK, views.AuditPage(s.page(r, request, "Audit log"), h))
}

// operationDetail reads one operation the viewer may see. A hidden server's
// operation looks the same as a missing one.
func (s *Server) operationDetail(r *http.Request, request *Request) views.OperationDetailView {
	o := views.OperationDetailView{Admin: request.User().Role == db.RoleAdmin}
	notFound := "Operation not found"
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
	page := s.page(r, request, "Operation details")
	page.Path = "/operations"
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
