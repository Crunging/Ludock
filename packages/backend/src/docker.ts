import type * as Docker from "./docker-client.js";
import { composeSourceLabels } from "./compose-source.js";
import { docker } from "./docker-client.js";
import { DockerApiError } from "./docker-transport.js";
import { AppError } from "./errors.js";
import {
  getGameConsoleAdapterSummary,
  resolveGameConsoleAdapter,
  type GameConsoleAdapterId,
  type QuickCommand,
} from "./game-console.js";
import type { FileRoot } from "@ludock/shared";
import { getFileRoots } from "./file-storage.js";
import { gameDisplayName, getGameIntegration, inferGameType } from "./server-presets.js";
import {
  approvedConfigurationLabels,
  composeIdentityLabels,
  evaluateContainerEligibility,
  hasInvalidComposeIdentity,
  LABEL_ADDRESS,
  LABEL_ENABLE,
  LABEL_NAME,
  LABEL_GAME,
  parseAddressLabel,
} from "./discovery.js";
import type { ServerObservation } from "./identity.js";
import { isSensitiveKey } from "./sensitive-keys.js";
import {
  dockerContainerIdSchema,
  type ContainerHealth,
  type DockerContainerId,
} from "@ludock/shared";

// Keep untrusted identifiers from altering the Docker API request path.
function assertValidContainerId(id: unknown): DockerContainerId {
  const parsed = dockerContainerIdSchema.safeParse(id);
  if (!parsed.success)
    throw new AppError("INVALID_CONTAINER_ID", 400, "Invalid container identifier");
  return parsed.data;
}

export interface ManagedContainer {
  id: DockerContainerId;
  shortId: string;
  name: string;
  displayName: string;
  image: string;
  state: string;
  status: string;
  health: ContainerHealth | null;
  stateSince: number | null;
  exit: { code: number; oomKilled: boolean } | null;
  gameType: string;
  gameName: string;
  /** The owner's ludock.address, which replaces port detection when valid. */
  addressLabel: { host: string; port: number | null } | null;
  /** Published host port for the game's own port, or the first published port. */
  connectPort: number | null;
  gameConsole: {
    id: GameConsoleAdapterId;
    name: string;
    commandPlaceholder: string;
    commands: QuickCommand[];
  } | null;
  fileRoots: FileRoot[];
  ports: Array<{ private: number; public: number; type: string }>;
  created: number;
  labels: Record<string, string>;
}

interface DiscoveryDiagnostic {
  containerId: string;
  name: string;
  code: "INVALID_ENABLE_LABEL" | "INVALID_COMPOSE_IDENTITY" | "INVALID_ADDRESS_LABEL";
  message: string;
}

/** Administrator-only diagnostics. Never include the untrusted label value. */
export async function getDiscoveryDiagnostics(): Promise<
  DiscoveryDiagnostic[]
