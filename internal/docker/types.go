package docker

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

// HealthState is a container health result.
type HealthState struct {
	Status string `json:"Status"`
}

// ContainerState is the state section of an inspection.
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

// ContainerConfig is the config section of an inspection.
type ContainerConfig struct {
	Image       string            `json:"Image"`
	Labels      map[string]string `json:"Labels"`
	Env         []string          `json:"Env"`
	OpenStdin   bool              `json:"OpenStdin"`
	StdinOnce   bool              `json:"StdinOnce"`
	Tty         bool              `json:"Tty"`
	StopTimeout *int              `json:"StopTimeout,omitempty"`
}

// MountPoint is one mount of an inspected container.
type MountPoint struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
	Name        string `json:"Name,omitempty"`
}

// PortBinding is a published port.
type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// NetworkSettings is the network section of an inspection.
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

// ContainerStats is a one-shot stats sample.
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
		Usage float64 `json:"usage"`
		Limit float64 `json:"limit"`
	} `json:"memory_stats"`
}

// BindOptions configures a bind mount.
type BindOptions struct {
	Propagation            string `json:"Propagation,omitempty"`
	NonRecursive           bool   `json:"NonRecursive,omitempty"`
	ReadOnlyForceRecursive bool   `json:"ReadOnlyForceRecursive,omitempty"`
}

// MountSettings is a mount for a created container.
type MountSettings struct {
	Type        string       `json:"Type"`
	Source      string       `json:"Source"`
	Target      string       `json:"Target"`
	ReadOnly    bool         `json:"ReadOnly,omitempty"`
	BindOptions *BindOptions `json:"BindOptions,omitempty"`
}

// HostConfig is the host configuration of a created container.
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

// Healthcheck overrides an image healthcheck.
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

// Filters is a Docker filter set.
type Filters map[string][]string
