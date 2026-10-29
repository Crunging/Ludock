// Package config reads and validates Ludock's environment once at startup.
// Every setting is a LUDOCK_* variable; other packages receive values, never
// read the environment themselves.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/datasize"
)

// Config is the process configuration.
type Config struct {
	// Listen is the HTTP address, host:port (LUDOCK_LISTEN, default ":3000").
	Listen string
	// DataDir holds the database and its identity key (LUDOCK_DATA_DIR,
	// default "data"; the image sets /data).
	DataDir string
	// LogLevel is error, warn, info, or debug (LUDOCK_LOG_LEVEL).
	LogLevel string
	// TrustedProxies may supply X-Forwarded-For (LUDOCK_TRUSTED_PROXIES,
	// comma-separated addresses or CIDRs).
	TrustedProxies string
	// SetupCode is the first-administrator code; generated when empty.
	SetupCode string
	// DockerSocket is the Engine socket inside Ludock (LUDOCK_DOCKER_SOCKET).
	DockerSocket string
	// SelfContainer identifies Ludock's own container (LUDOCK_SELF_CONTAINER,
	// default HOSTNAME).
	SelfContainer string
	// BackupRoots are approved backup destinations (LUDOCK_BACKUP_ROOTS,
	// ':'-separated, default /backups; set it empty to turn backups off).
	BackupRoots []string
	// DockerConfig is a read-only Docker client config folder for private
	// registries (LUDOCK_DOCKER_CONFIG).
	DockerConfig string
	// SensitivePaths are extra protected paths (LUDOCK_SENSITIVE_PATHS).
	SensitivePaths []string
	// HelperImage overrides the file and backup helper image
	// (LUDOCK_HELPER_IMAGE, pinned by digest).
	HelperImage string
	// MaxUploadBytes limits each uploaded file (LUDOCK_MAX_UPLOAD_SIZE,
	// default 2 GiB).
	MaxUploadBytes int64
	// FileHelperIdle is how long an idle file-listing helper is kept for the
	// next listing (LUDOCK_FILE_HELPER_IDLE, default 30s).
	FileHelperIdle time.Duration
	// UpdateSettle is how long an updated server without a healthcheck must
	// keep running before it replaces the original (LUDOCK_UPDATE_SETTLE,
	// default 10s).
	UpdateSettle time.Duration
	// SaveSettle is how long Ludock waits after a game's save command before
	// it stops the server (LUDOCK_SAVE_SETTLE, default 5s).
	SaveSettle time.Duration
}

// Defaults.
const (
	DefaultListen         = ":3000"
	DefaultDockerSocket   = "/var/run/docker.sock"
	DefaultBackupRoot     = "/backups"
	DefaultMaxUploadSize  = 2 << 30
	DefaultFileHelperIdle = 30 * time.Second
	DefaultUpdateSettle   = 10 * time.Second
	DefaultSaveSettle     = 5 * time.Second
)

// DBPath is the database file inside the data directory.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "ludock.db") }

// HealthURL is the local health endpoint, for the image healthcheck.
func (c Config) HealthURL() string {
	host, port, _ := net.SplitHostPort(c.Listen)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/api/v1/health"
}

func value(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// pathList splits a ':'-separated list of absolute paths.
func pathList(name, raw string) ([]string, error) {
	var paths []string
	for _, part := range strings.Split(raw, ":") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.HasPrefix(part, "/") {
			// The value is a host path; name the variable only.
			return nil, fmt.Errorf("%s must list absolute paths separated by ':'", name)
		}
		if cleaned := path.Clean(part); !contains(paths, cleaned) {
			paths = append(paths, cleaned)
		}
	}
	return paths, nil
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

var levels = []string{"error", "warn", "info", "debug"}

// Load reads and validates the environment. Errors name the variable, never
// its value, since some values are secrets.
func Load() (Config, error) {
	c := Config{
		Listen:         value("LUDOCK_LISTEN", DefaultListen),
		DataDir:        value("LUDOCK_DATA_DIR", "data"),
		LogLevel:       strings.ToLower(value("LUDOCK_LOG_LEVEL", "info")),
		TrustedProxies: value("LUDOCK_TRUSTED_PROXIES", ""),
		SetupCode:      value("LUDOCK_SETUP_CODE", ""),
		DockerSocket:   value("LUDOCK_DOCKER_SOCKET", DefaultDockerSocket),
		SelfContainer:  value("LUDOCK_SELF_CONTAINER", value("HOSTNAME", "")),
		DockerConfig:   value("LUDOCK_DOCKER_CONFIG", ""),
		HelperImage:    value("LUDOCK_HELPER_IMAGE", ""),
		MaxUploadBytes: DefaultMaxUploadSize,
		FileHelperIdle: DefaultFileHelperIdle,
		UpdateSettle:   DefaultUpdateSettle,
		SaveSettle:     DefaultSaveSettle,
	}
	var problems []error
	if host, port, err := net.SplitHostPort(c.Listen); err != nil || port == "" {
		problems = append(problems, errors.New("LUDOCK_LISTEN must be host:port, for example :3000"))
	} else if number, err := strconv.Atoi(port); err != nil || number < 0 || number > 65535 || strings.ContainsAny(host, " /") {
		problems = append(problems, errors.New("LUDOCK_LISTEN must be host:port, for example :3000"))
	}
	if !contains(levels, c.LogLevel) {
		problems = append(problems, errors.New("LUDOCK_LOG_LEVEL must be error, warn, info, or debug"))
	}
	if !strings.HasPrefix(c.DockerSocket, "/") {
		problems = append(problems, errors.New("LUDOCK_DOCKER_SOCKET must be an absolute path"))
	}
	backups, set := os.LookupEnv("LUDOCK_BACKUP_ROOTS")
	if !set {
		backups = DefaultBackupRoot
	}
	var err error
	if c.BackupRoots, err = pathList("LUDOCK_BACKUP_ROOTS", backups); err != nil {
		problems = append(problems, err)
	}
	if c.SensitivePaths, err = pathList("LUDOCK_SENSITIVE_PATHS", os.Getenv("LUDOCK_SENSITIVE_PATHS")); err != nil {
		problems = append(problems, err)
	}
	if c.DockerConfig != "" && !strings.HasPrefix(c.DockerConfig, "/") {
		problems = append(problems, errors.New("LUDOCK_DOCKER_CONFIG must be an absolute path"))
	}
	if raw := value("LUDOCK_MAX_UPLOAD_SIZE", ""); raw != "" {
		if size, ok := datasize.Parse(raw); !ok || size <= 0 {
			problems = append(problems, errors.New("LUDOCK_MAX_UPLOAD_SIZE must be a positive size such as 500 MB, 2 GiB, or 1.5 GiB"))
		} else {
			c.MaxUploadBytes = size
		}
	}
	for _, setting := range []struct {
		name string
		into *time.Duration
	}{{"LUDOCK_FILE_HELPER_IDLE", &c.FileHelperIdle}, {"LUDOCK_UPDATE_SETTLE", &c.UpdateSettle},
		{"LUDOCK_SAVE_SETTLE", &c.SaveSettle}} {
		if raw := value(setting.name, ""); raw != "" {
			if duration, err := time.ParseDuration(raw); err != nil || duration <= 0 {
				problems = append(problems, fmt.Errorf("%s must be a positive duration such as 30s or 2m", setting.name))
			} else {
				*setting.into = duration
			}
		}
	}
	return c, errors.Join(problems...)
}
