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
	on, left, err := s.Auth.TwoFactorStatus(r.Context(), request.Principal)
	view.TwoFactor = views.TwoFactorView{On: on, RecoveryLeft: left}
	if err != nil {
		view.Error = publicMessage(err)
	}
	return view
}

func (s *Server) account(w http.ResponseWriter, r *http.Request, request *Request) {
	view := s.accountView(r, request)
	switch {
	case r.URL.Query().Get("changed") == "1":
		view.Message = "Password changed. Other sessions have been signed out."
	case r.URL.Query().Get("two-factor") == "off":
		view.Message = "Two-factor sign-in is off. Other sessions have been signed out."
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
			clearSessionCookie(w, r)
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
	render(w, r, http.StatusOK, views.AccountSessions(view), views.AccountStatusUpdate(view))
}

// accountResult renders the account page after a form post, with change
// applied to its view. A 401 signs the browser out instead.
func (s *Server) accountResult(w http.ResponseWriter, r *http.Request, request *Request, err error, change func(*views.AccountView)) {
	status := http.StatusOK
	public, ok := apperr.As(err)
	if err != nil {
		if !ok || public.Status == http.StatusUnauthorized {
			s.fail(w, r, request, err)
			return
		}
		if public.RetryAfterMs > 0 {
			w.Header().Set("Retry-After", itoa(max(1, (public.RetryAfterMs+999)/1000)))
		}
		status = public.Status
	}
	view := s.accountView(r, request)
	if err != nil {
		view.Error = public.Message
	}
	if change != nil {
		change(&view)
	}
	page := s.page(r, request, "Account")
	page.Path = "/account"
	render(w, r, status, views.AccountPage(page, view))
}

func (s *Server) beginTwoFactor(w http.ResponseWriter, r *http.Request, request *Request) {
	enrollment, err := s.Auth.BeginTwoFactor(r.Context(), request.Principal)
	s.accountResult(w, r, request, err, func(view *views.AccountView) { view.TwoFactor.Enrollment = enrollment })
}

func (s *Server) cancelTwoFactor(w http.ResponseWriter, r *http.Request, request *Request) {
	s.accountResult(w, r, request, s.Auth.CancelTwoFactor(r.Context(), request.Principal), nil)
}

// enableTwoFactor replaces the session like a password change, and shows the
// recovery codes on the page it returns: they can't be fetched again.
func (s *Server) enableTwoFactor(w http.ResponseWriter, r *http.Request, request *Request) {
	principal, codes, err := s.Auth.EnableTwoFactor(r.Context(), request.Principal, r.PostFormValue("currentPassword"),
		r.PostFormValue("code"), request.Client)
	if err != nil {
		enrollment, pendingErr := s.Auth.PendingTwoFactor(r.Context(), request.Principal)
		if pendingErr != nil {
			err = pendingErr
		}
		s.accountResult(w, r, request, err, func(view *views.AccountView) { view.TwoFactor.Enrollment = enrollment })
		return
	}
	setSessionCookie(w, r, principal.Token)
	replaced := &Request{Principal: principal, Client: request.Client, CSRF: CSRFToken(principal.Token)}
	s.accountResult(w, r, replaced, nil, func(view *views.AccountView) {
		view.TwoFactor.RecoveryCodes = codes
		view.Message = "Two-factor sign-in is on. Other sessions have been signed out."
	})
}

func (s *Server) disableTwoFactor(w http.ResponseWriter, r *http.Request, request *Request) {
	principal, err := s.Auth.DisableTwoFactor(r.Context(), request.Principal, r.PostFormValue("currentPassword"), request.Client)
	if err != nil {
		s.accountResult(w, r, request, err, nil)
		return
	}
	setSessionCookie(w, r, principal.Token)
	http.Redirect(w, r, "/account?two-factor=off", http.StatusSeeOther)
}
