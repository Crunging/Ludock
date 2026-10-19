package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/db/dbtest"
)

func TestPolicyHeadersAndCrossOriginRejection(t *testing.T) {
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
