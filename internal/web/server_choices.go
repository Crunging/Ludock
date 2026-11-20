package web

import (
	"net/http"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/web/views"
)

// addServersView lists addable containers, or returns an error once the
// session is no longer an administrator's.
func (s *Server) addServersView(r *http.Request, request *Request) (views.AddServersView, error) {
	var v views.AddServersView
	containers, err := s.App.Observer.Unmanaged(r.Context())
	// Listing reads Docker; the account may have changed meanwhile.
	if err := s.recheckAdmin(r.Context(), request); err != nil {
		return v, err
	}
	if err != nil {
		v.Error = "Ludock can’t list containers right now. Check the connection in Settings → System."
		if _, public := apperr.As(err); public {
			v.Error = publicMessage(err)
		}
	}
	v.Containers = containers
	return v, nil
}

// unmanagedFragment lists the containers an administrator can add.
func (s *Server) unmanagedFragment(w http.ResponseWriter, r *http.Request, request *Request) {
	v, err := s.addServersView(r, request)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	render(w, r, http.StatusOK, views.AddServers(v))
}

// addServer makes Ludock manage a container an administrator picked.
func (s *Server) addServer(w http.ResponseWriter, r *http.Request, request *Request) {
	server, err := s.App.Servers.Add(r.Context(), request.User().ID, request.Principal.TokenHash,
		r.PostFormValue("container"), r.PostFormValue("game"), s.auditEntry(request))
	err = authz.AdminMutationError(err)
	// An account that is no longer an administrator sees no containers.
	if public, ok := apperr.As(err); ok && (public.Status == http.StatusUnauthorized || public.Status == http.StatusForbidden) {
		s.fail(w, r, request, err)
		return
	}
	v, viewErr := s.addServersView(r, request)
	if viewErr != nil {
		s.fail(w, r, request, viewErr)
		return
	}
	if err != nil {
		v.Error = publicMessage(err)
		render(w, r, http.StatusOK, views.AddServers(v))
		return
	}
	v.Added = &views.AddedServer{ID: server.ID, Name: server.DisplayName}
	w.Header().Set("HX-Trigger", "servers-changed")
	render(w, r, http.StatusOK, views.AddServers(v))
}

// removeServer takes a server out of Ludock and returns to the list.
func (s *Server) removeServer(w http.ResponseWriter, r *http.Request, request *Request) {
	_, err := s.App.Servers.Remove(r.Context(), request.User().ID, request.Principal.TokenHash, r.PathValue("id"), s.auditEntry(request))
	if err != nil {
		s.fail(w, r, request, authz.AdminMutationError(err))
		return
	}
	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) auditEntry(request *Request) db.AuditEntry {
	return db.AuditEntry{UserID: request.User().ID, IPAddress: request.Client.IP}
}
