package update

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Crunging/Ludock/internal/apperr"
)

var errRegistryConfig = apperr.New("REGISTRY_CONFIG_INVALID", 409,
	"Ludock could not read registry credentials from LUDOCK_DOCKER_CONFIG. Check that its config.json is readable and valid.")

// registryAuth is the X-Registry-Auth value for pulling image, from the
// inline credentials in folder/config.json, or "" without any for its
// registry. Credential helpers need the Docker CLI, which Ludock doesn't run.
func registryAuth(folder, image string) (string, error) {
	if folder == "" {
		return "", nil
	}
	file, err := os.OpenFile(filepath.Join(folder, "config.json"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", errRegistryConfig
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return "", errRegistryConfig
	}
	var config struct {
		Auths map[string]struct {
			Auth          string `json:"auth"`
			IdentityToken string `json:"identitytoken"`
		} `json:"auths"`
	}
	if json.Unmarshal(data, &config) != nil {
		return "", errRegistryConfig
	}
	registry := registryHost(image)
	for key, entry := range config.Auths {
		if normalizeRegistry(key) != registry || (entry.Auth == "" && entry.IdentityToken == "") {
			continue
		}
		payload := map[string]string{"serveraddress": key}
		if entry.IdentityToken != "" {
			payload["identitytoken"] = entry.IdentityToken
		} else {
			decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
			username, password, ok := strings.Cut(string(decoded), ":")
			if err != nil || !ok {
				return "", errRegistryConfig
			}
			payload["username"], payload["password"] = username, password
		}
		encoded, _ := json.Marshal(payload)
		return base64.URLEncoding.EncodeToString(encoded), nil
	}
	return "", nil
}

// registryHost is the registry an image reference pulls from.
func registryHost(image string) string {
	if first, _, found := strings.Cut(image, "/"); found && isRegistry(first) {
		return normalizeRegistry(first)
	}
	return "docker.io"
}

// isRegistry reports whether a reference's first path component names a
// registry host, by Docker's rules: one with a dot, a port, or an uppercase
// letter, or localhost.
func isRegistry(first string) bool {
	return strings.ContainsAny(first, ".:") || first == "localhost" || strings.ToLower(first) != first
}

// normalizeRegistry reduces a config.json key such as
// "https://index.docker.io/v1/" to its host.
func normalizeRegistry(key string) string {
	key = strings.ToLower(key)
	key = strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
	key, _, _ = strings.Cut(key, "/")
	switch key {
	case "index.docker.io", "registry-1.docker.io":
		return "docker.io"
	}
	return key
}
