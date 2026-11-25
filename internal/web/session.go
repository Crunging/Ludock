package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/selfupdate"
	"github.com/Crunging/Ludock/internal/web/views"
)

// formLimit bounds form bodies; uploads stream separately.
const formLimit = 64 * 1024

// formDeadline bounds reading a form; tests shorten it.
var formDeadline = 30 * time.Second

type Request struct {
	Principal *auth.Principal
	Client    auth.Client
	CSRF      string
}

func (r *Request) User() *db.SessionUser { return &r.Principal.User }

type contextKey struct{}

// CSRFToken binds a form token to the session. The session token is
// HttpOnly and secret, so another site cannot compute it.
func CSRFToken(sessionToken string) string {
	sum := sha256.Sum256([]byte("ludock:csrf:v1:" + sessionToken))
	return hex.EncodeToString(sum[:])
}

// maskCSRF hides the token under a fresh random pad for each page. Pages are
// compressed, and a token that changed with every response can't be guessed
// from compressed sizes (BREACH).
func maskCSRF(token string) string {
	raw, err := hex.DecodeString(token)
	if err != nil {
		return token
	}
	masked := make([]byte, 2*len(raw))
	rand.Read(masked[:len(raw)])
	for index, value := range raw {
		masked[len(raw)+index] = masked[index] ^ value
	}
	return hex.EncodeToString(masked)
}

// csrfMatches accepts the session's token, masked or not.
func csrfMatches(sent, token string) bool {
	if masked, err := hex.DecodeString(sent); err == nil && len(masked) == len(token) {
		half := len(masked) / 2
		for index := range half {
			masked[half+index] ^= masked[index]
		}
		sent = hex.EncodeToString(masked[half:])
	}
	return subtle.ConstantTimeCompare([]byte(sent), []byte(token)) == 1
}

func (s *Server) client(r *http.Request) auth.Client {
	return auth.Client{IP: s.ClientIP.Request(r), UserAgent: r.Header.Get("User-Agent")}
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// htmxSource and htmxTarget are the ids of the element that sent an htmx
// request and of the one its response goes into. htmx sends each as "tag#id".
func htmxSource(r *http.Request) string { return elementID(r.Header.Get("HX-Source")) }

func htmxTarget(r *http.Request) string { return elementID(r.Header.Get("HX-Target")) }

func elementID(identifier string) string {
	_, id, _ := strings.Cut(identifier, "#")
	return id
}

func (s *Server) principal(r *http.Request) (*auth.Principal, error) {
	return s.Auth.Session(r.Context(), auth.SessionToken(r))
}

// readForm parses a bounded urlencoded form within 30 s. Multipart bodies
// are refused: parsing one would read past the limit into temporary files.
// Raw uploads (application/octet-stream) are left for their handler, which
// enforces the upload size.
func readForm(w http.ResponseWriter, r *http.Request) error {
	kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if strings.HasPrefix(kind, "multipart/") {
		return apperr.New("UNSUPPORTED_MEDIA_TYPE", 415, "Forms must be sent URL-encoded")
	}
	if kind == "application/octet-stream" {
		return nil
	}
	controller := http.NewResponseController(w)
	if r.ContentLength != 0 {
		controller.SetReadDeadline(time.Now().Add(formDeadline))
	}
	r.Body = http.MaxBytesReader(w, r.Body, formLimit)
	if err := r.ParseForm(); err != nil {
		// The deadline stays: Go reads the rest of the body before answering.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return apperr.New("BODY_TOO_LARGE", 413, "Request body exceeds 64 KiB")
		}
		return apperr.New("INVALID_REQUEST", 400, "Invalid request")
	}
	// Only a URL-encoded body is read to its end, after which a passing
	// deadline would cancel the request.
	if r.ContentLength != 0 && kind == "application/x-www-form-urlencoded" {
		controller.SetReadDeadline(time.Time{})
	}
	return nil
}

func (s *Server) loginRedirect(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != "" {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}
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
			if !csrfMatches(token, CSRFToken(principal.Token)) {
				http.Error(w, "Cross-site request rejected", http.StatusForbidden)
				return
			}
			if principal, err = s.principal(r); err != nil || principal == nil {
				s.loginRedirect(w, r)
				return
			}
			if s.updating() {
				s.fail(w, r, nil, selfupdate.ErrUpdating)
				return
			}
		}
		request := &Request{Principal: principal, Client: s.client(r), CSRF: CSRFToken(principal.Token)}
		handler(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, request)), request)
	}
}

// updating reports that Ludock is updating itself, when changes are
// refused: putting a failed update back restores the data saved before it,
// which would undo them, such as a sign-out or a password set.
func (s *Server) updating() bool {
	return s.App != nil && s.App.SelfUpdate != nil && s.App.SelfUpdate.Updating()
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

func (s *Server) page(r *http.Request, request *Request, title string) views.Page {
	role := "Member"
	if request.Principal.User.Role == db.RoleAdmin {
		role = "Administrator"
	}
	return views.Page{Title: title, CSRF: maskCSRF(request.CSRF), Username: request.Principal.User.Username,
		Admin: request.Principal.User.Role == db.RoleAdmin, RoleName: role, Path: r.URL.Path}
}

// fail renders a public error: a fragment for htmx, a page otherwise.
// Internal errors never reach the browser.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, request *Request, err error) {
	if _, ok := apperr.As(err); !ok && errors.Is(err, context.Canceled) {
		return
	}
	public := publicError(err, logging.F("method", r.Method), logging.F("path", loggedPath(r)))
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

func render(w http.ResponseWriter, r *http.Request, status int, components ...templ.Component) {
	html(w)
	w.WriteHeader(status)
	for _, component := range components {
		component.Render(r.Context(), w)
	}
}

// publicError is err if it is public; other errors are logged, with fields,
// and shown generically.
func publicError(err error, fields ...logging.Field) *apperr.Error {
	if public, ok := apperr.As(err); ok {
		return public
	}
	if !errors.Is(err, context.Canceled) {
		log.Error("Unhandled request error", append(fields, causeFields(err)...)...)
	}
	if docker.Unavailable(err) {
		return dockerUnavailable
	}
	return errInternal
}

// dockerProblem is why Docker can't be used when the engine answers but shares
// no API version with Ludock, or "" when it just can't be reached.
func (s *Server) dockerProblem() string {
	if err := s.App.Docker.Incompatible(); err != nil {
		return err.Error()
	}
	return ""
}

func publicMessage(err error) string { return publicError(err).Message }

var (
	dockerUnavailable = apperr.New("DOCKER_UNAVAILABLE", http.StatusServiceUnavailable, "Ludock can’t reach Docker right now. Try again once it can.")
	errInternal       = apperr.New("INTERNAL", 500, "Internal server error")
)

// causeFields describe an unexpected error without its message.
func causeFields(err error) []logging.Field { return logging.Cause(err, docker.StatusCode(err)) }

func itoa(value int64) string { return strconv.FormatInt(value, 10) }
