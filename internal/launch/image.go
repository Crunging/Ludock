// Package launch starts helper containers from Ludock's own image and runs
// `ludock helper` commands in them.
package launch

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/self"
)

var errImageUnavailable = apperr.New("HELPER_IMAGE_UNAVAILABLE", 503,
	"Ludock couldn’t find its own image in Docker, which it needs to reach the server’s files. Check that Docker is running; if Ludock’s container has a custom hostname, set LUDOCK_SELF_CONTAINER to its container name.")

// Outside a container there is no runtime image to reuse; helpers then need
// an explicitly configured, digest-pinned Ludock image.
var errNoHelperImage = apperr.New("HELPER_IMAGE_UNAVAILABLE", 503,
	"Ludock is not running in a container it can identify. Set LUDOCK_SELF_CONTAINER to its container name or ID; outside a container, set LUDOCK_HELPER_IMAGE to a Ludock image pinned by digest.")

var imageID = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Images resolves the helper image: this deployment's exact runtime, so
// helpers need no registry pull and get the same patches.
type Images struct {
	// Configured is LUDOCK_HELPER_IMAGE.
	Configured string
	Own        *self.Container

	mu sync.Mutex
	// speaking are configured images that answered helper.Version.
	speaking map[string]bool
}

// ErrHelperOutdated is a configured helper image that speaks another
// helper.Version than this Ludock: an older one could drop what uploads and
// archives now keep.
var ErrHelperOutdated = apperr.New("HELPER_IMAGE_OUTDATED", 503,
	"The helper image set for Ludock in LUDOCK_HELPER_IMAGE doesn’t match this Ludock, so it wasn’t used. Remove LUDOCK_HELPER_IMAGE, or set it to the image of this Ludock version.")

// Speaks confirms that a configured helper image speaks helper.Version
// before it uploads, backs up, or restores; Ludock's own image always does.
// ask runs the helper's version operation, once per image. A helper too old
// to know it refuses it as an unknown operation.
func (i *Images) Speaks(ask func() ([]byte, error)) error {
	if i.Configured == "" {
		return nil
	}
	i.mu.Lock()
	known := i.speaking[i.Configured]
	i.mu.Unlock()
	if known {
		return nil
	}
	out, err := ask()
	var answer struct{ Version int }
	if errors.Is(err, ErrUnsafe) || (err == nil && (json.Unmarshal(out, &answer) != nil || answer.Version != helper.Version)) {
		return ErrHelperOutdated
	}
	if err != nil {
		return err
	}
	i.mu.Lock()
	if i.speaking == nil {
		i.speaking = map[string]bool{}
	}
	i.speaking[i.Configured] = true
	i.mu.Unlock()
	return nil
}

// NewImages resolves helper images: configured (pinned by digest) wins,
// else the image of Ludock's own container.
func NewImages(configured string, own *self.Container) *Images {
	return &Images{Configured: configured, Own: own}
}

func (i *Images) Resolve(ctx context.Context) (string, error) {
	if i.Configured != "" {
		return ValidateImage(i.Configured)
	}
	own, err := i.Own.Read(ctx)
	if err != nil {
		return "", errImageUnavailable
	}
	switch own.Answer {
	case self.NotFound:
		return "", errNoHelperImage
	case self.Found:
		// Config.Image may be a moving tag; only the content-addressed ID is safe.
		if imageID.MatchString(own.Image) {
			return own.Image, nil
		}
	}
	return "", errImageUnavailable
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
var ErrUnpinnedImage = apperr.New("HELPER_IMAGE_UNPINNED", 503, "LUDOCK_HELPER_IMAGE must identify a trusted Ludock helper by its immutable sha256 digest.")

func ValidateImage(image string) (string, error) {
	if !pinned(image) {
		return "", ErrUnpinnedImage
	}
	return image, nil
}
