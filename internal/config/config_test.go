package config

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func clearEnv(t *testing.T) {
	for _, name := range []string{"LUDOCK_LISTEN", "LUDOCK_DATA_DIR", "LUDOCK_LOG_LEVEL", "LUDOCK_TRUSTED_PROXIES", "LUDOCK_SETUP_CODE",
		"LUDOCK_DOCKER_SOCKET", "LUDOCK_SELF_CONTAINER", "LUDOCK_DOCKER_CONFIG", "LUDOCK_SENSITIVE_PATHS",
		"LUDOCK_HELPER_IMAGE", "LUDOCK_MAX_UPLOAD_SIZE"} {
		t.Setenv(name, "")
	}
	t.Setenv("HOSTNAME", "ludock-1")
	mountinfoPath = "/nonexistent"
	os.Unsetenv("LUDOCK_TRUSTED_PROXIES")
}

func TestDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TrustedProxiesSet {
		t.Fatal("unset trusted proxies are set")
	}
	t.Setenv("LUDOCK_TRUSTED_PROXIES", "")
	if empty, _ := Load(); !empty.TrustedProxiesSet {
		t.Fatal("an empty value must fix the list to none")
	}
	// Settings the panel also offers stay empty, so the panel decides.
	if cfg.Listen != ":3000" || cfg.DBPath() != "data/ludock.db" || cfg.LogLevel != "" || cfg.DockerSocket != "/var/run/docker.sock" ||
		cfg.SelfContainer != "ludock-1" || cfg.MaxUploadBytes != 0 || cfg.TrustedProxies != "" || cfg.HealthURL() != "http://127.0.0.1:3000/api/v1/health" {
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
	t.Setenv("LUDOCK_SENSITIVE_PATHS", "/srv/secrets")
	cfg, err := Load()
	if err != nil || cfg.LogLevel != "debug" || cfg.MaxUploadBytes != 500_000_000 || cfg.HealthURL() != "http://127.0.0.1:8080/api/v1/health" ||
		!slices.Equal(cfg.SensitivePaths, []string{"/srv/secrets"}) {
		t.Fatal(cfg, err)
	}
	t.Setenv("LUDOCK_LISTEN", "3000")
	t.Setenv("LUDOCK_LOG_LEVEL", "verbose")
	t.Setenv("LUDOCK_MAX_UPLOAD_SIZE", "lots")
	t.Setenv("LUDOCK_SENSITIVE_PATHS", "relative/secrets")
	t.Setenv("LUDOCK_SETUP_CODE", "secret-value-never-printed")
	t.Setenv("LUDOCK_TRUSTED_PROXIES", "proxy.example")
	_, err = Load()
	for _, name := range []string{"LUDOCK_LISTEN", "LUDOCK_LOG_LEVEL", "LUDOCK_MAX_UPLOAD_SIZE", "LUDOCK_SENSITIVE_PATHS", "LUDOCK_TRUSTED_PROXIES"} {
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s not reported: %v", name, err)
		}
	}
	if err != nil && (strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "relative/stacks")) {
		t.Fatal("error leaks a value")
	}
}

// Ludock finds its own container ID in the file Docker or Podman mounts at
// /etc/hostname, which a custom hostname or host networking doesn't change.
func TestContainerIDFromMountinfo(t *testing.T) {
	id := strings.Repeat("0123456789abcdef", 4)
	for _, c := range []struct{ mountinfo, want string }{
		{"612 590 254:1 /var/lib/docker/containers/" + id + "/hostname /etc/hostname rw,relatime - ext4 /dev/vda1 rw", id},
		{"612 590 0:45 /containers/storage/overlay-containers/" + id + "/userdata/hostname /etc/hostname rw - tmpfs tmpfs rw", id},
		{"612 590 254:1 /var/lib/docker/containers/" + id + "/hosts /etc/hosts rw - ext4 /dev/vda1 rw", ""},
		{"612 590 254:1 /srv/" + id + "/hostname /data/hostname rw - ext4 /dev/vda1 rw", ""},
		{"612 590 254:1 /etc/hostname /etc/hostname rw - ext4 /dev/vda1 rw", ""},
		{"", ""},
	} {
		if got := containerID(c.mountinfo); got != c.want {
			t.Errorf("containerID(%q) = %q, want %q", c.mountinfo, got, c.want)
		}
	}
	clearEnv(t)
	path := t.TempDir() + "/mountinfo"
	if err := os.WriteFile(path, []byte("612 590 254:1 /var/lib/docker/containers/"+id+"/hostname /etc/hostname rw - ext4 /dev/vda1 rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mountinfoPath = path
	if cfg, _ := Load(); cfg.SelfContainer != id {
		t.Fatal(cfg.SelfContainer)
	}
	t.Setenv("LUDOCK_SELF_CONTAINER", "ludock")
	if cfg, _ := Load(); cfg.SelfContainer != "ludock" {
		t.Fatal(cfg.SelfContainer)
	}
}
