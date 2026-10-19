package auth

import (
	"errors"
	"strings"
	"testing"
)

func resolver(t *testing.T, configured string) *ClientIP {
	t.Helper()
	c, err := NewClientIP(configured)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientIPStopsAtFirstUntrustedHop(t *testing.T) {
	if got := resolver(t, "").Resolve("198.51.100.1", "203.0.113.1"); got != "203.0.113.1" {
		t.Fatal(got)
	}
	r := resolver(t, "172.18.0.0/24,2001:db8:1234::/48")
	for _, c := range []struct{ header, peer, want string }{
		{"198.51.100.1", "203.0.113.1", "203.0.113.1"},
		{"spoofed, 203.0.113.1, 2001:db8:1234::2", "172.18.0.2", "203.0.113.1"},
		{"198.51.100.1", "172.18.1.2", "172.18.1.2"},
		{"198.51.100.1", "2001:db8:1235::2", "2001:db8:1235::2"},
		{"198.51.100.1", "", ""},
	} {
		if got := r.Resolve(c.header, c.peer); got != c.want {
			t.Errorf("Resolve(%q, %q) = %q, want %q", c.header, c.peer, got, c.want)
		}
	}
}

func TestClientIPNormalizesSpellings(t *testing.T) {
	r := resolver(t, "172.18.0.2,::ffff:172.18.0.3/128")
	if got := r.Resolve("2001:0DB8:0000::1", "::ffff:ac12:2"); got != "2001:db8::1" {
		t.Fatal(got)
	}
	if got := r.Resolve("::ffff:203.0.113.1", "172.18.0.3"); got != "203.0.113.1" {
		t.Fatal(got)
	}
	if got := resolver(t, "").Resolve("", "::ffff:cb00:7101"); got != "203.0.113.1" {
		t.Fatal(got)
	}
}

func TestClientIPMalformedTrustedHopFallsBack(t *testing.T) {
	r := resolver(t, "172.18.0.2")
	many := strings.TrimSuffix(strings.Repeat("172.18.0.2,", 33), ",")
	for _, header := range []string{"unknown", "127.1", "203.0.113.1,", many} {
		if got := r.Resolve(header, "172.18.0.2"); got != "172.18.0.2" {
			t.Errorf("%q -> %q", header, got)
		}
	}
}

func TestClientIPRejectsInvalidTrust(t *testing.T) {
	for _, configured := range []string{"*", "172.18.0.2/33", "::1/129", "10.0.0.1/8/1", "010.0.0.1"} {
		if _, err := NewClientIP(configured); !errors.Is(err, ErrTrustedProxies) {
			t.Errorf("%q: %v", configured, err)
		}
	}
}
