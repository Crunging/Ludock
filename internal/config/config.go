// Package config reads Ludock's environment variables. The names match the
// previous releases so existing .env files keep working.
package config

import (
	"os"
	"strconv"
	"strings"
)

// Config is the process configuration.
type Config struct {
	Port           int
	Host           string
	DBPath         string
	LogLevel       string
	TrustedProxies string
	SetupCode      string
	DockerSocket   string
	// SelfContainer identifies Ludock's own container (defaults to HOSTNAME).
	SelfContainer string
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// Load reads the environment.
func Load() Config {
	port, err := strconv.Atoi(env("PORT", "3000"))
	if err != nil || port < 0 || port > 65535 {
		port = 3000
	}
	return Config{
		Port:           port,
		Host:           os.Getenv("HOST"),
		DBPath:         env("LUDOCK_DB_PATH", "data/ludock.db"),
		LogLevel:       os.Getenv("LOG_LEVEL"),
		TrustedProxies: os.Getenv("LUDOCK_TRUSTED_PROXIES"),
		SetupCode:      os.Getenv("LUDOCK_SETUP_CODE"),
		DockerSocket:   env("DOCKER_SOCKET", "/var/run/docker.sock"),
		SelfContainer:  env("LUDOCK_SELF_CONTAINER", os.Getenv("HOSTNAME")),
	}
}
