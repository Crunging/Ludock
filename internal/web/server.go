// Package web serves Ludock's pages, fragments, and streams.
package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/app"
	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/logging"
)

var log = logging.New("http")

// HealthCheck reports whether a dependency is usable.
type HealthCheck func(ctx context.Context) error

// Server holds the HTTP dependencies.
type Server struct {
	// App holds every service the pages use.
	App      *app.App
	DB       *db.DB
	Auth     *auth.Service
	ClientIP *auth.ClientIP
	// Docker checks the Docker connection for /api/v1/health.
	Docker HealthCheck

	mux      *http.ServeMux
	healthMu sync.Mutex
	healthAt time.Time
	healthy  bool
	sockets  sockets

	streamsMu sync.Mutex
	// streamsClosed closes when shutdown begins, ending every event stream.
	streamsClosed chan struct{}
	// openStreams counts event streams per user and per session.
	openStreams streamCounts
}

func (s *Server) streamsDone() <-chan struct{} {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	if s.streamsClosed == nil {
		s.streamsClosed = make(chan struct{})
	}
	return s.streamsClosed
}

// CloseStreams ends every event stream for shutdown. Streams otherwise stay
// open until their session ends, and http.Server.Shutdown waits for them.
func (s *Server) CloseStreams() {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	if s.streamsClosed == nil {
		s.streamsClosed = make(chan struct{})
	}
	select {
	case <-s.streamsClosed:
	default:
		close(s.streamsClosed)
	}
}

// Handler builds the routed handler.
func (s *Server) Handler() http.Handler {
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("GET /api/v1/health", s.health)
	s.mux.HandleFunc("GET /assets/", serveAsset)
	s.mux.HandleFunc("GET /login", s.loginPage)
	s.mux.HandleFunc("POST /login", s.login)
	s.mux.HandleFunc("POST /setup", s.setup)
	s.mux.HandleFunc("POST /logout", s.authed(s.logout))
	if s.App != nil {
		s.routes()
	}
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Not found", http.StatusNotFound)
	})
	return s.policy(compressPages(s.mux))
}

const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

func requestID() string {
	var raw [16]byte
	rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(data)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush lets streamed responses (SSE) pass through the recorder.
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// policy applies headers and cross-origin rules to every request.
func (s *Server) policy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		id := requestID()
		header := w.Header()
		header.Set("X-Request-ID", id)
		header.Set("X-Content-Type-Options", "nosniff")
		// same-origin, not no-referrer: Chrome sends "Origin: null" on form posts
		// under no-referrer, which the cross-origin check must reject.
		header.Set("Referrer-Policy", "same-origin")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Cross-Origin-Opener-Policy", "same-origin")
		header.Set("Cross-Origin-Resource-Policy", "same-origin")
		header.Set("Origin-Agent-Cluster", "?1")
		header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		header.Set("Content-Security-Policy", contentSecurityPolicy)
		if !strings.HasPrefix(r.URL.Path, "/assets/") || r.URL.Query().Get("v") == "" {
			// Pages and fragments carry account and server data.
			header.Set("Cache-Control", "no-store")
		}
		if auth.IsExternalHTTPS(r) {
			header.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		recorder := &statusRecorder{ResponseWriter: w}
		defer func() {
			if recovered := recover(); recovered != nil {
				if recovered == http.ErrAbortHandler {
					panic(recovered)
				}
				log.Error("Unhandled request error", logging.F("requestId", id), logging.F("method", r.Method),
					logging.F("path", r.URL.Path))
				if recorder.status == 0 {
					http.Error(recorder, "Internal server error", http.StatusInternalServerError)
				}
			}
			log.Debug("HTTP request completed", logging.F("requestId", id), logging.F("method", r.Method),
				logging.F("path", r.URL.Path), logging.F("status", recorder.status),
				logging.F("durationMs", time.Since(started).Milliseconds()), logging.F("remoteAddress", s.ClientIP.Request(r)))
		}()
		if !safeMethod(r.Method) && (r.Header.Get("Sec-Fetch-Site") == "cross-site" ||
			(r.Header.Get("Origin") != "" && !auth.IsSameOrigin(r))) {
			http.Error(recorder, "Cross-origin request rejected", http.StatusForbidden)
			return
		}
		next.ServeHTTP(recorder, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

// health caches its probe for five seconds so monitoring cannot hammer
// Docker.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	s.healthMu.Lock()
	if s.healthAt.IsZero() || time.Since(s.healthAt) >= 5*time.Second {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		healthy := s.DB.Check(ctx) == nil && (s.Docker == nil || s.Docker(ctx) == nil)
		cancel()
		s.healthAt, s.healthy = time.Now(), healthy
	}
	healthy := s.healthy
	s.healthMu.Unlock()
	if healthy {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "docker": "connected", "database": "connected"})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded"})
}
