package web

import (
	"net/http"
	"strings"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/web/views"
)

func (s *Server) usersView(r *http.Request, request *Request) views.UsersView {
	u := views.UsersView{CurrentID: request.User().ID}
	users, err := s.App.DB.ListUsers(r.Context())
	if err != nil {
		u.Error = publicMessage(err)
	}
	u.Users = users
	return u
}

func (s *Server) usersPage(w http.ResponseWriter, r *http.Request, request *Request) {
	render(w, r, http.StatusOK, views.UsersPage(s.page(r, request, "Users"), s.usersView(r, request)))
}

// usersResult re-renders the users region with the outcome of a change.
// Errors keep the region (and the create form's draft) in place.
func (s *Server) usersResult(w http.ResponseWriter, r *http.Request, request *Request, err error, notice string, change func(*views.UsersView)) {
	if public, ok := apperr.As(err); ok && public.Status == http.StatusUnauthorized {
		s.fail(w, r, request, err)
		return
	}
	u := s.usersView(r, request)
	status := http.StatusOK
	if err != nil {
		u.Error = publicMessage(err)
		if public, ok := apperr.As(err); ok {
			status = public.Status
		} else {
			status = http.StatusInternalServerError
		}
	} else {
		u.Notice = notice
	}
	if change != nil {
		change(&u)
	}
	render(w, r, status, views.UsersContent(u))
}

// errNoOrigin refuses a link when the request doesn't say which address the
// administrator is using; a link is never built from the Host header.
var errNoOrigin = apperr.New("ORIGIN_REQUIRED", 400, "Ludock couldn't tell which address you're using. Reload the page and try again.")

// linkView describes a link that was just made; it is shown only this once.
func linkView(r *http.Request, username string, link *auth.Link) *views.LinkResult {
	origin, _ := auth.RequestOrigin(r)
	return &views.LinkResult{Username: username, URL: origin + "/link/" + link.Token, Purpose: link.Purpose, ExpiresAt: link.ExpiresAt}
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, request *Request) {
	username, role := r.PostFormValue("username"), db.Role(r.PostFormValue("role"))
	draft := func(u *views.UsersView) {
		u.DraftName, u.DraftRole, u.DraftPassword = strings.TrimSpace(username), role, r.PostFormValue("access") == "password"
	}
	var user *db.User
	var link *auth.Link
	var err error
	if r.PostFormValue("access") == "password" {
		user, err = s.App.Auth.CreateUser(r.Context(), request.Principal, username, r.PostFormValue("password"), role, request.Client)
	} else if _, ok := auth.RequestOrigin(r); !ok {
		err = errNoOrigin
	} else {
		user, link, err = s.App.Auth.InviteUser(r.Context(), request.Principal, username, role, request.Client)
	}
	if err != nil {
		s.usersResult(w, r, request, err, "", draft)
		return
	}
	notice := "Created " + user.Username + ". Share servers using their Server access controls below."
	if user.Role == db.RoleAdmin {
		notice = "Created " + user.Username + ". Administrators can manage all servers and settings."
	}
	s.usersResult(w, r, request, nil, notice, func(u *views.UsersView) {
		if link != nil {
			u.Link = linkView(r, user.Username, link)
		}
		if user.Role != db.RoleAdmin {
			u.OpenGrants = user.ID
		}
	})
}

func (s *Server) createUserLink(w http.ResponseWriter, r *http.Request, request *Request) {
	id := r.PathValue("id")
	if _, ok := auth.RequestOrigin(r); !ok {
		s.usersResult(w, r, request, errNoOrigin, "", nil)
		return
	}
	link, err := s.App.Auth.CreateLink(r.Context(), request.Principal, id, request.Client)
	if err != nil {
		s.usersResult(w, r, request, err, "", nil)
		return
	}
	name := "the user"
	if user, _ := s.App.DB.FindUserByID(r.Context(), id); user != nil {
		name = user.Username
	}
	s.usersResult(w, r, request, nil, "", func(u *views.UsersView) { u.Link = linkView(r, name, link) })
}

func (s *Server) revokeUserLink(w http.ResponseWriter, r *http.Request, request *Request) {
	id := r.PathValue("id")
	err := s.App.Auth.RevokeLink(r.Context(), request.Principal, id, request.Client)
	notice := ""
	if err == nil {
		notice = "Link revoked. It no longer works."
		if user, _ := s.App.DB.FindUserByID(r.Context(), id); user != nil {
			notice = "Revoked the link for " + user.Username + ". It no longer works."
		}
	}
	s.usersResult(w, r, request, err, notice, nil)
}

