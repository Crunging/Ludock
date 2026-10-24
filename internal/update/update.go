// Package update pulls a newer image for a server and replaces its container
// with one that has the same settings, through the Docker Engine API alone.
// Whatever created the container (Compose, Portainer, Dockge, or the Docker
// CLI) keeps owning its definition; Ludock never reads or edits it.
package update

import (
	"context"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/servers"
)

// Service checks and plans image updates.
type Service struct {
	Docker *docker.Client
	// DockerConfig is a Docker client config folder with registry
	// credentials (LUDOCK_DOCKER_CONFIG), or "".
	DockerConfig string
}

var (
	errPinned = apperr.New("UNSUPPORTED_IMAGE", 409,
		"This server's image is pinned to a digest or image ID, so there is no newer version to pull. Change the image through the tool that created the container.")
	errAutoRemove = apperr.New("UNSUPPORTED_CONTAINER", 409,
		"Docker removes this container when it stops (--rm), so Ludock can't replace it. Recreate it through the tool that created it.")
	errSwarm = apperr.New("UNSUPPORTED_CONTAINER", 409,
		"Docker Swarm manages this container. Update its service through Swarm.")
	errDependents = apperr.New("SERVICE_DEPENDENCY", 409,
		"Another container shares this server's network, process, or IPC namespace and would lose it. Update them together through the tool that created them.")
	errVolumeUnknown = apperr.New("VOLUME_UNKNOWN", 409,
		"Docker doesn't say which volume one of this server's mounts uses, so Ludock can't keep its data. Recreate it through the tool that created it.")
	errImageGone = apperr.New("IMAGE_UNAVAILABLE", 409,
		"Docker no longer has the image this server's container was created from, so Ludock can't tell its settings from the image's defaults. Recreate it once through the tool that created it.")
)

// Plan is a container checked for replacement.
type Plan struct {
	Spec *docker.ContainerSpec
	// Image is the configured image reference that is pulled.
	Image string
	// previous holds the defaults the current image gave the container.
	previous *docker.ImageInspect
	// inherited are destinations mounted from VolumesFrom containers.
	inherited map[string]bool
}

var imageID = regexp.MustCompile(`^(sha256:)?[0-9a-f]{12,64}$`)

// Check reads the server's container from Docker and confirms that Ludock
// can replace it.
func (s *Service) Check(ctx context.Context, server *servers.Context) (*Plan, error) {
	return s.check(ctx, server, nil)
}

// Recheck checks the server again, as Check does, keeping what earlier read
// that Docker may since have dropped: the old image's defaults, which a pull
// can remove, and each network's MAC and address, which a stop clears.
func (s *Service) Recheck(ctx context.Context, server *servers.Context, earlier *Plan) (*Plan, error) {
	plan, err := s.check(ctx, server, earlier)
	if err != nil {
		return nil, err
	}
	if plan.Spec.ID == earlier.Spec.ID {
		for name, endpoint := range plan.Spec.NetworkSettings.Networks {
			if before, ok := earlier.Spec.NetworkSettings.Networks[name]; ok && endpoint.MacAddress == "" {
				endpoint.MacAddress, endpoint.IPAddress = before.MacAddress, before.IPAddress
				plan.Spec.NetworkSettings.Networks[name] = endpoint
			}
		}
	}
	return plan, nil
}

func (s *Service) check(ctx context.Context, server *servers.Context, earlier *Plan) (*Plan, error) {
	spec, err := s.Docker.InspectSpec(ctx, server.Container.ID)
	if err != nil {
		return nil, err
	}
	var config struct {
		Image  string
		Labels map[string]string
	}
	var host struct {
		AutoRemove  bool
		VolumesFrom []string
	}
	if decodeInto(spec.Config, &config) != nil || decodeInto(spec.HostConfig, &host) != nil || spec.ID != server.Container.ID {
		return nil, apperr.New("INVALID_CONTAINER", 409, "Docker returned an unreadable container configuration")
	}
	if config.Image == "" || strings.Contains(config.Image, "@") || imageID.MatchString(config.Image) {
		return nil, errPinned
	}
	if host.AutoRemove {
		return nil, errAutoRemove
	}
	if _, swarm := config.Labels["com.docker.swarm.service.id"]; swarm {
		return nil, errSwarm
	}
	if err := s.assertNoDependents(ctx, spec); err != nil {
		return nil, err
	}
	if err := s.nameVolumes(ctx, spec); err != nil {
		return nil, err
	}
	inherited := map[string]bool{}
	for _, entry := range host.VolumesFrom {
		source, _, _ := strings.Cut(entry, ":")
		from, err := s.Docker.InspectSpec(ctx, source)
		if err != nil {
			return nil, err
		}
		for _, mount := range from.Mounts {
			inherited[path.Clean(mount.Destination)] = true
		}
	}
	if earlier != nil && earlier.Spec.Image == spec.Image {
		return &Plan{Spec: spec, Image: config.Image, previous: earlier.previous, inherited: inherited}, nil
	}
	var previous *docker.ImageInspect
	if running := spec.ImageManifestDescriptor; running != nil && running.Platform != nil {
		previous, err = s.Docker.InspectImageVariant(ctx, spec.Image, *running.Platform)
	} else {
		previous, err = s.Docker.InspectImageConfig(ctx, spec.Image)
	}
	if docker.IsNotFound(err) {
		return nil, errImageGone
	}
	if err != nil {
		return nil, err
	}
	return &Plan{Spec: spec, Image: config.Image, previous: previous, inherited: inherited}, nil
}

