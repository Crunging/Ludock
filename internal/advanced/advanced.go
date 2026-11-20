// Package advanced keeps the settings under Settings → Advanced: the upload
// size limit, verbose logging, and trusted reverse proxies. Each can also be
// fixed by an environment variable, which then wins over the saved value.
package advanced

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/config"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/logging"
)

var log = logging.New("settings")

const (
	uploadKey  = "advanced.upload-limit"
	loggingKey = "advanced.verbose-logging"
	proxiesKey = "advanced.trusted-proxies"

	// VerboseFor is how long verbose logging stays on, long enough to catch
	// a problem that comes back with a nightly schedule.
	VerboseFor = 24 * time.Hour
)

type uploadSetting struct {
	Bytes int64 `json:"bytes"`
}

type loggingSetting struct {
	VerboseUntil int64 `json:"verboseUntil"`
}

type proxiesSetting struct {
	Trusted string `json:"trusted"`
}

type Service struct {
	DB       *db.DB
	ClientIP *auth.ClientIP
	// Env holds the values the environment sets; an empty one is unset.
	Env config.Config

	mu    sync.Mutex
	timer *time.Timer
	// saving keeps the trusted proxies in effect in the order they were saved.
	saving sync.Mutex
}

var errFixed = apperr.New("SETTING_FIXED", 409, "Ludock’s deployment sets this with an environment variable, which wins over the panel.")

// ErrInvalidProxies is the panel's message for a value LUDOCK_TRUSTED_PROXIES
// would refuse.
var ErrInvalidProxies = apperr.New("INVALID_TRUSTED_PROXIES", 400,
	"Enter IP addresses or ranges separated by commas, such as 172.18.0.2 or 172.18.0.0/16. Host names and ports aren’t accepted.")

// Apply puts the saved logging and proxy settings into effect at startup. A
// saved value that no longer parses is ignored rather than stopping Ludock,
// so it can still be fixed in the panel.
func (s *Service) Apply(ctx context.Context) {
	s.applyLogging(ctx)
	proxies := s.Proxies(ctx)
	prefixes, err := auth.ParseTrustedProxies(proxies.Trusted)
	if err != nil {
		log.Warn("Ignoring the saved trusted proxies, which are invalid; fix them in Settings → Advanced")
		prefixes = nil
	}
	s.ClientIP.Trust(prefixes)
}

// UploadLimit is the largest file one upload may send; 0 means no limit
// beyond the free space on the server's disk.
func (s *Service) UploadLimit(ctx context.Context) (bytes int64, fixed bool) {
	if s.Env.MaxUploadBytes > 0 {
		return s.Env.MaxUploadBytes, true
	}
	var saved uploadSetting
	if _, err := s.DB.Setting(ctx, uploadKey, &saved); err != nil {
		log.Warn("Could not read the upload size limit; uploads are limited by free space only")
	}
	return max(saved.Bytes, 0), false
}

// SetUploadLimit saves a limit in bytes, or 0 for none.
func (s *Service) SetUploadLimit(ctx context.Context, adminID, tokenHash string, bytes int64, audit db.AuditEntry) error {
	if _, fixed := s.UploadLimit(ctx); fixed {
		return errFixed
	}
	if bytes < 0 {
		return apperr.New("INVALID_REQUEST", 400, "Invalid request")
	}
	audit.Action, audit.TargetType, audit.Details = "settings.upload_limit.updated", "settings", map[string]any{"bytes": bytes}
	return s.DB.AdminMutation(ctx, adminID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		if err := s.DB.SetSetting(ctx, tx, uploadKey, uploadSetting{Bytes: bytes}); err != nil {
			return err
		}
		return s.DB.WriteAudit(ctx, tx, audit)
	})
}

type Logging struct {
	// Level is the environment's level when Fixed.
	Level string
	Fixed bool
	// VerboseUntil is when panel-enabled verbose logging ends, in Unix ms; 0
	// when it is off.
	VerboseUntil int64
}

