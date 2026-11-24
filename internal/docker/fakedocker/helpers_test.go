package fakedocker

import (
	"testing"

	"github.com/Crunging/Ludock/internal/docker"
)

// A helper's paths map to the host through the deepest mount that holds
// them, on whole path components, whatever order the mounts are in.
func TestHelperPathsUseDeepestMount(t *testing.T) {
	var c Container
	c.CreateRequest.HostConfig = &docker.HostConfig{Mounts: []docker.MountSettings{
		{Target: "/data", Source: "/host/a"}, {Target: "/data/mods", Source: "/host/b"}, {Target: "/data/a&b", Source: "/host/c"}}}
	for argument, want := range map[string]string{
		`{"root":"/data/mods/x","base":"/data"}`:        `{"root":"/host/b/x","base":"/host/a"}`,
		`{"root":"/data/mods","paths":["/data/world"]}`: `{"root":"/host/b","paths":["/host/a/world"]}`,
		`{"root":"/database","path":"/data/modsx"}`:     `{"root":"/database","path":"/host/a/modsx"}`,
		`{"root":"/data/a\u0026b/x"}`:                   `{"root":"/host/c/x"}`,
	} {
		if got := hostArgs(&c, []string{argument})[0]; got != want {
			t.Errorf("%s: %s", argument, got)
		}
	}
}