// assertNoDependents refuses when another container joined one of this
// container's namespaces: replacing it would cut that container off.
func (s *Service) assertNoDependents(ctx context.Context, spec *docker.ContainerSpec) error {
	containers, err := s.Docker.ListContainers(ctx, true, nil)
	if err != nil {
		return err
	}
	name := strings.TrimPrefix(spec.Name, "/")
	for _, candidate := range containers {
		if candidate.ID == spec.ID {
			continue
		}
		other, err := s.Docker.InspectSpec(ctx, candidate.ID)
		if docker.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		var modes map[string]any
		if decodeInto(other.HostConfig, &modes) != nil {
			return apperr.New("INVALID_CONTAINER", 409, "Docker returned an unreadable container configuration")
		}
		for _, key := range []string{"NetworkMode", "PidMode", "IpcMode"} {
			mode, _ := modes[key].(string)
			target, joined := strings.CutPrefix(mode, "container:")
			if joined && (target == name || (len(target) >= 12 && strings.HasPrefix(spec.ID, target))) {
				return errDependents
			}
		}
	}
	return nil
}

// nameVolumes fills in the names Docker leaves out of some volume mounts,
// such as the anonymous volume of a destination-only bind, from the volume
// mounted at the same place. A volume Ludock can't name could not be
// reused, so the update is refused instead.
func (s *Service) nameVolumes(ctx context.Context, spec *docker.ContainerSpec) error {
	var volumes []docker.VolumeInspect
	for index, mount := range spec.Mounts {
		if mount.Type != "volume" || mount.Name != "" {
			continue
		}
		if volumes == nil {
			var err error
			if volumes, err = s.Docker.ListVolumes(ctx); err != nil {
				return err
			}
		}
		found := slices.IndexFunc(volumes, func(volume docker.VolumeInspect) bool {
			return volume.Mountpoint != "" && volume.Mountpoint == mount.Source
		})
		if found < 0 {
			return errVolumeUnknown
		}
		spec.Mounts[index].Name = volumes[found].Name
	}
	return nil
}

// RegistryAuth is the credential header for pulling the plan's image.
func (s *Service) RegistryAuth(plan *Plan) (string, error) {
	return registryAuth(s.DockerConfig, plan.Image)
}

// Capability describes whether a server can be updated here.
type Capability struct {
	UpdatesOnStart    bool
	Available         bool
	ActionLabel       string
	Image             string
	UnavailableReason string
}

// Capability reports update availability. busy reports active work.
func (s *Service) Capability(ctx context.Context, server *servers.Context, admin bool, busy func() bool) Capability {
	capability := Capability{ActionLabel: "Update server", Image: server.Container.Image}
	if server.Container.GameType == "unknown" {
		capability.ActionLabel = "Update image"
	}
	if integration := games.ImageIntegration(server.Container.Image); integration != nil {
		capability.UpdatesOnStart = integration.UpdatesOnStart
	}
	fail := func(err error) Capability {
		if public, ok := apperr.As(err); ok {
			capability.UnavailableReason = public.Message
		} else {
			capability.UnavailableReason = "Ludock could not read this server's container from Docker. Check the Docker connection and try again."
		}
		return capability
	}
	if !admin {
		return fail(apperr.New("FORBIDDEN", 403, "Administrator permission is required to pull a newer image and recreate the server."))
	}
	if busy() {
		return fail(apperr.New("OPERATION_CONFLICT", 409, "Wait for the active operation to finish before updating"))
	}
	if _, err := s.Check(ctx, server); err != nil {
		return fail(err)
	}
	capability.Available = true
	return capability
}
