package docker

import "encoding/json"

// Engine API fields Ludock uses. Field names follow the daemon.

// ContainerSummary is one /containers/json entry.
type ContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	Labels  map[string]string `json:"Labels"`
	State   *string           `json:"State"`
	Status  *string           `json:"Status"`
	Created int64             `json:"Created"`
}

type HealthState struct {
	Status string `json:"Status"`
}

type ContainerState struct {
	Status     string       `json:"Status"`
	Running    bool         `json:"Running"`
	Paused     bool         `json:"Paused"`
	Restarting bool         `json:"Restarting"`
	OOMKilled  bool         `json:"OOMKilled"`
	ExitCode   int          `json:"ExitCode"`
	StartedAt  string       `json:"StartedAt"`
	FinishedAt string       `json:"FinishedAt"`
	Health     *HealthState `json:"Health,omitempty"`
}

type ContainerConfig struct {
	Image       string            `json:"Image"`
	Labels      map[string]string `json:"Labels"`
	Env         []string          `json:"Env"`
	OpenStdin   bool              `json:"OpenStdin"`
	StdinOnce   bool              `json:"StdinOnce"`
	Tty         bool              `json:"Tty"`
	StopTimeout *int              `json:"StopTimeout,omitempty"`
}

type MountPoint struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
	Name        string `json:"Name,omitempty"`
}

type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type NetworkSettings struct {
	Ports    map[string][]PortBinding `json:"Ports"`
	Networks map[string]struct {
		IPAddress string `json:"IPAddress"`
	} `json:"Networks"`
}

// ContainerInspect is /containers/{id}/json.
type ContainerInspect struct {
	ID              string          `json:"Id"`
	Image           string          `json:"Image"`
	Name            string          `json:"Name"`
	Created         string          `json:"Created"`
	Config          ContainerConfig `json:"Config"`
	State           ContainerState  `json:"State"`
	Mounts          []MountPoint    `json:"Mounts"`
	NetworkSettings NetworkSettings `json:"NetworkSettings"`
}

// ContainerSpec is an inspection that keeps the configuration Docker reports
// verbatim, including fields Ludock never reads, so a container can be
// recreated without losing settings.
type ContainerSpec struct {
	ID              string                     `json:"Id"`
	Image           string                     `json:"Image"`
	Name            string                     `json:"Name"`
	Created         string                     `json:"Created"`
	State           ContainerState             `json:"State"`
	Config          map[string]json.RawMessage `json:"Config"`
	HostConfig      map[string]json.RawMessage `json:"HostConfig"`
	Mounts          []MountPoint               `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]EndpointSettings `json:"Networks"`
	} `json:"NetworkSettings"`
	// ImageManifestDescriptor is the platform manifest the container runs,
	// on engines with the containerd image store.
	ImageManifestDescriptor *ManifestDescriptor `json:"ImageManifestDescriptor,omitempty"`
}

type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// ManifestDescriptor identifies one platform manifest of an image.
type ManifestDescriptor struct {
	Digest   string    `json:"digest"`
	Platform *Platform `json:"platform,omitempty"`
}

// ImageManifest is one manifest of an image on the containerd image store.
type ImageManifest struct {
	ID        string `json:"ID"`
	Kind      string `json:"Kind"`
	Available bool   `json:"Available"`
	ImageData *struct {
		Platform Platform `json:"Platform"`
	} `json:"ImageData,omitempty"`
}

// EndpointIPAMConfig is a static address on one network.
type EndpointIPAMConfig struct {
	IPv4Address  string   `json:"IPv4Address,omitempty"`
	IPv6Address  string   `json:"IPv6Address,omitempty"`
	LinkLocalIPs []string `json:"LinkLocalIPs,omitempty"`
}

// EndpointSettings is a container's endpoint on one network: the fields a
// create request accepts, and the runtime address that explains its MAC.
type EndpointSettings struct {
	IPAMConfig *EndpointIPAMConfig `json:"IPAMConfig,omitempty"`
	Links      []string            `json:"Links,omitempty"`
	Aliases    []string            `json:"Aliases,omitempty"`
	MacAddress string              `json:"MacAddress,omitempty"`
	DriverOpts map[string]string   `json:"DriverOpts,omitempty"`
	GwPriority int                 `json:"GwPriority,omitempty"`
	IPAddress  string              `json:"IPAddress,omitempty"`
}

// ImageInspect is /images/{name}/json: the image ID, the defaults it gives
// containers, and its platform manifests where the engine lists them.
type ImageInspect struct {
	ID           string                     `json:"Id"`
	OS           string                     `json:"Os"`
	Architecture string                     `json:"Architecture"`
	Variant      string                     `json:"Variant"`
	Config       map[string]json.RawMessage `json:"Config"`
	Manifests    []ImageManifest            `json:"Manifests"`
}

type ContainerStats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage float64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage float64 `json:"system_cpu_usage"`
		OnlineCPUs     float64 `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage float64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage float64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage float64            `json:"usage"`
		Limit float64            `json:"limit"`
		Stats map[string]float64 `json:"stats,omitempty"`
	} `json:"memory_stats"`
}

