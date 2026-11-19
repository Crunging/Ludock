package web

import (
	"crypto/subtle"
	"errors"
	"net/http"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/web/views"
)

// SameSite=Lax: links are opened from chat and mail, and the cookie set by
// that cross-site navigation must reach the redirect that follows it.
func setLinkCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{Name: auth.LinkCookie, Value: token, Path: "/link", MaxAge: auth.LinkTTLMs / 1000,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: auth.IsExternalHTTPS(r)})
}

func clearLinkCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: auth.LinkCookie, Value: "", Path: "/link", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: auth.IsExternalHTTPS(r)})
}

// openLink moves the token from the address into a cookie and redirects, so
// it stays out of history, referrers, and anything the next page loads.
func (s *Server) openLink(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	if _, err := s.Auth.OpenLink(r.Context(), r.PathValue("token"), s.client(r)); err != nil {
		s.renderLink(w, r, views.LinkForm{}, err)
		return
	}
	setLinkCookie(w, r, r.PathValue("token"))
	http.Redirect(w, r, "/link", http.StatusSeeOther)
}

// linkFormKey ties a link's form to the link it was opened from. The cookie
// holds only the latest link a browser opened, so a form left open in
// another tab must not set the password of a different account.
func linkFormKey(token string) string { return auth.HashToken("ludock:link-form:v1:" + token) }

func (s *Server) linkPage(w http.ResponseWriter, r *http.Request) {
	link, err := s.Auth.OpenLink(r.Context(), auth.LinkToken(r), s.client(r))
	if err != nil {
		s.renderLink(w, r, views.LinkForm{}, err)
		return
	}
	render(w, r, http.StatusOK, views.LinkPage(views.LinkForm{Key: linkFormKey(auth.LinkToken(r)), Username: link.Username,
		Purpose: link.Purpose, ExpiresAt: link.ExpiresAt, TwoFactor: link.TwoFactor}))
}

// renderLink shows the form again with err, or the one neutral answer for
// a link that doesn't work.
func (s *Server) renderLink(w http.ResponseWriter, r *http.Request, form views.LinkForm, err error) {
	public, ok := apperr.As(err)
	if !ok {
		s.fail(w, r, nil, err)
		return
	}
	if public.RetryAfterMs > 0 {
		w.Header().Set("Retry-After", itoa(max(1, (public.RetryAfterMs+999)/1000)))
	}
	if public.Code == "LINK_INVALID" || form.Username == "" {
		// The cookie of a link opened in another tab stays for that tab.
		if public.Code == "LINK_INVALID" {
			clearLinkCookie(w, r)
		}
		render(w, r, public.Status, views.LinkUnavailable(public.Message))
		return
	}
	form.Error = public.Message
	render(w, r, public.Status, views.LinkPage(form))
}

func (s *Server) useLink(w http.ResponseWriter, r *http.Request) {
	token := auth.LinkToken(r)
	if err := readForm(w, r); err != nil {
		s.renderLink(w, r, views.LinkForm{}, err)
		return
	}
	if token != "" && subtle.ConstantTimeCompare([]byte(r.PostFormValue("link")), []byte(linkFormKey(token))) != 1 {
		s.renderLink(w, r, views.LinkForm{}, apperr.New("LINK_CHANGED", 409, "You opened another link in this browser. Open the link you want to use again."))
		return
	}
	link, err := s.Auth.OpenLink(r.Context(), token, s.client(r))
	if err != nil {
		s.renderLink(w, r, views.LinkForm{}, err)
		return
	}
	form := views.LinkForm{Key: linkFormKey(token), Username: link.Username, Purpose: link.Purpose, ExpiresAt: link.ExpiresAt, TwoFactor: link.TwoFactor}
	password := r.PostFormValue("password")
	if password != r.PostFormValue("confirmation") {
		form.ConfirmationError = true
		s.renderLink(w, r, form, apperr.New("PASSWORD_MISMATCH", 400, "Passwords do not match."))
		return
	}
	principal, err := s.Auth.UseLink(r.Context(), token, password, s.client(r))
	var required *auth.CodeRequired
	if errors.As(err, &required) {
		clearLinkCookie(w, r)
		setChallengeCookie(w, r, required.Token)
		render(w, r, http.StatusOK, views.LoginPage(views.LoginForm{Code: true, PasswordSet: true}))
		return
	}
	if err != nil {
		s.renderLink(w, r, form, err)
		return
	}
	clearLinkCookie(w, r)
	setSessionCookie(w, r, principal.Token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
