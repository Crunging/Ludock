package fakedocker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"github.com/Crunging/Ludock/internal/docker"
)

func (e *Engine) AddImage(reference string) {
	e.mu.Lock()
	e.images[reference] = "sha256:" + strings.Repeat("2", 64)
	e.mu.Unlock()
}

// SetImageConfig sets the container defaults of an image ID, as its
// Dockerfile would.
func (e *Engine) SetImageConfig(id string, config map[string]any) {
	e.mu.Lock()
	e.imageConfigs[id] = config
	e.mu.Unlock()
}

// SetVariantConfig sets the container defaults of one platform variant of
// an image ID, which inspection returns when asked for that platform.
func (e *Engine) SetVariantConfig(id string, platform docker.Platform, config map[string]any) {
	encoded, _ := json.Marshal(platform)
	e.mu.Lock()
	e.imageConfigs[id+" "+string(encoded)] = config
	e.mu.Unlock()
}

// ManifestDigest is the registry digest the fake gives image ID id.
func ManifestDigest(id string) string {
	sum := sha256.Sum256([]byte("manifest " + id))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// PublishImage points reference's tag in the fake registry at image ID id,
// which pulling it then yields. The new image keeps the defaults of the one
// the tag held.
func (e *Engine) PublishImage(reference, id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.imageConfigs[id]; !ok {
		if config, ok := e.imageConfigs[e.tagLocked(reference)]; ok {
			e.imageConfigs[id] = config
		}
	}
	e.registry[reference] = id
}

// FailRegistry makes registry lookups of reference fail with status, or
// succeed again with 0.
func (e *Engine) FailRegistry(reference string, status int) {
	e.mu.Lock()
	e.registryFail[reference] = status
	e.mu.Unlock()
}

// tagLocked is the image ID reference's tag points to: the published one,
// else the local tag, else the image a container created from it runs, as
// if the registry still held what was pulled.
func (e *Engine) tagLocked(reference string) string {
	if id, ok := e.registry[reference]; ok {
		return id
	}
	if id, ok := e.images[reference]; ok {
		return id
	}
	for _, id := range e.order {
		if c := e.containers[id]; c.Config.Image == reference {
			return c.Image
		}
	}
	return ""
}

// repoDigestsLocked lists "repository@digest" for each reference whose tag
// or container uses image ID id, as a pull of it would have recorded.
func (e *Engine) repoDigestsLocked(id string) []string {
	references := map[string]bool{}
	for reference, local := range e.images {
		if local == id {
			references[reference] = true
		}
	}
	for _, c := range e.containers {
		if c.Image == id && c.Config.Image != "" {
			references[c.Config.Image] = true
		}
	}
	digests := []string{}
	for reference := range references {
		repository, _, _ := strings.Cut(reference, "@")
		if slash, colon := strings.LastIndex(repository, "/"), strings.LastIndex(repository, ":"); colon > slash {
			repository = repository[:colon]
		}
		digests = append(digests, repository+"@"+ManifestDigest(id))
	}
	sort.Strings(digests)
	return slices.Compact(digests)
}
