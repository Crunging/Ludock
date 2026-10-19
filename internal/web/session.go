package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/web/views"
)

// formLimit bounds form bodies; uploads stream separately.
const formLimit = 64 * 1024

// Request is the authenticated context of one request.
type Request struct {
	Principal *auth.Principal
	Client    auth.Client
	CSRF      string
}

// User is the session's principal.
func (r *Request) User() *db.SessionUser { return &r.Principal.User }

type contextKey struct{}

// CSRFToken binds a form token to the session. The session token is
// HttpOnly and secret, so another site cannot compute it.
func CSRFToken(sessionToken string) string {
	sum := sha256.Sum256([]byte("ludock:csrf:v1:" + sessionToken))
	return hex.EncodeToString(sum[:])
}

func (s *Server) client(r *http.Request) auth.Client {
	return auth.Client{IP: s.ClientIP.Request(r), UserAgent: r.Header.Get("User-Agent")}
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

func (s *Server) principal(r *http.Request) (*auth.Principal, error) {
	return s.Auth.Session(r.Context(), auth.SessionToken(r))
}

// readForm parses a bounded urlencoded or multipart-free form within 30 s.
func readForm(w http.ResponseWriter, r *http.Request) error {
	kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if kind != "application/x-www-form-urlencoded" {
		return nil
	}
	controller := http.NewResponseController(w)
	controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	defer controller.SetReadDeadline(time.Time{})
	r.Body = http.MaxBytesReader(w, r.Body, formLimit)
	if err := r.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return apperr.New("BODY_TOO_LARGE", 413, "Request body exceeds 64 KiB")
		}
		return apperr.New("INVALID_REQUEST", 400, "Invalid request")
	}
	return nil
}

func (s *Server) loginRedirect(w http.ResponseWriter, r *http.Request) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	next := r.URL.Path
	if r.URL.RawQuery != "" {
		next += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
}

// authed requires a session. For unsafe methods it also checks the CSRF
// token, reads the bounded form, and authenticates again: the body can
// arrive slowly, and a revocation or role change meanwhile must take effect
// before the handler acts.
func (s *Server) authed(handler func(http.ResponseWriter, *http.Request, *Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, err := s.principal(r)
		if err != nil {
			s.fail(w, r, nil, err)
			return
		}
		if principal == nil {
			s.loginRedirect(w, r)
			return
		}
		if !safeMethod(r.Method) {
			if err := readForm(w, r); err != nil {
				s.fail(w, r, nil, err)
				return
			}
			token := r.Header.Get("X-CSRF-Token")
			if token == "" {
				token = r.PostFormValue("_csrf")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(CSRFToken(principal.Token))) != 1 {
				http.Error(w, "Cross-site request rejected", http.StatusForbidden)
				return
			}
			if principal, err = s.principal(r); err != nil || principal == nil {
				s.loginRedirect(w, r)
				return
			}
		}
		request := &Request{Principal: principal, Client: s.client(r), CSRF: CSRFToken(principal.Token)}
		handler(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, request)), request)
	}
}

// admin additionally requires an administrator session.
func (s *Server) admin(handler func(http.ResponseWriter, *http.Request, *Request)) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, request *Request) {
		if err := auth.RequireAdmin(request.Principal); err != nil {
			if r.Method == http.MethodGet && !isHTMX(r) {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			s.fail(w, r, request, err)
			return
		}
		handler(w, r, request)
	})
}

// page builds the shell description for a request.
func (s *Server) page(r *http.Request, request *Request, title string) views.Page {
	role := string(request.Principal.User.Role)
	if role == "admin" {
		role = "Administrator"
	}
	return views.Page{Title: title, CSRF: request.CSRF, Username: request.Principal.User.Username,
		Admin: request.Principal.User.Role == db.RoleAdmin, RoleName: role, Path: r.URL.Path}
}

// fail renders a public error: a fragment for htmx, a page otherwise.
// Internal errors never reach the browser.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, request *Request, err error) {
	public, ok := apperr.As(err)
	if !ok {
		if errors.Is(err, context.Canceled) {
			return
		}
		log.Error("Unhandled request error", logging.F("method", r.Method), logging.F("path", r.URL.Path))
		public = apperr.New("INTERNAL", 500, "Internal server error")
	}
	if public.RetryAfterMs > 0 {
		w.Header().Set("Retry-After", itoa(max(1, (public.RetryAfterMs+999)/1000)))
	}
	if public.Status == http.StatusUnauthorized {
		s.loginRedirect(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if isHTMX(r) || request == nil {
		w.WriteHeader(public.Status)
		views.Alert("error", public.Message).Render(r.Context(), w)
		return
	}
	w.WriteHeader(public.Status)
	views.ErrorPage(s.page(r, request, "Error"), "Something went wrong", public.Message).Render(r.Context(), w)
}

func itoa(value int64) string { return strconv.FormatInt(value, 10) }
