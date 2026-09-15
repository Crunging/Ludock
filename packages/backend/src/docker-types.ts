/** The Docker Engine API fields used by Ludock and its disposable fixtures.
 * Keep daemon field names here; logical server contracts live in shared. */
export interface ContainerInfo {
  Id: string;
  Names: string[];
  Image: string;
  Labels: Record<string, string>;
  State: string;
  Status: string;
  Created: number;
}

export interface ContainerInspectInfo {
  Id: string;
  Image: string;
  Name: string;
  Created: string;
  Config: {
    Image: string;
    Labels: Record<string, string> | null;
    Env: string[] | null;
    OpenStdin: boolean;
    StdinOnce: boolean;
    Tty: boolean;
  };
  State: {
    Status: string;
    Running: boolean;
    Paused: boolean;
    Restarting: boolean;
    OOMKilled: boolean;
    ExitCode: number;
    StartedAt: string;
    Health?: { Status: string };
  };
  Mounts: Array<{
    Type: string;
    Source: string;
    Destination: string;
    RW: boolean;
    Name?: string;
  }>;
  NetworkSettings: {
    Ports: Record<string, Array<{ HostIp: string; HostPort: string }> | null>;
    Networks: Record<string, { IPAddress: string }>;
  };
}

export interface ContainerStats {
  cpu_stats: { cpu_usage: { total_usage: number }; system_cpu_usage: number; online_cpus?: number };
  precpu_stats: { cpu_usage: { total_usage: number }; system_cpu_usage: number };
  memory_stats: { usage: number; limit: number };
}

export interface MountSettings {
  Type: "bind" | "volume" | "tmpfs";
  Source: string;
  Target: string;
  ReadOnly?: boolean;
  BindOptions?: {
    Propagation?: "rprivate";
    NonRecursive?: boolean;
    ReadOnlyForceRecursive?: boolean;
  };
}

export interface HostConfig {
  Mounts?: MountSettings[];
  Binds?: string[];
  NetworkMode?: string;
  ReadonlyRootfs?: boolean;
  AutoRemove?: boolean;
  CapDrop?: string[];
  CapAdd?: string[];
  SecurityOpt?: string[];
  PidsLimit?: number;
  Memory?: number;
  NanoCpus?: number;
  Init?: boolean;
}

export interface ContainerCreateOptions {
  Image: string;
  name?: string;
  Entrypoint?: string[];
  Cmd?: string[];
  Env?: string[];
  User?: string;
  Labels?: Record<string, string>;
  HostConfig?: HostConfig;
  OpenStdin?: boolean;
  StdinOnce?: boolean;
  Tty?: boolean;
}

export interface ExecCreateOptions {
  Cmd?: string[];
  Env?: string[];
  User?: string;
  AttachStdin?: boolean;
  AttachStdout?: boolean;
  AttachStderr?: boolean;
  Tty?: boolean;
}

export interface ExecInspectInfo {
  Running: boolean;
  ExitCode: number | null;
}

export interface ContainerAttachOptions {
  stream?: boolean;
  stdin?: boolean;
  stdout?: boolean;
  stderr?: boolean;
}

export interface ContainerLogsOptions {
  follow?: boolean;
  stdout?: boolean;
  stderr?: boolean;
  tail?: number;
  timestamps?: boolean;
}

export interface VolumeInspectInfo {
  Name: string;
  Driver: string;
  Options: Record<string, string> | null;
  Mountpoint: string;
}

export type DockerFilters = Record<string, string[]>;
