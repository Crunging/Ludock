package web

import (
	"database/sql"
	"net/http"
	"slices"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/web/views"
)

// sharingView reads who a server is shared with, for administrators.
func (s *Server) sharingView(r *http.Request, request *Request, serverID string) (views.SharingView, error) {
	server, err := s.App.Authz.Assert(r.Context(), request.User(), serverID, authz.ServerView)
	if err != nil {
		return views.SharingView{}, err
	}
	v := views.SharingView{ServerID: server.ID, ServerName: server.DisplayName, Paused: server.SharingPaused}
	if current, err := s.App.Servers.Refresh(r.Context()); err == nil {
		v.ControlsHost = current[server.ContainerID].ControlsHost
	}
	if v.Shares, err = s.App.Authz.Sharing(r.Context(), server.ID); err != nil {
		return v, err
	}
	users, err := s.App.DB.ListUsers(r.Context())
	if err != nil {
		return v, err
	}
	for _, user := range users {
		shared := slices.ContainsFunc(v.Shares, func(share authz.Share) bool { return share.UserID == user.ID })
		if user.Role == db.RoleMember && !user.Disabled && !shared {
			v.People = append(v.People, user)
		}
	}
	return v, nil
}

// sharingResult re-renders the Sharing tab with the outcome of a change.
// Errors keep the share form's choices.
func (s *Server) sharingResult(w http.ResponseWriter, r *http.Request, request *Request, err error, change func(*views.SharingView)) {
	// An account that is no longer an administrator is shown no list.
	if s.adminRefused(w, r, request, err) {
		return
	}
	v, viewErr := s.sharingView(r, request, r.PathValue("id"))
	if viewErr != nil {
		s.fail(w, r, request, viewErr)
		return
	}
	status := http.StatusOK
	if err != nil {
		v.Error, status = publicMessage(err), http.StatusInternalServerError
		if public, ok := apperr.As(err); ok {
			status = public.Status
		}
	}
	if change != nil {
		change(&v)
	}
	// Building the list waits on Docker; a refused change shows it only to
	// an administrator still.
	if err != nil {
		s.renderAdmin(w, r, request, status, views.SharingContent(v))
		return
	}
	render(w, r, status, views.SharingContent(v))
}

var errNoLevel = apperr.New("INVALID_GRANT", 400, "Choose what they can do")

// shareServer shares a server with a member, or with someone new: their
// account, invite link, and access are made together.
func (s *Server) shareServer(w http.ResponseWriter, r *http.Request, request *Request) {
	serverID, person, username := r.PathValue("id"), r.PostFormValue("person"), r.PostFormValue("username")
	level := authz.Level(r.PostFormValue("level"))
	notice, link, err := s.share(r, request, serverID, person, username, level)
	s.sharingResult(w, r, request, err, func(v *views.SharingView) {
		if err != nil {
			v.DraftPerson, v.DraftName, v.DraftLevel = person, username, level
			v.DraftCustom, _ = views.ShareCapabilities(authz.LevelCustom, r.PostForm["capability"])
			return
		}
		v.Notice, v.Link = notice, link
	})
}

func (s *Server) share(r *http.Request, request *Request, serverID, person, username string, level authz.Level) (string, *views.LinkResult, error) {
	capabilities, ok := views.ShareCapabilities(level, r.PostForm["capability"])
	if !ok {
		return "", nil, errNoLevel
	}
	// Discovery applies binding changes, such as paused sharing, first.
	if _, err := s.App.Servers.RefreshNow(r.Context()); err != nil {
		return "", nil, err
	}
	if person != "new" {
		if err := s.App.Authz.Share(r.Context(), request.User(), request.Principal.TokenHash, person, serverID, capabilities); err != nil {
			return "", nil, err
		}
		return "Shared with " + s.username(r, person) + ". They can use it right away.", nil, nil
	}
	if _, ok := auth.RequestOrigin(r); !ok {
		return "", nil, errNoOrigin
	}
	user, link, err := s.App.Auth.InviteUser(r.Context(), request.Principal, username, db.RoleMember, request.Client,
		func(tx *sql.Tx, id string) error {
			return s.App.Authz.ShareIn(r.Context(), tx, request.User().ID, id, serverID, capabilities)
		})
	if err != nil {
		return "", nil, err
	}
	return "Shared with " + user.Username + ". Send them this link to choose a password and sign in.", linkView(r, user.Username, link), nil
}

func (s *Server) username(r *http.Request, id string) string {
	if user, _ := s.App.DB.FindUserByID(r.Context(), id); user != nil {
		return user.Username
	}
	return "them"
}

// changeShare changes or ends a member's access to a server.
func (s *Server) changeShare(w http.ResponseWriter, r *http.Request, request *Request) {
	serverID, userID := r.PathValue("id"), r.PathValue("user")
	level := authz.Level(r.PostFormValue("level"))
	var capabilities []authz.Capability
	ok := level == "none"
	if !ok {
		capabilities, ok = views.ShareCapabilities(level, r.PostForm["capability"])
	}
	var err error
	if !ok {
		err = errNoLevel
	} else if _, err = s.App.Servers.RefreshNow(r.Context()); err == nil {
		err = s.App.Authz.Share(r.Context(), request.User(), request.Principal.TokenHash, userID, serverID, capabilities)
	}
	name := s.username(r, userID)
	s.sharingResult(w, r, request, err, func(v *views.SharingView) {
		switch {
		case err != nil:
		case level == "none":
			v.Notice = "Stopped sharing with " + name + "."
		default:
			v.Notice = "Changed " + name + "’s access to “" + views.LevelName(authz.LevelOf(capabilities)) + "”."
		}
	})
}
