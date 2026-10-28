package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	for _, name := range []string{"LUDOCK_LISTEN", "LUDOCK_DATA_DIR", "LUDOCK_LOG_LEVEL", "LUDOCK_TRUSTED_PROXIES", "LUDOCK_SETUP_CODE",
		"LUDOCK_DOCKER_SOCKET", "LUDOCK_SELF_CONTAINER", "LUDOCK_DOCKER_CONFIG", "LUDOCK_SENSITIVE_PATHS",
		"LUDOCK_HELPER_IMAGE", "LUDOCK_MAX_UPLOAD_SIZE", "LUDOCK_FILE_HELPER_IDLE", "LUDOCK_UPDATE_SETTLE", "LUDOCK_SAVE_SETTLE"} {
		t.Setenv(name, "")
	}
	t.Setenv("HOSTNAME", "ludock-1")
}

func TestDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":3000" || cfg.DBPath() != "data/ludock.db" || cfg.LogLevel != "info" || cfg.DockerSocket != "/var/run/docker.sock" ||
		cfg.SelfContainer != "ludock-1" || cfg.MaxUploadBytes != 2<<30 || cfg.FileHelperIdle != 30*time.Second || cfg.UpdateSettle != 10*time.Second || cfg.SaveSettle != 5*time.Second || cfg.HealthURL() != "http://127.0.0.1:3000/api/v1/health" {
		t.Fatalf("%+v", cfg)
	}
}

func TestBackupRootsDefaultAndOff(t *testing.T) {
	clearEnv(t)
	t.Setenv("LUDOCK_BACKUP_ROOTS", "")
	cfg, _ := Load()
	if len(cfg.BackupRoots) != 0 {
		t.Fatal("empty value keeps backups off", cfg.BackupRoots)
	}
	t.Setenv("LUDOCK_BACKUP_ROOTS", "/backups:/mnt/disk2/:/backups")
	cfg, _ = Load()
	if !slices.Equal(cfg.BackupRoots, []string{"/backups", "/mnt/disk2"}) {
		t.Fatal(cfg.BackupRoots)
	}
}

func TestValuesAndErrors(t *testing.T) {
	clearEnv(t)
	t.Setenv("LUDOCK_LISTEN", "127.0.0.1:8080")
	t.Setenv("LUDOCK_LOG_LEVEL", "DEBUG")
	t.Setenv("LUDOCK_MAX_UPLOAD_SIZE", "500 MB")
	t.Setenv("LUDOCK_FILE_HELPER_IDLE", "2s")
	t.Setenv("LUDOCK_UPDATE_SETTLE", "1m")
	t.Setenv("LUDOCK_SAVE_SETTLE", "500ms")
	t.Setenv("LUDOCK_SENSITIVE_PATHS", "/srv/secrets")
	cfg, err := Load()
	if err != nil || cfg.LogLevel != "debug" || cfg.MaxUploadBytes != 500_000_000 || cfg.FileHelperIdle != 2*time.Second || cfg.UpdateSettle != time.Minute || cfg.SaveSettle != 500*time.Millisecond || cfg.HealthURL() != "http://127.0.0.1:8080/api/v1/health" ||
		!slices.Equal(cfg.SensitivePaths, []string{"/srv/secrets"}) {
		t.Fatal(cfg, err)
	}
	t.Setenv("LUDOCK_LISTEN", "3000")
	t.Setenv("LUDOCK_LOG_LEVEL", "verbose")
	t.Setenv("LUDOCK_MAX_UPLOAD_SIZE", "lots")
	t.Setenv("LUDOCK_FILE_HELPER_IDLE", "0s")
	t.Setenv("LUDOCK_UPDATE_SETTLE", "soon")
	t.Setenv("LUDOCK_SAVE_SETTLE", "-1s")
	t.Setenv("LUDOCK_SENSITIVE_PATHS", "relative/secrets")
	t.Setenv("LUDOCK_SETUP_CODE", "secret-value-never-printed")
	_, err = Load()
	for _, name := range []string{"LUDOCK_LISTEN", "LUDOCK_LOG_LEVEL", "LUDOCK_MAX_UPLOAD_SIZE", "LUDOCK_FILE_HELPER_IDLE", "LUDOCK_UPDATE_SETTLE", "LUDOCK_SAVE_SETTLE", "LUDOCK_SENSITIVE_PATHS"} {
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s not reported: %v", name, err)
		}
	}
	if err != nil && (strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "relative/stacks")) {
		t.Fatal("error leaks a value")
	}
}