> {
  const containers = await docker.listContainers({ all: true });
  return containers.flatMap((container): DiscoveryDiagnostic[] => {
    const eligibility = evaluateContainerEligibility(
      container.Image,
      container.Labels || {},
    );
    const labels = container.Labels || {};
    const invalidLabel = eligibility.reason === "invalid-enable-label";
    const invalidIdentity =
      eligibility.eligible && hasInvalidComposeIdentity(labels);
    const invalidAddress = eligibility.eligible && !invalidIdentity &&
      Object.hasOwn(labels, LABEL_ADDRESS) && parseAddressLabel(labels[LABEL_ADDRESS]) === null;
    const diagnostic = (code: DiscoveryDiagnostic["code"], message: string): DiscoveryDiagnostic => ({
      containerId: container.Id,
      name: (container.Names[0] || "").replace(/^\//, ""),
      code,
      message,
    });
    if (invalidLabel)
      return [diagnostic("INVALID_ENABLE_LABEL",
        "Container excluded: ludock.enable must be true or false (case-insensitive; surrounding spaces are allowed).")];
    if (invalidIdentity)
      return [diagnostic("INVALID_COMPOSE_IDENTITY",
        "Container excluded: Compose project, service, and replica number must form a complete identity. Correct the owning manager's metadata before managing this container.")];
    if (invalidAddress)
      return [diagnostic("INVALID_ADDRESS_LABEL",
        "ludock.address is ignored: use a host name or IP address with an optional port, such as play.example.com:25565 or [2001:db8::1]:2456. The detected address is shown instead.")];
    return [];
  });
}

export interface ManagedContainerObservation {
  container: ManagedContainer;
  observation: ServerObservation;
}

export async function listManagedContainerObservations(): Promise<
  ManagedContainerObservation[]
> {
  const containers = await docker.listContainers({ all: true });
  const eligible = containers.filter(
    (container) =>
      evaluateContainerEligibility(container.Image, container.Labels || {})
        .eligible,
  );
  const observations = new Array<ManagedContainerObservation | undefined>(
    eligible.length,
  );
  let nextIndex = 0;
  let failure: { error: unknown } | undefined;

  // Bound daemon load while inspecting fresh data for every fingerprint. Keep
  // list order even when inspections finish out of order.
  async function inspectNext(): Promise<void> {
    while (!failure && nextIndex < eligible.length) {
      const index = nextIndex++;
      try {
        observations[index] = await getManagedContainerObservation(
          assertValidContainerId(eligible[index].Id),
        );
      } catch (error) {
        const code = (error as { statusCode?: number })?.statusCode;
        if (
          code === 404 ||
          code === 403 ||
          (error as { code?: string })?.code === "INVALID_COMPOSE_IDENTITY"
        )
          continue;
        failure ??= { error };
      }
    }
  }

  // Drain launched reads before rejecting, so the caller retains refresh
  // ownership until all work from this attempt has finished.
  await Promise.all(
    Array.from({ length: Math.min(4, eligible.length) }, () => inspectNext()),
  );
  if (failure) throw failure.error;
  return observations.filter((observation) => observation !== undefined);
}

export async function getManagedContainerObservation(
  id: DockerContainerId,
): Promise<ManagedContainerObservation> {
  const container = docker.getContainer(assertValidContainerId(id));
  const info = await container.inspect();
  assertEligible(info);
  const managed = toInspectedManagedContainer(info);
  return {
    container: managed,
    observation: toServerObservation(info, managed),
  };
}

/** Docker keeps the last check result after a container stops; only a running one is current. */
function containerHealth(info: Docker.ContainerInspectInfo): ContainerHealth | null {
  if (info.State.Status !== "running") return null;
  const status = info.State.Health?.Status;
  return status === "starting" || status === "healthy" || status === "unhealthy"
    ? status
    : null;
}

/** Docker reports never-set times as year 0001. */
function dockerTime(value: string | undefined): number | null {
  const time = value ? Date.parse(value) : Number.NaN;
  return Number.isFinite(time) && time > 0 ? time : null;
}

interface PortBinding {
  private: number;
  public: number;
  type: string;
  hostIp: string;
}

/** Docker limits a loopback-bound mapping to the Docker host itself. */
function loopbackOnly(binding: PortBinding): boolean {
  return /^127\./.test(binding.hostIp) || binding.hostIp === "::1";
}

/** Picks the host port players use. When a game's own port is published, only a
 * reachable mapping with the game's protocol counts: TCP and UDP mappings are
 * independent, and a loopback-only mapping is unreachable for players. Without
 * one, no address is offered rather than another protocol or port such as RCON.
 * Other images, and games whose port is not published, use their first
 * reachable port. */
function connectPort(
  bindings: PortBinding[],
  gamePort: { port: number; protocol: string } | undefined,
): number | null {
  const reachable = bindings.filter((binding) => binding.public > 0 && !loopbackOnly(binding));
  if (gamePort && bindings.some((binding) => binding.public > 0 && binding.private === gamePort.port)) {
    return reachable.find((binding) =>
      binding.private === gamePort.port && binding.type === gamePort.protocol)?.public ?? null;
  }
  return reachable[0]?.public ?? null;
}

function toInspectedManagedContainer(
  info: Docker.ContainerInspectInfo,
): ManagedContainer {
  const labels = info.Config.Labels || {};
  const gameType = labels[LABEL_GAME]?.trim() || inferGameType(info.Config.Image);
  const bindings = Object.entries(info.NetworkSettings.Ports || {}).flatMap(
    ([containerPort, published]): PortBinding[] => {
      if (!published) return [];
      const [port, type] = containerPort.split("/");
      return published.map((binding) => ({
        private: parseInt(port, 10),
        public: parseInt(binding.HostPort, 10),
        type: type || "tcp",
        hostIp: binding.HostIp || "",
      }));
    },
  );
  const managed = {
    id: assertValidContainerId(info.Id),
    shortId: info.Id.substring(0, 12),
    name: info.Name.replace(/^\//, ""),
    displayName: labels[LABEL_NAME] || info.Name.replace(/^\//, ""),
    image: info.Config.Image,
    state: info.State.Status,
    status: `${info.State.Status}${info.State.Health ? ` (${info.State.Health.Status})` : ""}`,
    health: containerHealth(info),
    stateSince: info.State.Status === "running"
      ? dockerTime(info.State.StartedAt)
      : info.State.Status === "exited" || info.State.Status === "dead"
        ? dockerTime(info.State.FinishedAt)
        : null,
    exit: (info.State.Status === "exited" || info.State.Status === "dead") &&
      Number.isInteger(info.State.ExitCode)
      ? { code: info.State.ExitCode, oomKilled: info.State.OOMKilled === true }
      : null,
    gameType,
    gameName: gameDisplayName(gameType),
    // Host addresses stay internal; the public contract lists ports only.
    ports: bindings.map((binding) => ({
      private: binding.private,
      public: binding.public,
      type: binding.type,
    })),
    created: new Date(info.Created).getTime(),
    labels: approvedConfigurationLabels(labels),
  };
  return {
    ...managed,
    addressLabel: parseAddressLabel(labels[LABEL_ADDRESS]),
    connectPort: connectPort(bindings, getGameIntegration(gameType)?.gamePort),
    gameConsole: getGameConsoleAdapterSummary(managed),
    fileRoots: getFileRoots(managed, info.Mounts || []),
  };
}

/** Internal only: raw mounts/configuration are hashed by identity.ts, never serialized as server DTOs. */
function toServerObservation(
  info: Docker.ContainerInspectInfo,
  server: ManagedContainer,
): ServerObservation {
  const labels = info.Config.Labels || {};
  const compose = composeIdentityLabels(labels);
  const adapter = resolveGameConsoleAdapter(server);
  const credentialNames = new Set(adapter?.passwordEnvCandidates || []);
  if (labels["ludock.console.password-env"])
    credentialNames.add(labels["ludock.console.password-env"]);
  const gameConfiguration = Object.fromEntries(
    Object.entries(server.labels).filter(
      // Presentation labels can change without a review of the server's identity.
      ([key]) => key !== LABEL_ENABLE && key !== LABEL_NAME && key !== LABEL_ADDRESS,
    ),
  );
  for (const entry of info.Config.Env || []) {
    const split = entry.indexOf("=");
    if (split === -1) continue;
    const key = entry.slice(0, split);
    if (
      credentialNames.has(key) ||
      isSensitiveKey(key) ||
      key === "RCON_PORT" ||
      key === "ENABLE_RCON"
    )
      gameConfiguration[`env:${key}`] = entry.slice(split + 1);
  }
  if (adapter?.id === "stdin-console") {
    gameConfiguration["stdin:open"] = String(info.Config.OpenStdin);
    gameConfiguration["stdin:once"] = String(info.Config.StdinOnce);
  }
  return {
    containerId: assertValidContainerId(info.Id),
    name: server.name,
    displayName: server.displayName,
    gameType: server.gameType,
    ...(compose ? { compose, composeSourceLabels: composeSourceLabels(labels) } : {}),
    mounts: (info.Mounts || []).map((mount) => ({
      type: mount.Type,
      source: mount.Source,
      destination: mount.Destination,
      writable: mount.RW,
      ...(mount.Name ? { name: mount.Name } : {}),
    })),
    gameConfiguration,
  };
}

export async function getContainerStats(
  id: DockerContainerId,
  assertAccess: (observation: ServerObservation) => void,
): Promise<{ cpuPercent: number; memUsageMB: number; memLimitMB: number }> {
  const { container, info } = await getManagedDockerContainer(id);
  assertAccess(toServerObservation(info, toInspectedManagedContainer(info)));
  const stats = await container.stats({ stream: false });

  const cpuDelta =
    stats.cpu_stats.cpu_usage.total_usage -
    stats.precpu_stats.cpu_usage.total_usage;
  const systemDelta =
    stats.cpu_stats.system_cpu_usage - stats.precpu_stats.system_cpu_usage;
  const numCpus = stats.cpu_stats.online_cpus || 1;
  const cpuPercent =
    systemDelta > 0 ? (cpuDelta / systemDelta) * numCpus * 100 : 0;

  const memUsageMB = stats.memory_stats.usage / (1024 * 1024);
  const memLimitMB = stats.memory_stats.limit / (1024 * 1024);

  return {
    cpuPercent: Math.round(cpuPercent * 100) / 100,
    memUsageMB: Math.round(memUsageMB * 100) / 100,
    memLimitMB: Math.round(memLimitMB * 100) / 100,
  };
}

export type ContainerAction = "start" | "stop" | "restart";

/** Inspect and authorize the current container immediately before acting on it.
 * Docker's 304 means start/stop was already satisfied, which is not a failure. */
export async function changeContainerState(
  id: DockerContainerId,
  action: ContainerAction,
  assertAccess?: (observation: ServerObservation) => void,
): Promise<void> {
  const { container, info } = await getManagedDockerContainer(id);
  assertAccess?.(toServerObservation(info, toInspectedManagedContainer(info)));
  await container[action]().catch((error: unknown) => {
    if (action === "restart" || !(error instanceof DockerApiError) || error.statusCode !== 304)
      throw error;
  });
}

export async function checkDockerConnection(): Promise<void> {
  await docker.ping();
}

export function getContainer(id: DockerContainerId): Docker.Container {
  return docker.getContainer(assertValidContainerId(id));
}

async function getManagedDockerContainer(
  id: DockerContainerId,
): Promise<{ container: Docker.Container; info: Docker.ContainerInspectInfo }> {
  const container = docker.getContainer(assertValidContainerId(id));
  const info = await container.inspect();
  assertEligible(info);

  return { container, info };
}

function assertEligible(info: Docker.ContainerInspectInfo): void {
  if (!evaluateContainerEligibility(info.Config.Image || "", info.Config.Labels || {}).eligible)
    throw new AppError("FORBIDDEN", 403, "Container is not managed by Ludock");
  if (hasInvalidComposeIdentity(info.Config.Labels || {}))
    throw new AppError(
      "INVALID_COMPOSE_IDENTITY", 409,
      "Container Compose identity is incomplete; administrator review required",
    );
}