func (s *Service) Logging(ctx context.Context) Logging {
	if s.Env.LogLevel != "" {
		return Logging{Level: s.Env.LogLevel, Fixed: true}
	}
	var saved loggingSetting
	if _, err := s.DB.Setting(ctx, loggingKey, &saved); err != nil {
		log.Warn("Could not read the logging setting")
	}
	if saved.VerboseUntil <= time.Now().UnixMilli() {
		return Logging{Level: logging.LevelInfo.String()}
	}
	return Logging{Level: logging.LevelDebug.String(), VerboseUntil: saved.VerboseUntil}
}

// SetVerbose turns verbose logging on for VerboseFor, or off.
func (s *Service) SetVerbose(ctx context.Context, adminID, tokenHash string, on bool, audit db.AuditEntry) error {
	if s.Env.LogLevel != "" {
		return errFixed
	}
	var until int64
	if on {
		until = time.Now().Add(VerboseFor).UnixMilli()
	}
	audit.Action, audit.TargetType, audit.Details = "settings.verbose_logging.updated", "settings", map[string]any{"enabled": on}
	err := s.DB.AdminMutation(ctx, adminID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		if err := s.DB.SetSetting(ctx, tx, loggingKey, loggingSetting{VerboseUntil: until}); err != nil {
			return err
		}
		return s.DB.WriteAudit(ctx, tx, audit)
	})
	if err == nil {
		s.applyLogging(context.WithoutCancel(ctx))
	}
	return err
}

// applyLogging sets the log level and, while verbose logging is on, a timer
// that turns it off.
func (s *Service) applyLogging(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	current := s.Logging(ctx)
	level, _ := logging.ParseLevel(current.Level)
	logging.SetLevel(level)
	if current.VerboseUntil > 0 {
		s.timer = time.AfterFunc(time.Until(time.UnixMilli(current.VerboseUntil)), func() {
			s.applyLogging(context.Background())
		})
	}
}

type Proxies struct {
	Trusted string
	Fixed   bool
}

func (s *Service) Proxies(ctx context.Context) Proxies {
	if s.Env.TrustedProxiesSet {
		return Proxies{Trusted: s.Env.TrustedProxies, Fixed: true}
	}
	var saved proxiesSetting
	if _, err := s.DB.Setting(ctx, proxiesKey, &saved); err != nil {
		log.Warn("Could not read the trusted proxies; no proxy is trusted")
	}
	return Proxies{Trusted: saved.Trusted}
}

// ErrProxiesChanged refuses a one-click trust once another save trusted a
// proxy.
var ErrProxiesChanged = apperr.New("PROXY_CHANGED", 409, "The proxy setting changed. Review it and save again.")

// SetProxies validates and saves trusted proxies exactly as
// LUDOCK_TRUSTED_PROXIES is read, then trusts them. onlyIfNone saves only
// while no proxy is trusted.
func (s *Service) SetProxies(ctx context.Context, adminID, tokenHash, value string, onlyIfNone bool, audit db.AuditEntry) error {
	if s.Env.TrustedProxiesSet {
		return errFixed
	}
	prefixes, err := auth.ParseTrustedProxies(value)
	if err != nil {
		return ErrInvalidProxies
	}
	var entries []string
	if strings.TrimSpace(value) != "" {
		for _, entry := range strings.Split(value, ",") {
			entries = append(entries, strings.TrimSpace(entry))
		}
	}
	trusted := strings.Join(entries, ", ")
	audit.Action, audit.TargetType, audit.Details = "settings.trusted_proxies.updated", "settings", map[string]any{"trusted": trusted}
	s.saving.Lock()
	defer s.saving.Unlock()
	if onlyIfNone && s.Proxies(ctx).Trusted != "" {
		return ErrProxiesChanged
	}
	err = s.DB.AdminMutation(ctx, adminID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		if err := s.DB.SetSetting(ctx, tx, proxiesKey, proxiesSetting{Trusted: trusted}); err != nil {
			return err
		}
		return s.DB.WriteAudit(ctx, tx, audit)
	})
	if err != nil {
		return err
	}
	s.ClientIP.Trust(prefixes)
	return nil
}
