// Package launch starts helper containers from Ludock's own image and runs
// `ludock helper` commands in them.
package launch

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
)

var errImageUnavailable = apperr.New("HELPER_IMAGE_UNAVAILABLE", 503,
	"Ludock could not identify its runtime image. Check Docker connectivity and any LUDOCK_SELF_CONTAINER setting.")

// Outside a container there is no runtime image to reuse; helpers then need
// an explicitly configured, digest-pinned Ludock image.
var errNoHelperImage = apperr.New("HELPER_IMAGE_UNAVAILABLE", 503,
	"Ludock is not running in a container it can identify. Set LUDOCK_HELPER_IMAGE to a Ludock image pinned by digest, or set LUDOCK_SELF_CONTAINER.")

var imageID = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Images resolves the helper image: this deployment's exact runtime, so
// helpers need no registry pull and get the same patches.
type Images struct {
	Docker *docker.Client
	// Configured is LUDOCK_HELPER_IMAGE; Self is LUDOCK_SELF_CONTAINER or
	// HOSTNAME.
	Configured, Self string

	mu       sync.Mutex
	resolved string
}

// NewImages resolves helper images: configured (pinned by digest) wins,
// else the image of Ludock's own container named self.
func NewImages(client *docker.Client, configured, self string) *Images {
	return &Images{Docker: client, Configured: configured, Self: self}
}

// Resolve returns the helper image reference.
func (i *Images) Resolve(ctx context.Context) (string, error) {
	if i.Configured != "" {
		return ValidateImage(i.Configured)
	}
	if i.Self == "" {
		return "", errNoHelperImage
	}
	i.mu.Lock()
	resolved := i.resolved
	i.mu.Unlock()
	if resolved != "" {
		return resolved, nil
	}
	info, err := i.Docker.Inspect(ctx, i.Self)
	if err != nil {
		// A native run or custom hostname need not name a container. Other
		// daemon failures stay retryable errors.
		if docker.StatusCode(err) == http.StatusNotFound {
			return "", errNoHelperImage
		}
		return "", errImageUnavailable
	}
	// Config.Image may be a moving tag; only the content-addressed ID is safe.
	if !imageID.MatchString(info.Image) {
		return "", errImageUnavailable
	}
	i.mu.Lock()
	i.resolved = info.Image
	i.mu.Unlock()
	return info.Image, nil
}

// Docker's reference grammar and familiar-name rules
// (github.com/distribution/reference v0.6.0). This validates syntax, not
// registry trust or image contents.
var (
	registryPattern  = regexp.MustCompile(`^(?:[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*|\[[A-Fa-f0-9:]+\])(?::[0-9]+)?$`)
	componentPattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	tagPattern       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	pinnedPattern    = regexp.MustCompile(`^([^@]+)@sha256:[a-f0-9]{64}$`)
	registryHint     = regexp.MustCompile(`[.:A-Z]`)
)

func pinned(image string) bool {
	if strings.ContainsAny(image, " \t\r\n\f\v") {
		return false
	}
	match := pinnedPattern.FindStringSubmatch(image)
	if match == nil {
		return false
	}
	repository := match[1]
	if separator := strings.LastIndex(repository, ":"); separator > strings.LastIndex(repository, "/") {
		if !tagPattern.MatchString(repository[separator+1:]) {
			return false
		}
		repository = repository[:separator]
	}
	registry := "docker.io"
	if slash := strings.Index(repository, "/"); slash != -1 {
		first := repository[:slash]
		if first == "localhost" || registryHint.MatchString(first) {
			registry, repository = first, repository[slash+1:]
		}
	}
	if !registryPattern.MatchString(registry) {
		return false
	}
	if registry == "index.docker.io" {
		registry = "docker.io"
	}
	// Docker adds this namespace before applying its 255-character limit.
	if registry == "docker.io" && !strings.Contains(repository, "/") {
		repository = "library/" + repository
	}
	if len(repository) > 255 {
		return false
	}
	for _, component := range strings.Split(repository, "/") {
		if !componentPattern.MatchString(component) {
			return false
		}
	}
	return true
}

// ErrUnpinnedImage rejects a LUDOCK_HELPER_IMAGE without a digest.
var ErrUnpinnedImage = errors.New("LUDOCK_HELPER_IMAGE must identify a trusted Ludock helper by its immutable sha256 digest.")

// ValidateImage accepts only digest-pinned references.
func ValidateImage(image string) (string, error) {
	if !pinned(image) {
		return "", ErrUnpinnedImage
	}
	return image, nil
}
