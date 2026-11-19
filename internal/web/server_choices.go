package web

import (
	"net/http"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/web/views"
)

func (s *Server) addServersView(r *http.Request) views.AddServersView {
	var v views.AddServersView
	containers, err := s.App.Observer.Unmanaged(r.Context())
	if err != nil {
		v.Error = "Ludock can’t list containers right now. Check the Docker connection in Diagnostics."
		if _, public := apperr.As(err); public {
			v.Error = publicMessage(err)
		}
	}
	v.Containers = containers
	return v
}

// unmanagedFragment lists the containers an administrator can add.
func (s *Server) unmanagedFragment(w http.ResponseWriter, r *http.Request, request *Request) {
	render(w, r, http.StatusOK, views.AddServers(s.addServersView(r)))
}

// addServer makes Ludock manage a container an administrator picked.
func (s *Server) addServer(w http.ResponseWriter, r *http.Request, request *Request) {
	server, err := s.App.Servers.Add(r.Context(), request.User().ID, request.Principal.TokenHash,
		r.PostFormValue("container"), r.PostFormValue("game"))
	err = authz.AdminMutationError(err)
	// An account that is no longer an administrator sees no containers.
	if public, ok := apperr.As(err); ok && (public.Status == http.StatusUnauthorized || public.Status == http.StatusForbidden) {
		s.fail(w, r, request, err)
		return
	}
	v := s.addServersView(r)
	if err != nil {
		v.Error = publicMessage(err)
		render(w, r, http.StatusOK, views.AddServers(v))
		return
	}
	s.audit(r, request, "server.added", server.ID, map[string]any{"gameType": server.GameType})
	v.Added = &views.AddedServer{ID: server.ID, Name: server.DisplayName}
	w.Header().Set("HX-Trigger", "servers-changed")
	render(w, r, http.StatusOK, views.AddServers(v))
}

// removeServer takes a server out of Ludock and returns to the list.
func (s *Server) removeServer(w http.ResponseWriter, r *http.Request, request *Request) {
	server, err := s.App.Servers.Remove(r.Context(), request.User().ID, request.Principal.TokenHash, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, request, authz.AdminMutationError(err))
		return
	}
	s.audit(r, request, "server.removed", server.ID, nil)
	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusOK)
}
