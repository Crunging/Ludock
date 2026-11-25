// Package config reads and validates Ludock's environment once at startup.
// Every setting is a LUDOCK_* variable; other packages receive values, never
// read the environment themselves. Settings that Settings → Advanced also
// offers are empty here unless the environment sets them, and then win.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/datasize"
	"github.com/Crunging/Ludock/internal/mountinfo"
)

type Config struct {
	// Listen is the HTTP address, host:port (LUDOCK_LISTEN, default ":3000").
	Listen string
	// DataDir holds the database and its identity key (LUDOCK_DATA_DIR,
	// default "data"; the image sets /data).
	DataDir string
	// LogLevel is error, warn, info, or debug (LUDOCK_LOG_LEVEL).
	LogLevel string
	// TrustedProxies may supply X-Forwarded-For (LUDOCK_TRUSTED_PROXIES,
	// comma-separated addresses or CIDRs). TrustedProxiesSet is true when the
	// variable is present, even empty, which trusts none.
	TrustedProxies    string
	TrustedProxiesSet bool
	// SetupCode is the first-administrator code; generated when empty.
	SetupCode string
	// DockerSocket is the Engine socket inside Ludock (LUDOCK_DOCKER_SOCKET).
	DockerSocket string
	// SelfContainer identifies Ludock's own container (LUDOCK_SELF_CONTAINER,
	// else detected from its mounts, else HOSTNAME).
	SelfContainer string
	// SelfFound is how: "env", "mounts" (its full ID), or "hostname".
	SelfFound string
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
	// MaxUploadBytes limits each uploaded file (LUDOCK_MAX_UPLOAD_SIZE).
	MaxUploadBytes int64
	// DevCopyRoot is a folder treated as mounted at /backups-copy, for
	// development against cmd/fakedocker (LUDOCK_DEV_COPY_ROOT).
	DevCopyRoot string
}

const (
	DefaultListen       = ":3000"
	DefaultDockerSocket = "/var/run/docker.sock"
	DefaultBackupRoot   = "/backups"
)

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
		if cleaned := path.Clean(part); !slices.Contains(paths, cleaned) {
			paths = append(paths, cleaned)
		}
	}
	return paths, nil
}

// hostnameMount finds the file Docker and Podman mount at /etc/hostname, which
// sits in a folder named for the container's full ID.
var hostnameMount = regexp.MustCompile(`/([0-9a-f]{64})(?:/userdata)?/hostname$`)

// containerID reads the container's own ID from its mounts, which stays
// right when a custom hostname or host networking changes HOSTNAME.
func containerID(mounts []mountinfo.Mount) string {
	for _, mount := range mounts {
		if mount.Point == "/etc/hostname" {
			if match := hostnameMount.FindStringSubmatch(mount.Root); match != nil {
				return match[1]
			}
		}
	}
	return ""
}

var levels = []string{"error", "warn", "info", "debug"}

// Load reads and validates the environment. Errors name the variable, never
// its value, since some values are secrets.
func Load() (Config, error) {
	c := Config{
		Listen:         value("LUDOCK_LISTEN", DefaultListen),
		DataDir:        value("LUDOCK_DATA_DIR", "data"),
		LogLevel:       strings.ToLower(value("LUDOCK_LOG_LEVEL", "")),
		TrustedProxies: value("LUDOCK_TRUSTED_PROXIES", ""),
		SetupCode:      value("LUDOCK_SETUP_CODE", ""),
		DockerSocket:   value("LUDOCK_DOCKER_SOCKET", DefaultDockerSocket),
		SelfContainer:  value("LUDOCK_SELF_CONTAINER", ""),
		DockerConfig:   value("LUDOCK_DOCKER_CONFIG", ""),
		HelperImage:    value("LUDOCK_HELPER_IMAGE", ""),
	}
	c.SelfFound = "env"
	if c.SelfContainer == "" {
		c.SelfContainer, c.SelfFound = value("HOSTNAME", ""), "hostname"
		if mounts, err := mountinfo.Read(); err == nil {
			if id := containerID(mounts); id != "" {
				c.SelfContainer, c.SelfFound = id, "mounts"
			}
		}
	}
	var problems []error
	if host, port, err := net.SplitHostPort(c.Listen); err != nil || port == "" {
		problems = append(problems, errors.New("LUDOCK_LISTEN must be host:port, for example :3000"))
	} else if number, err := strconv.Atoi(port); err != nil || number < 0 || number > 65535 || strings.ContainsAny(host, " /") {
		problems = append(problems, errors.New("LUDOCK_LISTEN must be host:port, for example :3000"))
	}
	if c.LogLevel != "" && !slices.Contains(levels, c.LogLevel) {
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
	if dev, err := pathList("LUDOCK_DEV_COPY_ROOT", os.Getenv("LUDOCK_DEV_COPY_ROOT")); err != nil || len(dev) > 1 {
		problems = append(problems, errors.New("LUDOCK_DEV_COPY_ROOT must be one absolute path"))
	} else if len(dev) == 1 {
		c.DevCopyRoot = dev[0]
	}
	if c.DockerConfig != "" && !strings.HasPrefix(c.DockerConfig, "/") {
		problems = append(problems, errors.New("LUDOCK_DOCKER_CONFIG must be an absolute path"))
	}
	_, c.TrustedProxiesSet = os.LookupEnv("LUDOCK_TRUSTED_PROXIES")
	if _, err := auth.ParseTrustedProxies(c.TrustedProxies); err != nil {
		problems = append(problems, err)
	}
	if raw := value("LUDOCK_MAX_UPLOAD_SIZE", ""); raw != "" {
		if size, ok := datasize.Parse(raw); !ok || size <= 0 {
			problems = append(problems, errors.New("LUDOCK_MAX_UPLOAD_SIZE must be a positive size such as 500 MB, 2 GiB, or 1.5 GiB"))
		} else {
			c.MaxUploadBytes = size
		}
	}
	return c, errors.Join(problems...)
}
