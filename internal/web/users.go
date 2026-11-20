package web

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/web/views"
)

func (s *Server) usersView(r *http.Request, request *Request) views.UsersView {
	u := views.UsersView{CurrentID: request.User().ID, Reach: map[string][]views.Reach{}}
	users, err := s.App.DB.ListUsers(r.Context())
	if err != nil {
		u.Error = publicMessage(err)
	}
	u.Users = users
	names, err := s.App.Servers.Names(r.Context(), request.User())
	if err != nil {
		return u
	}
	for _, user := range users {
		if user.Role != db.RoleMember {
			continue
		}
		grants, err := s.App.Authz.UserGrants(r.Context(), user.ID)
		if err != nil {
			continue
		}
		for _, name := range names {
			for _, grant := range grants {
				if level := authz.LevelOf(grant.Capabilities); grant.ServerID == name.ID && level != authz.LevelNone {
					u.Reach[user.ID] = append(u.Reach[user.ID], views.Reach{ServerID: name.ID, Name: name.DisplayName, Level: level})
				}
			}
		}
	}
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
	result := &views.LinkResult{Username: username, URL: origin + "/link/" + link.Token, Purpose: link.Purpose, ExpiresAt: link.ExpiresAt}
	// The link uses the address the administrator opened Ludock with, which
	// may be one only they can reach.
	if parsed, err := url.Parse(origin); err == nil {
		result.Host = parsed.Hostname()
		result.ThisComputer, result.LocalNetwork = loopbackHost(result.Host), localHost(result.Host)
	}
	return result
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
		user, link, err = s.App.Auth.InviteUser(r.Context(), request.Principal, username, role, request.Client, nil)
	}
	if err != nil {
		s.usersResult(w, r, request, err, "", draft)
		return
	}
	notice := "Created " + user.Username + ". Share servers with them from each server’s Sharing tab."
	if user.Role == db.RoleAdmin {
		notice = "Created " + user.Username + ". Administrators can use every server and setting."
	}
	s.usersResult(w, r, request, nil, notice, func(u *views.UsersView) {
		if link != nil {
			u.Link = linkView(r, user.Username, link)
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
	var disabled *bool
	switch r.PostFormValue("disabled") {
	case "true":
		disabled = new(bool)
		*disabled = true
	case "false":
		disabled = new(bool)
	}
	user, err := s.App.Auth.ChangeAccess(r.Context(), request.Principal, r.PathValue("id"), db.Role(r.PostFormValue("role")),
		disabled, request.Client)
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
