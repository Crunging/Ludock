package update

import (
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"path"
	"reflect"
	"slices"
	"strings"

	"github.com/Crunging/Ludock/internal/docker"
)

// composeImageLabel records the image Compose created a container from.
// Compose recreates a container whose label differs from its current image,
// so the replacement records the new image the same way.
const composeImageLabel = "com.docker.compose.image"

// OperationLabel names the update that created a container, so recovery can
// tell its replacement from another container under the temporary name.
const OperationLabel = "ludock.update"

func decodeInto(fields map[string]json.RawMessage, target any) error {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}

func decode(raw json.RawMessage) any {
	var value any
	if len(raw) > 0 {
		json.Unmarshal(raw, &value)
	}
	return value
}

func blank(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return typed == ""
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	}
	return false
}

// same compares two JSON values, treating null, "", [], and {} as equal.
func same(left, right any) bool {
	if blank(left) && blank(right) {
		return true
	}
	return reflect.DeepEqual(left, right)
}

func encode(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

// Replacement is the create request for a copy of the planned container
// that runs image. Inspection merges the image's defaults into the
// container's configuration; values still equal to the old image's defaults
// are left out so the new image supplies its own, as Watchtower does.
// Anonymous volumes are mounted by name, as Compose does, so the replacement
// keeps their data instead of starting with empty ones.
func (p *Plan) Replacement(image *docker.ImageInspect, operationID string) (map[string]any, error) {
	spec := p.Spec
	config := maps.Clone(spec.Config)
	host := maps.Clone(spec.HostConfig)
	defaults := map[string]json.RawMessage{}
	if p.previous != nil && p.previous.Config != nil {
		defaults = p.previous.Config
	}
	networkMode, _ := decode(host["NetworkMode"]).(string)
	utsMode, _ := decode(host["UTSMode"]).(string)

	for _, key := range []string{"WorkingDir", "User", "StopSignal", "Shell"} {
		if same(decode(config[key]), decode(defaults[key])) {
			delete(config, key)
		}
	}
	// Build-time and deprecated fields; MAC addresses belong to endpoints.
	for _, key := range []string{"OnBuild", "ArgsEscaped", "MacAddress"} {
		delete(config, key)
	}
	entrypoint, cmd := decode(config["Entrypoint"]), decode(config["Cmd"])
	if blank(entrypoint) && !blank(decode(defaults["Entrypoint"])) {
		// The owner cleared the image's entrypoint (--entrypoint ""); an empty
		// value would inherit it again, so send Docker's explicit reset.
		config["Entrypoint"] = encode([]string{""})
	} else if same(entrypoint, decode(defaults["Entrypoint"])) {
		switch {
		case same(cmd, decode(defaults["Cmd"])):
			delete(config, "Entrypoint")
			delete(config, "Cmd")
		// Setting an entrypoint clears the image's command. An explicit
		// entrypoint equal to the image's, with no command, must stay
		// explicit, or the new image's command would come back.
		case blank(cmd):
		default:
			delete(config, "Entrypoint")
		}
	}
	// Docker names the host after the container's short ID unless one is set.
	hostname, _ := decode(config["Hostname"]).(string)
	if hostname == spec.ID[:min(12, len(spec.ID))] || networkMode == "host" || strings.HasPrefix(networkMode, "container:") || utsMode != "" {
		delete(config, "Hostname")
	}

	var env, imageEnv []string
	json.Unmarshal(config["Env"], &env)
	json.Unmarshal(defaults["Env"], &imageEnv)
	config["Env"] = encode(ownEnv(env, imageEnv))

	var labels, imageLabels map[string]string
	json.Unmarshal(config["Labels"], &labels)
	json.Unmarshal(defaults["Labels"], &imageLabels)
	for key, value := range imageLabels {
		if current, ok := labels[key]; ok && current == value {
			delete(labels, key)
		}
	}
	if recorded, ok := labels[composeImageLabel]; ok {
		labels[composeImageLabel] = p.composeImage(recorded, image)
	}
	if labels == nil {
		labels = map[string]string{}
	}
	labels[OperationLabel] = operationID
	config["Labels"] = encode(labels)

	var volumes, imageVolumes map[string]any
	json.Unmarshal(config["Volumes"], &volumes)
	json.Unmarshal(defaults["Volumes"], &imageVolumes)
	for key := range imageVolumes {
		delete(volumes, key)
	}
	config["Volumes"] = encode(volumes)

	var ports, imagePorts, bindings map[string]any
	json.Unmarshal(config["ExposedPorts"], &ports)
	json.Unmarshal(defaults["ExposedPorts"], &imagePorts)
	json.Unmarshal(host["PortBindings"], &bindings)
	for key := range imagePorts {
		delete(ports, key)
	}
	for key := range bindings {
		if ports == nil {
			ports = map[string]any{}
		}
		ports[key] = map[string]any{}
	}
	config["ExposedPorts"] = encode(ports)

	// Docker fills each unset healthcheck field from the image.
	if healthcheck, ok := decode(config["Healthcheck"]).(map[string]any); ok {
		imageHealthcheck, _ := decode(defaults["Healthcheck"]).(map[string]any)
		for key, value := range healthcheck {
			if same(value, imageHealthcheck[key]) {
				delete(healthcheck, key)
			}
		}
		if len(healthcheck) == 0 {
			delete(config, "Healthcheck")
		} else {
			config["Healthcheck"] = encode(healthcheck)
		}
	}

	// Inspection reports legacy links as "/other:/self/alias"; create takes
	// "other:alias".
	var links []string
	json.Unmarshal(host["Links"], &links)
	for index, link := range links {
		name, alias, ok := strings.Cut(link, ":")
		if !ok {
			return nil, fmt.Errorf("invalid link")
		}
		links[index] = strings.TrimPrefix(name, "/") + ":" + path.Base(alias)
	}
	if links != nil {
		host["Links"] = encode(links)
	}

	binds, mounts, err := p.mounts()
	if err != nil {
		return nil, err
	}
	if binds != nil {
		host["Binds"] = encode(binds)
	}
	if mounts != nil {
		host["Mounts"] = encode(mounts)
	}

	body := map[string]any{}
	for key, value := range config {
		body[key] = value
	}
	body["Image"] = p.Image
	body["HostConfig"] = host
	if networkMode != "host" && networkMode != "none" && !strings.HasPrefix(networkMode, "container:") {
		body["NetworkingConfig"] = map[string]any{"EndpointsConfig": p.endpoints()}
	}
	return body, nil
}

// ownEnv recovers the environment a container was created with from the
// one Docker reports. Docker lists the owner's variables first, then appends
// the image's other variables in image order, so the shortest list that
// merges into the reported one is the owner's: a variable the owner set to
// the image's default value stays theirs if the new image changes the
// default. The whole list always merges into itself, since every image
// variable is in it, so the search stops short of it; an environment no
// shorter list explains keeps every variable the owner changed.
func ownEnv(env, image []string) []string {
	for split := 0; split < len(env); split++ {
		if slices.Equal(mergeEnv(env[:split], image), env) {
			return env[:split]
		}
	}
	changed := map[string]bool{}
	for _, entry := range env {
		if !slices.Contains(image, entry) {
			changed[envKey(entry)] = true
		}
	}
	// Docker uses a variable's last entry, so all of a changed variable's
	// entries stay, in order.
	return slices.DeleteFunc(slices.Clone(env), func(entry string) bool { return !changed[envKey(entry)] })
}

func envKey(entry string) string {
	name, _, _ := strings.Cut(entry, "=")
	return name
}

// mergeEnv adds an image's variables to a create request's as Docker does:
// an empty request takes the image's as they are; otherwise each image
// variable is appended unless a variable of that name is already listed.
func mergeEnv(own, image []string) []string {
	if len(own) == 0 {
		return image
	}
	merged := slices.Clone(own)
	for _, entry := range image {
		if !slices.ContainsFunc(merged, func(listed string) bool { return envKey(listed) == envKey(entry) }) {
			merged = append(merged, entry)
		}
	}
	return merged
}

// composeImage is the image label for the replacement, recorded the way the
// original's was: older Compose records the image ID, newer Compose the
// digest of the platform manifest the container runs. An unrecognized value
// is kept; Compose then recreates the container once more.
func (p *Plan) composeImage(recorded string, image *docker.ImageInspect) string {
	if recorded == p.Spec.Image {
		return image.ID
	}
	running := p.Spec.ImageManifestDescriptor
	if running == nil || running.Platform == nil || recorded != running.Digest {
		return recorded
	}
	for _, manifest := range image.Manifests {
		if manifest.Kind == "image" && manifest.Available && manifest.ImageData != nil && manifest.ImageData.Platform == *running.Platform {
			return manifest.ID
		}
	}
	return recorded
}

// Platform is the platform of the image the container runs, such as
// linux/arm64/v8, so a non-native variant is pulled and run again; "" if
// Docker doesn't say.
func (p *Plan) Platform() string {
	platform := docker.Platform{}
	if running := p.Spec.ImageManifestDescriptor; running != nil && running.Platform != nil {
		platform = *running.Platform
	} else if p.previous != nil {
		platform = docker.Platform{OS: p.previous.OS, Architecture: p.previous.Architecture, Variant: p.previous.Variant}
	}
	if platform.OS == "" || platform.Architecture == "" {
		return ""
	}
	return strings.TrimSuffix(platform.OS+"/"+platform.Architecture+"/"+platform.Variant, "/")
}

// RestartPolicy is the container's restart policy as Docker reports it.
func (p *Plan) RestartPolicy() json.RawMessage { return p.Spec.HostConfig["RestartPolicy"] }

// mounts returns HostConfig.Binds and Mounts with every anonymous volume
// mounted by name at its destination, or nil for a list that doesn't change.
// A volume is anonymous when no bind or mount names its source: one the
// image declared, one from `docker run -v /path`, Compose's `- /path`, or an
// API client's destination-only bind.
func (p *Plan) mounts() (binds []string, mounts []map[string]any, err error) {
	json.Unmarshal(p.Spec.HostConfig["Binds"], &binds)
	if raw := p.Spec.HostConfig["Mounts"]; len(raw) > 0 && json.Unmarshal(raw, &mounts) != nil {
		return nil, nil, fmt.Errorf("invalid mounts")
	}
	named := map[string]bool{}
	for _, bind := range binds {
		if parts := strings.Split(bind, ":"); len(parts) >= 2 {
			named[path.Clean(parts[1])] = true
		}
	}
	for _, mount := range mounts {
		if source, _ := mount["Source"].(string); source != "" {
			target, _ := mount["Target"].(string)
			named[path.Clean(target)] = true
		}
	}
	bindsChanged, mountsChanged := false, false
	for _, mount := range p.Spec.Mounts {
		destination := path.Clean(mount.Destination)
		if mount.Type != "volume" || mount.Name == "" || named[destination] {
			continue
		}
		// A destination-only bind takes no options; Docker would refuse the
		// same destination in both lists, so the bind names the volume.
		if index := slices.IndexFunc(binds, func(bind string) bool { return path.Clean(bind) == destination }); index >= 0 {
			binds[index] = mount.Name + ":" + binds[index]
			bindsChanged = true
			continue
		}
		index := slices.IndexFunc(mounts, func(entry map[string]any) bool {
			target, _ := entry["Target"].(string)
			return path.Clean(target) == destination
		})
		// An explicit mount overrides one from VolumesFrom, so it is patched
		// even where another container also supplies the destination.
		if index >= 0 {
			mounts[index]["Source"] = mount.Name
			mountsChanged = true
			continue
		}
		if p.inherited[destination] {
			continue
		}
		mountsChanged = true
		mounts = append(mounts, map[string]any{"Type": "volume", "Source": mount.Name, "Target": mount.Destination, "ReadOnly": !mount.RW})
	}
	if !bindsChanged {
		binds = nil
	}
	if !mountsChanged {
		mounts = nil
	}
	return binds, mounts, nil
}

type endpoint struct {
	IPAMConfig *docker.EndpointIPAMConfig `json:"IPAMConfig,omitempty"`
	Links      []string                   `json:"Links,omitempty"`
	Aliases    []string                   `json:"Aliases,omitempty"`
	MacAddress string                     `json:"MacAddress,omitempty"`
	DriverOpts map[string]string          `json:"DriverOpts,omitempty"`
	GwPriority int                        `json:"GwPriority,omitempty"`
}

// endpoints keeps each network's static addresses, aliases, and options.
// Docker's own short-ID alias and MACs it derived from the old address are
// dropped, as Watchtower does.
func (p *Plan) endpoints() map[string]endpoint {
	result := map[string]endpoint{}
	shortID := p.Spec.ID[:min(12, len(p.Spec.ID))]
	for name, settings := range p.Spec.NetworkSettings.Networks {
		next := endpoint{Links: settings.Links, DriverOpts: settings.DriverOpts, GwPriority: settings.GwPriority, MacAddress: settings.MacAddress}
		if ipam := settings.IPAMConfig; ipam != nil && (ipam.IPv4Address != "" || ipam.IPv6Address != "" || len(ipam.LinkLocalIPs) > 0) {
			next.IPAMConfig = ipam
		}
		// The default bridge network has no aliases.
		if name != "bridge" {
			for _, alias := range settings.Aliases {
				if alias != shortID && alias != p.Spec.ID {
					next.Aliases = append(next.Aliases, alias)
				}
			}
		}
		if derivedMAC(settings.MacAddress, settings.IPAddress) {
			next.MacAddress = ""
		}
		result[name] = next
	}
	return result
}

// derivedMAC reports a MAC that older bridge drivers derived from the IPv4
// address: 02:42 followed by its four octets.
func derivedMAC(mac, address string) bool {
	ip := net.ParseIP(address).To4()
	if ip == nil {
		return false
	}
	return strings.EqualFold(mac, fmt.Sprintf("02:42:%02x:%02x:%02x:%02x", ip[0], ip[1], ip[2], ip[3]))
}
