package web

import (
	"net/http"
	"strings"
	"unicode"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/web/views"
)

const sessionMaxAge = 7 * 24 * 60 * 60

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{Name: auth.SessionCookie, Value: token, Path: "/", MaxAge: sessionMaxAge, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: auth.IsExternalHTTPS(r)})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: auth.SessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: auth.IsExternalHTTPS(r)})
}

// safeNext keeps redirects on this site. Browsers drop tabs and newlines from
// URLs, so "/\t/evil.example" would leave it; control characters are refused.
func safeNext(next string) string {
	if len(next) == 0 || next[0] != '/' {
		return "/"
	}
	// "//host" and "/\host" leave the site. The backslash case is also refused
	// below; this explicit form is the one CodeQL recognizes.
	if len(next) > 1 && (next[1] == '/' || next[1] == '\\') {
		return "/"
	}
	if strings.ContainsRune(next, '\\') || strings.ContainsFunc(next, unicode.IsControl) || strings.HasPrefix(next, "/login") {
		return "/"
	}
	return next
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if principal, _ := s.principal(r); principal != nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	state, err := s.Auth.SetupState(r.Context())
	if err != nil {
		s.fail(w, r, nil, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if state.Required && state.Locked {
		views.SetupExpired().Render(r.Context(), w)
		return
	}
	next := r.URL.Query().Get("next")
	if next != "" {
		next = safeNext(next)
	}
	views.LoginPage(views.LoginForm{Setup: state.Required, Next: next}).Render(r.Context(), w)
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, form views.LoginForm, err error) {
	status := http.StatusBadRequest
	if public, ok := apperr.As(err); ok {
		status = public.Status
		form.Error = public.Message
		if public.RetryAfterMs > 0 {
			w.Header().Set("Retry-After", itoa(max(1, (public.RetryAfterMs+999)/1000)))
		}
		if public.Code == "SETUP_LOCKED" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(status)
			views.SetupExpired().Render(r.Context(), w)
			return
		}
	} else if err != nil {
		s.fail(w, r, nil, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	views.LoginPage(form).Render(r.Context(), w)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if err := readForm(w, r); err != nil {
		s.renderLogin(w, r, views.LoginForm{}, err)
		return
	}
	username := r.PostFormValue("username")
	form := views.LoginForm{Username: strings.TrimSpace(username), Next: safeNext(r.PostFormValue("next"))}
	principal, err := s.Auth.Login(r.Context(), username, r.PostFormValue("password"), s.client(r))
	if err != nil {
		s.renderLogin(w, r, form, err)
		return
	}
	setSessionCookie(w, r, principal.Token)
	http.Redirect(w, r, form.Next, http.StatusSeeOther)
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	form := views.LoginForm{Setup: true}
	if err := readForm(w, r); err != nil {
		s.renderLogin(w, r, form, err)
		return
	}
	form.Username = strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	if password != r.PostFormValue("confirmation") {
		form.ConfirmationError = true
		s.renderLogin(w, r, form, apperr.New("PASSWORD_MISMATCH", 400, "Passwords do not match."))
		return
	}
	principal, err := s.Auth.CompleteSetup(r.Context(), r.PostFormValue("username"), password, r.PostFormValue("bootstrapCode"), s.client(r))
	if err != nil {
		s.renderLogin(w, r, form, err)
		return
	}
	setSessionCookie(w, r, principal.Token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, request *Request) {
	// Keep the cookie until the session is gone, so a failed sign-out can be
	// retried from this browser instead of leaving a live session behind.
	if err := s.Auth.Logout(r.Context(), request.Principal.Token, request.Client); err != nil {
		log.Error("Could not end a session")
		s.fail(w, r, request, apperr.New("LOGOUT_FAILED", 503, "Ludock could not end this session. Try signing out again."))
		return
	}
	clearSessionCookie(w, r)
	w.Header().Set("Clear-Site-Data", `"cache", "cookies", "storage"`)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