type BindOptions struct {
	Propagation            string `json:"Propagation,omitempty"`
	NonRecursive           bool   `json:"NonRecursive,omitempty"`
	ReadOnlyForceRecursive bool   `json:"ReadOnlyForceRecursive,omitempty"`
}

type MountSettings struct {
	Type        string       `json:"Type"`
	Source      string       `json:"Source"`
	Target      string       `json:"Target"`
	ReadOnly    bool         `json:"ReadOnly,omitempty"`
	BindOptions *BindOptions `json:"BindOptions,omitempty"`
}

type HostConfig struct {
	Mounts         []MountSettings `json:"Mounts,omitempty"`
	Binds          []string        `json:"Binds,omitempty"`
	NetworkMode    string          `json:"NetworkMode,omitempty"`
	ReadonlyRootfs bool            `json:"ReadonlyRootfs,omitempty"`
	AutoRemove     bool            `json:"AutoRemove,omitempty"`
	CapDrop        []string        `json:"CapDrop,omitempty"`
	CapAdd         []string        `json:"CapAdd,omitempty"`
	SecurityOpt    []string        `json:"SecurityOpt,omitempty"`
	PidsLimit      int64           `json:"PidsLimit,omitempty"`
	Memory         int64           `json:"Memory,omitempty"`
	NanoCpus       int64           `json:"NanoCpus,omitempty"`
	Init           bool            `json:"Init,omitempty"`
}

type Healthcheck struct {
	Test []string `json:"Test"`
}

// ContainerCreate is the body of /containers/create; Name becomes a query
// parameter.
type ContainerCreate struct {
	Name        string            `json:"-"`
	Image       string            `json:"Image"`
	Healthcheck *Healthcheck      `json:"Healthcheck,omitempty"`
	Entrypoint  []string          `json:"Entrypoint,omitempty"`
	Cmd         []string          `json:"Cmd,omitempty"`
	Env         []string          `json:"Env,omitempty"`
	User        string            `json:"User,omitempty"`
	Labels      map[string]string `json:"Labels,omitempty"`
	HostConfig  *HostConfig       `json:"HostConfig,omitempty"`
	OpenStdin   bool              `json:"OpenStdin,omitempty"`
	StdinOnce   bool              `json:"StdinOnce,omitempty"`
	Tty         bool              `json:"Tty,omitempty"`
}

// ExecCreate is the body of /containers/{id}/exec.
type ExecCreate struct {
	Cmd          []string `json:"Cmd,omitempty"`
	Env          []string `json:"Env,omitempty"`
	User         string   `json:"User,omitempty"`
	AttachStdin  bool     `json:"AttachStdin,omitempty"`
	AttachStdout bool     `json:"AttachStdout,omitempty"`
	AttachStderr bool     `json:"AttachStderr,omitempty"`
	Tty          bool     `json:"Tty"`
}

// ExecInspect is /exec/{id}/json.
type ExecInspect struct {
	Running  bool `json:"Running"`
	ExitCode *int `json:"ExitCode"`
}

// VolumeInspect is /volumes/{name}.
type VolumeInspect struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Options    map[string]string `json:"Options"`
	Mountpoint string            `json:"Mountpoint"`
}

type Filters map[string][]string
