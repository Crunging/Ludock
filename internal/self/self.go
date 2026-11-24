// Package self reads Ludock's own container, which Files and backups, helper
// containers, and the backup folder check all depend on.
package self

import (
	"context"
	"net/http"
	"slices"
	"sync"

	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/identity"
)

// Answer is what Docker said about Ludock's own container. Each answer holds
// for as long as Ludock runs, so it is read once; a Docker failure is not an
// answer and nothing is remembered from it.
//
//   - Found: its mounts and image are known. Files and backups leave out game
//     folders that hold or are inside one of its mounts, helpers run from its
//     image, and a backup folder must be one of its writable mounts.
//   - NotFound (404, or no name): Ludock isn't in a container it can
//     identify, such as a development build or a custom hostname with
//     LUDOCK_SELF_CONTAINER unset. Every safe game folder is offered, helpers
//     need LUDOCK_HELPER_IMAGE, and no backup folder can be checked.
//   - Refused (403): something such as a socket proxy won't let Ludock
//     inspect itself. No game folder is offered at all, since any could
//     overlap its own; helpers and backup folders can't be checked.
//   - Unread, the zero value, is never returned by Read; Files treats it as
//     Refused.
type Answer int

const (
	Unread Answer = iota
	Found
	NotFound
	Refused
)

// Info is Ludock's own container as Docker described it; only a Found
// answer has the rest.
type Info struct {
	Answer Answer
	// Image is the content-addressed image ID.
	Image  string
	Mounts []identity.Mount
}

// Container is Ludock's own container, read from Docker on first use.
type Container struct {
	docker *docker.Client
	name   string

	mu    sync.Mutex
	known *Info
}

// New names Ludock's own container: LUDOCK_SELF_CONTAINER, or what
// configuration found. An empty name means it isn't in one.
func New(client *docker.Client, name string) *Container {
	return &Container{docker: client, name: name}
}

func (c *Container) Name() string { return c.name }

// Read returns the remembered answer, or asks Docker. The lock isn't held
// while Docker answers, so a slow inspection never holds up another caller's
// own deadline; callers that race on the first read each ask.
func (c *Container) Read(ctx context.Context) (Info, error) {
	c.mu.Lock()
	known := c.known
	c.mu.Unlock()
	if known != nil {
		return clone(*known), nil
	}
	answer := Info{Answer: NotFound}
	if c.name != "" {
		// A 403 or 404 from version negotiation is no answer about the container.
		if err := c.docker.Negotiate(ctx); err != nil {
			return Info{}, err
		}
		info, err := c.docker.Inspect(ctx, c.name)
		switch docker.StatusCode(err) {
		case http.StatusNotFound:
		case http.StatusForbidden:
			answer = Info{Answer: Refused}
		default:
			if err != nil {
				return Info{}, err
			}
			answer = Info{Answer: Found, Image: info.Image, Mounts: identity.DockerMounts(info)}
		}
	}
	c.mu.Lock()
	if c.known == nil {
		c.known = &answer
	}
	known = c.known
	c.mu.Unlock()
	return clone(*known), nil
}

func clone(info Info) Info {
	info.Mounts = slices.Clone(info.Mounts)
	return info
}
