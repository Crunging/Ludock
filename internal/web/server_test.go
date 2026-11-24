package web

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/logging"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/db/dbtest"
)

func TestPolicyHeadersAndCrossOriginRejection(t *testing.T) {
	t.Parallel()
	clientIP, _ := auth.NewClientIP("")
	dockerUp := true
	s := &Server{DB: dbtest.Open(t), ClientIP: clientIP, Docker: func(context.Context) error {
		if dockerUp {
			return nil
		}
		return errors.New("down")
	}}
	handler := s.Handler()
	request := httptest.NewRequest("GET", "http://panel.example/api/v1/health", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("X-Frame-Options") != "DENY" || response.Header().Get("Content-Security-Policy") == "" {
		t.Fatal(response.Code, response.Header())
	}
	for _, headers := range []map[string]string{
		{"Origin": "https://attacker.example"},
		{"Sec-Fetch-Site": "cross-site"},
	} {
		request := httptest.NewRequest("POST", "http://panel.example/anything", nil)
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Errorf("%v: %d", headers, response.Code)
		}
	}
	// The health probe is cached for five seconds.
	dockerUp = false
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "http://panel.example/api/v1/health", nil))
	if response.Code != 200 {
		t.Fatal("cache bypassed")
	}
	s.healthAt = s.healthAt.Add(-6e9)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "http://panel.example/api/v1/health", nil))
	if response.Code != http.StatusServiceUnavailable || response.Body.String() != "{\"status\":\"degraded\"}\n" {
		t.Fatal(response.Code, response.Body.String())
	}
}

// An unexpected error is logged by its types and Docker status, never its
// message, which can name a host path.
func TestCauseFieldsOmitMessages(t *testing.T) {
	err := fmt.Errorf("open /srv/secret/world: %w", &docker.APIError{Status: 500})
	encoded := string(logging.EncodeFields(causeFields(err)))
	if strings.Contains(encoded, "/srv/secret") || !strings.Contains(encoded, "*docker.APIError") || !strings.Contains(encoded, `"dockerStatus":500`) {
		t.Fatal(encoded)
	}
}

// A visitor who declares a small body and then stalls gets an answer once the
// read deadline passes, both where a handler answers without reading the body
// and where reading the form times out. Go reads such a leftover body before
// it answers, so without a deadline the connection stays open for good.
func TestStalledBodiesAreAnswered(t *testing.T) {
	bodyDeadline, formDeadline = 200*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { bodyDeadline, formDeadline = 60*time.Second, 30*time.Second })
	clientIP, _ := auth.NewClientIP("")
	server := httptest.NewServer((&Server{DB: dbtest.Open(t), ClientIP: clientIP}).Handler())
	t.Cleanup(server.Close)
	for _, path := range []string{"/logout", "/login"} {
		conn, err := net.Dial("tcp", server.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: panel\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 200000\r\n\r\nu", path)
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := bufio.NewReader(conn).ReadString('\n')
		conn.Close()
		if err != nil {
			t.Errorf("%s: no answer: %v", path, err)
		} else if !strings.HasPrefix(line, "HTTP/1.1 4") {
			t.Errorf("%s: %q", path, line)
		}
	}
}
