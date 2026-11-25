package advanced

import (
	"context"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/config"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/logging"
)

var ctx = context.Background()

// A saved proxy list that no longer parses is ignored at startup, so Ludock
// still starts and an administrator can fix it in the panel.
func TestApplyIgnoresInvalidSavedProxies(t *testing.T) {
	d := dbtest.Open(t)
	if err := d.SetSetting(ctx, nil, proxiesKey, proxiesSetting{Trusted: "proxy.example"}); err != nil {
		t.Fatal(err)
	}
	s := &Service{DB: d, ClientIP: &auth.ClientIP{}}
	s.Apply(ctx)
	if got := s.ClientIP.Resolve("203.0.113.9", "172.18.0.2"); got != "172.18.0.2" {
		t.Fatal(got)
	}
	if err := d.SetSetting(ctx, nil, proxiesKey, proxiesSetting{Trusted: "172.18.0.2"}); err != nil {
		t.Fatal(err)
	}
	s.Apply(ctx)
	if got := s.ClientIP.Resolve("203.0.113.9", "172.18.0.2"); got != "203.0.113.9" {
		t.Fatal(got)
	}
	// A one-click trust loses to a list saved since the hint was shown.
	if err := s.SetProxies(ctx, "admin", "token", "172.18.0.9", true, db.AuditEntry{}); err != ErrProxiesChanged {
		t.Fatal(err)
	}
	// The environment's list wins over the saved one.
	s.Env = config.Config{TrustedProxies: "10.0.0.1", TrustedProxiesSet: true}
	s.Apply(ctx)
	if got := s.ClientIP.Resolve("203.0.113.9", "172.18.0.2"); got != "172.18.0.2" {
		t.Fatal(got)
	}
	// An empty LUDOCK_TRUSTED_PROXIES trusts none, whatever was saved.
	s.Env = config.Config{TrustedProxiesSet: true}
	s.Apply(ctx)
	if got := s.ClientIP.Resolve("203.0.113.9", "172.18.0.2"); got != "172.18.0.2" || !s.Proxies(ctx).Fixed {
		t.Fatal(got)
	}
}

// Verbose logging turns itself off when its time is up, and
// LUDOCK_LOG_LEVEL wins over it.
func TestVerboseLoggingEnds(t *testing.T) {
	d := dbtest.Open(t)
	t.Cleanup(func() { logging.SetLevel(logging.LevelInfo) })
	// On is checked against a time far off, so a slow machine can't reach it
	// before the check; only then is it brought close.
	until := time.Now().Add(time.Hour).UnixMilli()
	if err := d.SetSetting(ctx, nil, loggingKey, loggingSetting{VerboseUntil: until}); err != nil {
		t.Fatal(err)
	}
	s := &Service{DB: d, ClientIP: &auth.ClientIP{}}
	s.Apply(ctx)
	if logging.CurrentLevel() != logging.LevelDebug || s.Logging(ctx).VerboseUntil != until {
		t.Fatal(logging.CurrentLevel())
	}
	if err := d.SetSetting(ctx, nil, loggingKey, loggingSetting{VerboseUntil: time.Now().Add(50 * time.Millisecond).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	s.Apply(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for logging.CurrentLevel() != logging.LevelInfo {
		if time.Now().After(deadline) {
			t.Fatal("verbose logging stayed on")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s.Logging(ctx).VerboseUntil != 0 {
		t.Fatal("still reported on")
	}

	if err := d.SetSetting(ctx, nil, loggingKey, loggingSetting{VerboseUntil: time.Now().Add(time.Hour).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	s.Env.LogLevel = "warn"
	s.Apply(ctx)
	if current := s.Logging(ctx); !current.Fixed || current.Level != "warn" || logging.CurrentLevel() != logging.LevelWarn {
		t.Fatal(current, logging.CurrentLevel())
	}
	if err := s.SetVerbose(ctx, "admin", "token", true, db.AuditEntry{}); err != errFixed {
		t.Fatal(err)
	}
}
