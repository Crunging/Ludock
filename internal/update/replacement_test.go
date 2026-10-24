package update

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/Crunging/Ludock/internal/docker"
)

const containerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func raw(t *testing.T, value map[string]any) map[string]json.RawMessage {
	t.Helper()
	result := map[string]json.RawMessage{}
	for key, item := range value {
		encoded, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		result[key] = encoded
	}
	return result
}

// replacement builds a plan's create request and decodes it as Docker would
// read it.
func replacement(t *testing.T, plan *Plan) map[string]any {
	t.Helper()
	body, err := plan.Replacement(&docker.ImageInspect{ID: "sha256:new"}, "operation")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(body)
	var decoded map[string]any
	json.Unmarshal(encoded, &decoded)
	return decoded
}

func plan(t *testing.T, config, host, image map[string]any) *Plan {
	t.Helper()
	spec := &docker.ContainerSpec{ID: containerID, Name: "/mc", Image: "sha256:old", Config: raw(t, config), HostConfig: raw(t, host)}
	return &Plan{Spec: spec, Image: "itzg/minecraft-server:latest", previous: &docker.ImageInspect{ID: "sha256:old", Config: raw(t, image)}}
}

// Inspection merges image defaults into a container's configuration. The
// replacement keeps only what its owner set, so the new image's defaults
// (a new PATH, command, or healthcheck) apply.
func TestReplacementLeavesImageDefaultsToNewImage(t *testing.T) {
	image := map[string]any{"Env": []string{"PATH=/old/bin", "VERSION=1"}, "Cmd": []string{"start"}, "Entrypoint": []string{"/init"},
		"WorkingDir": "/data", "User": "1000", "Labels": map[string]string{"org.opencontainers.image.version": "1", "shared": "image"},
		"ExposedPorts": map[string]any{"25565/tcp": map[string]any{}}, "Volumes": map[string]any{"/data": map[string]any{}},
		"Healthcheck": map[string]any{"Test": []string{"CMD", "mc-health"}, "Interval": 5e9}}
	config := map[string]any{"Image": "itzg/minecraft-server:latest", "Hostname": containerID[:12],
		"Env": []string{"PATH=/old/bin", "VERSION=1", "EULA=TRUE"}, "Cmd": []string{"start"}, "Entrypoint": []string{"/init"},
		"WorkingDir": "/data", "User": "1000", "Labels": map[string]string{"org.opencontainers.image.version": "1", "shared": "mine",
			"com.docker.compose.image": "sha256:old"},
		"ExposedPorts": map[string]any{"25565/tcp": map[string]any{}, "25575/tcp": map[string]any{}},
		"Volumes":      map[string]any{"/data": map[string]any{}, "/extra": map[string]any{}},
		"Healthcheck":  map[string]any{"Test": []string{"CMD", "mc-health"}, "Interval": 30e9}, "Tty": true, "OpenStdin": true}
	body := replacement(t, plan(t, config, map[string]any{"PortBindings": map[string]any{"19132/udp": []any{}}}, image))
	for _, key := range []string{"Cmd", "Entrypoint", "WorkingDir", "User", "Hostname"} {
		if _, kept := body[key]; kept {
			t.Errorf("%s pinned to the old image: %v", key, body[key])
		}
	}
	want := map[string]any{
		"Image": "itzg/minecraft-server:latest", "Env": []any{"EULA=TRUE"}, "Tty": true, "OpenStdin": true,
		"Labels":       map[string]any{"shared": "mine", "com.docker.compose.image": "sha256:new", "ludock.update": "operation"},
		"ExposedPorts": map[string]any{"25575/tcp": map[string]any{}, "19132/udp": map[string]any{}},
		"Volumes":      map[string]any{"/extra": map[string]any{}},
		"Healthcheck":  map[string]any{"Interval": 30e9},
	}
	for key, value := range want {
		if !reflect.DeepEqual(body[key], value) {
			t.Errorf("%s = %v, want %v", key, body[key], value)
		}
	}

	// Docker lists the owner's variables before the image's others, so one
	// the owner set to the image's default value is still recognized as
	// theirs; the new image may change that default.
	config["Env"] = []string{"VERSION=1", "EULA=TRUE", "PATH=/old/bin"}
	if body := replacement(t, plan(t, config, nil, image)); !reflect.DeepEqual(body["Env"], []any{"VERSION=1", "EULA=TRUE"}) {
		t.Error("own environment lost:", body["Env"])
	}
	config["Env"] = []string{"PATH=/old/bin", "VERSION=1", "EULA=TRUE"}
	for _, test := range []struct{ env, image, want []string }{
		// Docker skips an image variable repeated in the image itself.
		{[]string{"X=custom", "A=old", "B=default"}, []string{"A=old", "A=duplicate", "B=default"}, []string{"X=custom"}},
		// Docker uses the last entry, so every entry of a changed variable
		// stays, in order.
		{[]string{"PATH=/custom/bin", "PATH=/image/bin"}, []string{"PATH=/image/bin"}, []string{"PATH=/custom/bin", "PATH=/image/bin"}},
		// Without variables of its own the container has exactly the image's.
		{[]string{"PATH=/image/bin"}, []string{"PATH=/image/bin"}, []string{}},
	} {
		if got := ownEnv(test.env, test.image); !slices.Equal(got, test.want) {
			t.Errorf("ownEnv(%v, %v) = %v, want %v", test.env, test.image, got, test.want)
		}
	}
	// An explicit entrypoint equal to the image's, with no command, cleared
	// the image's command; dropping it would bring a command back.
	config["Cmd"] = nil
	if body := replacement(t, plan(t, config, nil, image)); !reflect.DeepEqual(body["Entrypoint"], []any{"/init"}) || body["Cmd"] != nil {
		t.Error("explicit entrypoint lost:", body["Entrypoint"], body["Cmd"])
	}
	// The owner's command runs with the new image's entrypoint.
	config["Cmd"] = []string{"debug"}
	if body := replacement(t, plan(t, config, nil, image)); body["Entrypoint"] != nil || !reflect.DeepEqual(body["Cmd"], []any{"debug"}) {
		t.Error("own command lost:", body["Entrypoint"], body["Cmd"])
	}
	// --entrypoint "" cleared the image's entrypoint; an empty value would
	// inherit it again.
	config["Entrypoint"], config["Cmd"] = nil, []string{"/custom"}
	if body := replacement(t, plan(t, config, nil, image)); !reflect.DeepEqual(body["Entrypoint"], []any{""}) {
		t.Error("cleared entrypoint not reset:", body["Entrypoint"])
	}
	config["Entrypoint"] = []string{"/init"}
	// An owner's hostname stays, except where the network namespace isn't
	// the container's own.
	config["Hostname"] = "survival"
	if body := replacement(t, plan(t, config, nil, image)); body["Hostname"] != "survival" {
		t.Error("hostname lost:", body["Hostname"])
	}
	if body := replacement(t, plan(t, config, map[string]any{"NetworkMode": "host"}, image)); body["Hostname"] != nil {
		t.Error("hostname kept on the host network:", body["Hostname"])
	}
}

