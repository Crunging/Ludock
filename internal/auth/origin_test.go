package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func originRequest(headers map[string]string) *httptestRequest {
	r := httptest.NewRequest("POST", "http://panel.example/", nil)
	r.Host = "panel.example"
	for name, value := range headers {
		if name == "host" {
			r.Host = value
			continue
		}
		r.Header.Set(name, value)
	}
	return r
}

type httptestRequest = http.Request

func TestForwardedPublicOrigin(t *testing.T) {
	r := originRequest(map[string]string{"origin": "https://panel.example", "x-forwarded-proto": "https",
		"x-forwarded-host": "panel.example", "host": "ludock:3000"})
	if !IsExternalHTTPS(r) || !IsSameOrigin(r) {
		t.Fatal("forwarded origin rejected")
	}
}

func TestFetchMetadataWins(t *testing.T) {
	if !IsSameOrigin(originRequest(map[string]string{"origin": "https://panel.example", "host": "ludock:3000",
		"x-forwarded-host": "gateway.internal", "x-forwarded-proto": "http", "sec-fetch-site": "same-origin"})) {
		t.Fatal("same-origin metadata rejected")
	}
	if IsSameOrigin(originRequest(map[string]string{"origin": "https://attacker.example", "host": "panel.example",
		"x-forwarded-proto": "https", "sec-fetch-site": "cross-site"})) {
		t.Fatal("cross-site accepted")
	}
}

func TestRejectsCrossHostAndMalformedOrigins(t *testing.T) {
	for _, origin := range []string{"https://attacker.example", "ftp://panel.example", "https://user@panel.example",
		"https://panel.example/path", "null", "https://panel.example?x", "https://panel.example#x"} {
		if IsSameOrigin(originRequest(map[string]string{"origin": origin, "host": "panel.example"})) {
			t.Errorf("accepted %q", origin)
		}
	}
	if IsSameOrigin(originRequest(map[string]string{"origin": "https://attacker.example", "host": "ludock:3000",
		"x-forwarded-host": "panel.example", "x-forwarded-proto": "https"})) {
		t.Fatal("accepted attacker through forwarded host")
	}
}

func TestOriginPortNormalization(t *testing.T) {
	for _, c := range []struct {
		origin, host string
		forwarded    map[string]string
		want         bool
	}{
		{"http://panel.example", "panel.example:80", nil, true},
		{"http://panel.example:8080", "PANEL.example:8080", nil, true},
		{"http://panel.example:8080", "panel.example", nil, false},
		{"https://panel.example:8443", "ludock:3000", map[string]string{"x-forwarded-proto": "https",
			"x-forwarded-host": "panel.example", "x-forwarded-port": "8443"}, true},
		{"http://[::1]:3000", "[::1]:3000", nil, true},
	} {
		headers := map[string]string{"origin": c.origin, "host": c.host}
		for k, v := range c.forwarded {
			headers[k] = v
		}
		if got := IsSameOrigin(originRequest(headers)); got != c.want {
			t.Errorf("%s vs %s: %v", c.origin, c.host, got)
		}
	}
}
