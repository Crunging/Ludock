package update

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
)

// Pulls send the inline credentials Docker saved for the image's registry,
// whichever way config.json spells it, and nothing for other registries.
func TestRegistryAuthMatchesRegistry(t *testing.T) {
	folder := t.TempDir()
	basic := base64.StdEncoding.EncodeToString([]byte("player:hunter2:with-colon"))
	config := `{"auths":{"https://index.docker.io/v1/":{"auth":"` + basic + `"},"ghcr.io":{"identitytoken":"token"},` +
		`"registry.example:5000":{"auth":"` + basic + `"}},"credsStore":"desktop"}`
	os.WriteFile(filepath.Join(folder, "config.json"), []byte(config), 0o600)
	decode := func(header string) map[string]string {
		data, err := base64.URLEncoding.DecodeString(header)
		var payload map[string]string
		if err != nil || json.Unmarshal(data, &payload) != nil {
			t.Fatal("invalid header", header)
		}
		return payload
	}
	for image, want := range map[string]map[string]string{
		"itzg/minecraft-server:java21":    {"username": "player", "password": "hunter2:with-colon", "serveraddress": "https://index.docker.io/v1/"},
		"docker.io/library/alpine:3":      {"username": "player", "password": "hunter2:with-colon", "serveraddress": "https://index.docker.io/v1/"},
		"ghcr.io/owner/game:latest":       {"identitytoken": "token", "serveraddress": "ghcr.io"},
		"registry.example:5000/game:1.2":  {"username": "player", "password": "hunter2:with-colon", "serveraddress": "registry.example:5000"},
		"quay.io/owner/game:latest":       nil,
		"localhost/registry.example:5000": nil,
	} {
		header, err := registryAuth(folder, image)
		if err != nil {
			t.Fatal(image, err)
		}
		if want == nil {
			if header != "" {
				t.Error(image, "sent credentials for another registry")
			}
			continue
		}
		if got := decode(header); len(got) != len(want) || got["username"] != want["username"] || got["password"] != want["password"] ||
			got["identitytoken"] != want["identitytoken"] || got["serveraddress"] != want["serveraddress"] {
			t.Error(image, got)
		}
	}
	if header, err := registryAuth(t.TempDir(), "ghcr.io/owner/game"); header != "" || err != nil {
		t.Error("a folder without config.json is anonymous:", header, err)
	}
	// A symlinked config.json may point anywhere on the host.
	linked := t.TempDir()
	os.Symlink(filepath.Join(folder, "config.json"), filepath.Join(linked, "config.json"))
	if _, err := registryAuth(linked, "ghcr.io/owner/game"); !apperr.Is(err, "REGISTRY_CONFIG_INVALID") {
		t.Error("followed a symlink:", err)
	}
}
