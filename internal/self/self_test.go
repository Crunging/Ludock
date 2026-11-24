package self_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/self"
)

// Each answer Docker gives about Ludock's own container means one thing to
// Files, helper images, and the backup folder check, and is remembered after
// Docker stops giving it; a Docker failure is retried. A game folder that is
// Ludock's backup folder is never offered, and none is when Ludock can't read
// its own mounts.
func TestOwnContainerAnswers(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, container string
		fault           int
		faulted         string
		roots           []string
		image           string
		backups         bool
	}{
		{name: "found", container: "ludock", roots: []string{"/mods"}, image: "sha256:", backups: true},
		{name: "not found", container: "missing", roots: []string{"/backups", "/mods"}, image: "Ludock is not running in a container"},
		{name: "refused", container: "ludock", fault: 403, image: "Ludock couldn’t find its own image"},
		{name: "docker failed", container: "ludock", fault: 500, image: "Ludock couldn’t find its own image"},
		{name: "version refused", container: "ludock", fault: 403, faulted: "/version", image: "Ludock couldn’t find its own image"},
	} {
		t.Run(c.name, func(t *testing.T) {
			engine, err := fakedocker.Start(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { engine.Close() })
			folder := t.TempDir()
			id := engine.AddContainer(fakedocker.GameOptions{ID: "ludock", Name: "ludock", Image: "ghcr.io/crunging/ludock:1", Running: true,
				Mounts: []docker.MountPoint{{Type: "bind", Source: folder, Destination: folder, RW: true}}})
			if c.faulted == "" {
				c.faulted = "/containers/ludock/json"
			}
			if c.fault != 0 {
				engine.AddFault(c.faulted, c.fault, false, 0)
			}
			own := self.New(docker.New(engine.Socket), c.container)
			game := []identity.Mount{{Type: "bind", Source: folder, Destination: "/backups", Writable: true},
				{Type: "bind", Source: "/srv/mods", Destination: "/mods", Writable: true}}
			failing := c.fault >= 500 || c.faulted == "/version"
			for attempt := range 2 {
				info, err := own.Read(ctx)
				if failing != (err != nil) {
					t.Fatal(attempt, err)
				}
				var roots []string
				for _, root := range files.Roots("custom", nil, game, info) {
					roots = append(roots, root.Path)
				}
				if err == nil && strings.Join(roots, ",") != strings.Join(c.roots, ",") {
					t.Fatal(attempt, roots)
				}
				image, err := (&launch.Images{Own: own}).Resolve(ctx)
				if c.image == "sha256:" {
					if image != engine.Container(id).Image {
						t.Fatal(attempt, image, err)
					}
				} else if !apperr.Is(err, "HELPER_IMAGE_UNAVAILABLE") || !strings.HasPrefix(err.Error(), c.image) {
					t.Fatal(attempt, err)
				}
				_, err = backups.NewStorage([]string{folder}, own).ValidateSettings(ctx, backups.Settings{Destination: folder})
				if c.backups != (err == nil) {
					t.Fatal(attempt, err)
				}
				engine.ClearFaults()
				// A remembered answer never asks Docker again.
				engine.AddFault("/containers/", 500, false, 0)
				if failing {
					engine.ClearFaults()
					// Nothing was remembered, so Docker's next answer counts.
					failing, c.roots, c.image, c.backups = false, []string{"/mods"}, "sha256:", true
				}
			}
		})
	}
}