func (s *Server) updateUserAccess(w http.ResponseWriter, r *http.Request, request *Request) {
	user, err := s.App.Auth.UpdateAccess(r.Context(), request.Principal, r.PathValue("id"), db.Role(r.PostFormValue("role")),
		r.PostFormValue("disabled") == "true", request.Client)
	notice := ""
	if err == nil {
		notice = "Updated access for " + user.Username + "."
	}
	s.usersResult(w, r, request, err, notice, nil)
}

func (s *Server) resetUserPassword(w http.ResponseWriter, r *http.Request, request *Request) {
	id := r.PathValue("id")
	err := s.App.Auth.ResetPassword(r.Context(), request.Principal, id, r.PostFormValue("password"), request.Client)
	notice := ""
	if err == nil {
		name := "the user"
		if user, _ := s.App.DB.FindUserByID(r.Context(), id); user != nil {
			name = user.Username
		}
		notice = "Password reset for " + name + ". Share the new password securely."
	}
	s.usersResult(w, r, request, err, notice, nil)
}

func (s *Server) resetUserTwoFactor(w http.ResponseWriter, r *http.Request, request *Request) {
	id := r.PathValue("id")
	err := s.App.Auth.ResetTwoFactor(r.Context(), request.Principal, id, request.Client)
	notice := ""
	if err == nil {
		name := "the user"
		if user, _ := s.App.DB.FindUserByID(r.Context(), id); user != nil {
			name = user.Username
		}
		notice = "Two-factor sign-in turned off for " + name + ". They were signed out and can set it up again in Account."
	}
	s.usersResult(w, r, request, err, notice, nil)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, request *Request) {
	id := r.PathValue("id")
	name := ""
	if user, _ := s.App.DB.FindUserByID(r.Context(), id); user != nil {
		name = user.Username
	}
	err := s.App.Auth.DeleteUser(r.Context(), request.Principal, id, request.Client)
	s.usersResult(w, r, request, err, "Deleted "+name+".", nil)
}

// grantsView reads a user's server access. Discovery runs first so newly
// found servers can be shared.
func (s *Server) grantsView(r *http.Request, request *Request, userID string) (views.GrantsView, error) {
	user, err := s.App.DB.FindUserByID(r.Context(), userID)
	if err != nil {
		return views.GrantsView{}, err
	}
	if user == nil {
		return views.GrantsView{}, apperr.New("NOT_FOUND", 404, "User not found")
	}
	g := views.GrantsView{UserID: user.ID, Username: user.Username, Role: user.Role, Selected: map[string][]authz.Capability{}}
	_, err = s.App.Servers.Refresh(r.Context())
	var names []servers.Name
	if err == nil {
		names, err = s.App.Servers.Names(r.Context(), request.User())
	}
	if err != nil {
		g.Error = "Unable to load server access."
		return g, nil
	}
	grants, err := s.App.Authz.UserGrants(r.Context(), user.ID)
	if err != nil {
		g.Error = publicMessage(err)
		return g, nil
	}
	for _, server := range names {
		g.Servers = append(g.Servers, views.HistoryServer{ID: server.ID, Name: server.DisplayName})
	}
	ceiling := authz.RoleCeiling(user.Role)
	for _, grant := range grants {
		for _, capability := range grant.Capabilities {
			if contains(ceiling, capability) {
				g.Selected[grant.ServerID] = append(g.Selected[grant.ServerID], capability)
			}
		}
	}
	g.Loaded = true
	return g, nil
}

func contains[T comparable](values []T, value T) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func (s *Server) userGrants(w http.ResponseWriter, r *http.Request, request *Request) {
	g, err := s.grantsView(r, request, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	render(w, r, http.StatusOK, views.GrantsEditor(g))
}

func (s *Server) saveUserGrants(w http.ResponseWriter, r *http.Request, request *Request) {
	userID := r.PathValue("id")
	inputs := views.GrantInputs(r.PostForm)
	// Grants name logical servers; discovery applies binding changes first.
	_, err := s.App.Servers.RefreshNow(r.Context())
	if err == nil {
		_, err = s.App.Authz.SetUserGrants(r.Context(), userID, inputs, request.User(), request.Principal.TokenHash)
	}
	if public, ok := apperr.As(err); ok && public.Status == http.StatusUnauthorized {
		s.fail(w, r, request, err)
		return
	}
	g, viewErr := s.grantsView(r, request, userID)
	if viewErr != nil {
		s.fail(w, r, request, viewErr)
		return
	}
	if err != nil {
		g.Error = publicMessage(err)
		// Keep the submitted choices on screen so nothing is silently lost.
		g.Selected = map[string][]authz.Capability{}
		for _, input := range inputs {
			g.Selected[input.ServerID] = input.Capabilities
		}
	} else {
		g.Notice = "Server access saved for " + g.Username + ". Access changes take effect immediately."
	}
	render(w, r, http.StatusOK, views.GrantsEditor(g))
}