// Anonymous volumes hold game data as often as named ones. Each is mounted by
// name at its destination so the replacement keeps the data; volumes and
// folders the owner named are left as configured.
func TestReplacementKeepsAnonymousVolumes(t *testing.T) {
	spec := func(host map[string]any, mounts ...docker.MountPoint) *Plan {
		p := plan(t, map[string]any{"Image": "game"}, host, nil)
		p.Spec.Mounts = mounts
		return p
	}
	mountsOf := func(body map[string]any) []any {
		mounts, _ := body["HostConfig"].(map[string]any)["Mounts"].([]any)
		return mounts
	}
	// Declared by the image or `docker run -v /state`.
	body := replacement(t, spec(map[string]any{"Binds": []string{"worlds:/worlds", "/srv/config:/config:ro"}},
		docker.MountPoint{Type: "volume", Name: "a1b2", Destination: "/state", RW: true},
		docker.MountPoint{Type: "volume", Name: "worlds", Destination: "/worlds", RW: true},
		docker.MountPoint{Type: "bind", Source: "/srv/config", Destination: "/config"}))
	want := []any{map[string]any{"Type": "volume", "Source": "a1b2", "Target": "/state", "ReadOnly": false}}
	if !reflect.DeepEqual(mountsOf(body), want) {
		t.Errorf("mounts = %v, want %v", mountsOf(body), want)
	}
	// Compose's `- /state` is a mount without a source; its options stay.
	compose := map[string]any{"Mounts": []any{map[string]any{"Type": "volume", "Target": "/state", "VolumeOptions": map[string]any{"NoCopy": true}},
		map[string]any{"Type": "volume", "Source": "worlds", "Target": "/worlds"}}}
	body = replacement(t, spec(compose, docker.MountPoint{Type: "volume", Name: "a1b2", Destination: "/state", RW: true},
		docker.MountPoint{Type: "volume", Name: "worlds", Destination: "/worlds", RW: true}))
	want = []any{map[string]any{"Type": "volume", "Source": "a1b2", "Target": "/state", "VolumeOptions": map[string]any{"NoCopy": true}},
		map[string]any{"Type": "volume", "Source": "worlds", "Target": "/worlds"}}
	if !reflect.DeepEqual(mountsOf(body), want) {
		t.Errorf("mounts = %v, want %v", mountsOf(body), want)
	}
	// An API client's destination-only bind made it; Docker refuses one
	// destination in both lists, so the bind itself names the volume.
	body = replacement(t, spec(map[string]any{"Binds": []string{"/state", "worlds:/worlds"}},
		docker.MountPoint{Type: "volume", Name: "a1b2", Destination: "/state", RW: true},
		docker.MountPoint{Type: "volume", Name: "worlds", Destination: "/worlds", RW: true}))
	if binds := body["HostConfig"].(map[string]any)["Binds"]; mountsOf(body) != nil || !reflect.DeepEqual(binds, []any{"a1b2:/state", "worlds:/worlds"}) {
		t.Errorf("binds = %v, mounts = %v", binds, mountsOf(body))
	}
	// Volumes from another container come from it again; mounting them too
	// would duplicate the destination.
	inherited := spec(map[string]any{"VolumesFrom": []string{"data"}}, docker.MountPoint{Type: "volume", Name: "c3d4", Destination: "/shared", RW: true})
	inherited.inherited = map[string]bool{"/shared": true}
	if mounts := mountsOf(replacement(t, inherited)); mounts != nil {
		t.Error("inherited volume mounted again:", mounts)
	}
	// An explicit unnamed mount overrides the inherited one at its destination.
	overridden := spec(map[string]any{"VolumesFrom": []string{"data"}, "Mounts": []any{map[string]any{"Type": "volume", "Target": "/shared"}}},
		docker.MountPoint{Type: "volume", Name: "e5f6", Destination: "/shared", RW: true})
	overridden.inherited = map[string]bool{"/shared": true}
	want = []any{map[string]any{"Type": "volume", "Source": "e5f6", "Target": "/shared"}}
	if mounts := mountsOf(replacement(t, overridden)); !reflect.DeepEqual(mounts, want) {
		t.Errorf("mounts = %v, want %v", mounts, want)
	}
}

