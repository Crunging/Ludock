package web

import (
	"net/http"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/web/views"
)

func (s *Server) accountView(r *http.Request, request *Request) views.AccountView {
	sessions, err := s.Auth.Sessions(r.Context(), request.Principal)
	view := views.AccountView{Sessions: sessions}
	if err != nil {
		view.SessionsError = publicMessage(err)
	}
	return view
}

func (s *Server) account(w http.ResponseWriter, r *http.Request, request *Request) {
	view := s.accountView(r, request)
	if r.URL.Query().Get("changed") == "1" {
		view.Message = "Password changed. Other sessions have been signed out."
	}
	render(w, r, http.StatusOK, views.AccountPage(s.page(r, request, "Account"), view))
}

func (s *Server) accountSessions(w http.ResponseWriter, r *http.Request, request *Request) {
	render(w, r, http.StatusOK, views.AccountSessions(s.accountView(r, request)))
}

// changePassword replaces the session: the browser gets the new cookie and
// a fresh page whose forms carry the new CSRF token.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, request *Request) {
	fail := func(err error) {
		public, ok := apperr.As(err)
		if !ok || public.Status == http.StatusUnauthorized {
			s.fail(w, r, request, err)
			return
		}
		if public.RetryAfterMs > 0 {
			w.Header().Set("Retry-After", itoa(max(1, (public.RetryAfterMs+999)/1000)))
		}
		view := s.accountView(r, request)
		view.Error = public.Message
		page := s.page(r, request, "Account")
		page.Path = "/account"
		render(w, r, public.Status, views.AccountPage(page, view))
	}
	newPassword := r.PostFormValue("newPassword")
	if newPassword != r.PostFormValue("confirmPassword") {
		fail(apperr.New("PASSWORD_MISMATCH", 400, "New passwords do not match."))
		return
	}
	principal, err := s.Auth.ChangePassword(r.Context(), request.Principal, r.PostFormValue("currentPassword"), newPassword, request.Client)
	if err != nil {
		fail(err)
		return
	}
	setSessionCookie(w, r, principal.Token)
	http.Redirect(w, r, "/account?changed=1", http.StatusSeeOther)
}

// revokeSession ends one of the account's sessions; ending this one signs
// the browser out.
func (s *Server) revokeSession(w http.ResponseWriter, r *http.Request, request *Request) {
	err := s.Auth.RevokeSession(r.Context(), request.Principal, r.PathValue("id"), request.Client)
	if err == nil {
		if current, _ := s.principal(r); current == nil {
			clearSessionCookie(w)
			s.loginRedirect(w, r)
			return
		}
	}
	view := s.accountView(r, request)
	if err != nil {
		if public, ok := apperr.As(err); !ok || public.Status == http.StatusUnauthorized {
			s.fail(w, r, request, err)
			return
		}
		view.Error = publicMessage(err)
	} else {
		view.Message = "Session revoked."
	}
	render(w, r, http.StatusOK, views.AccountSessions(view), views.AccountStatus(view, true))
}