// Each network keeps its static address and aliases. Docker's short-ID alias
// and a MAC derived from the old address would be wrong for the new
// container; a MAC the owner chose stays.
func TestReplacementNetworkEndpoints(t *testing.T) {
	p := plan(t, map[string]any{"Image": "game"}, map[string]any{"NetworkMode": "games"}, nil)
	p.Spec.NetworkSettings.Networks = map[string]docker.EndpointSettings{
		"games": {Aliases: []string{"minecraft", containerID[:12]}, IPAMConfig: &docker.EndpointIPAMConfig{IPv4Address: "172.20.0.10"},
			IPAddress: "172.20.0.10", MacAddress: "02:42:ac:14:00:0a"},
		"bridge": {Aliases: []string{"ignored"}, IPAddress: "172.17.0.2", MacAddress: "52:54:00:12:34:56", IPAMConfig: &docker.EndpointIPAMConfig{}},
	}
	body := replacement(t, p)
	want := map[string]any{"EndpointsConfig": map[string]any{
		"games":  map[string]any{"Aliases": []any{"minecraft"}, "IPAMConfig": map[string]any{"IPv4Address": "172.20.0.10"}},
		"bridge": map[string]any{"MacAddress": "52:54:00:12:34:56"},
	}}
	if !reflect.DeepEqual(body["NetworkingConfig"], want) {
		t.Errorf("networking = %v, want %v", body["NetworkingConfig"], want)
	}
	for _, mode := range []string{"host", "none", "container:" + containerID} {
		p := plan(t, map[string]any{"Image": "game"}, map[string]any{"NetworkMode": mode}, nil)
		if body := replacement(t, p); body["NetworkingConfig"] != nil {
			t.Error(mode, "got endpoints:", body["NetworkingConfig"])
		}
	}
}
