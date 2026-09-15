import Docker from "dockerode";
import { composeSourceLabels } from "./compose-source.js";
import { docker } from "./docker-client.js";
import {
  getGameConsoleAdapterSummary,
  resolveGameConsoleAdapter,
  type GameConsoleAdapterId,
} from "./game-console.js";
import { getFileRoots, type FileRoot } from "./file-storage.js";
import {
  getGameCapabilities,
  inferGameType,
  type GameCapabilities,
} from "./server-presets.js";
import {
  approvedConfigurationLabels,
  composeIdentityLabels,
  evaluateContainerEligibility,
  hasInvalidComposeIdentity,
  LABEL_ENABLE,
  LABEL_NAME,
  LABEL_GAME,
} from "./discovery.js";
import type { ServerObservation } from "./identity.js";
import {
  dockerContainerIdSchema,
  type DockerContainerId,
} from "@ludock/shared";

export { LABEL_ENABLE, LABEL_NAME, LABEL_GAME } from "./discovery.js";

// Keep untrusted identifiers from altering dockerode's Docker API request path.
function assertValidContainerId(id: unknown): DockerContainerId {
  const parsed = dockerContainerIdSchema.safeParse(id);
  if (!parsed.success) {
    const error = new Error("Invalid container identifier");
    Object.assign(error, { statusCode: 400, code: "INVALID_CONTAINER_ID" });
    throw error;
  }
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
  gameType: string;
  capabilities?: GameCapabilities;
  gameConsole: {
    id: GameConsoleAdapterId;
    name: string;
    commandPlaceholder: string;
  } | null;
  fileRoots: FileRoot[];
  ports: Array<{ private: number; public: number; type: string }>;
  created: number;
  labels: Record<string, string>;
}

export interface DiscoveryDiagnostic {
  containerId: string;
  name: string;
  code: "INVALID_ENABLE_LABEL" | "INVALID_COMPOSE_IDENTITY";
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
    const invalidLabel = eligibility.reason === "invalid-enable-label";
    const invalidIdentity =
      eligibility.eligible && hasInvalidComposeIdentity(container.Labels || {});
    if (!invalidLabel && !invalidIdentity) return [];
    return [
      {
        containerId: container.Id,
        name: (container.Names[0] || "").replace(/^\//, ""),
        code: invalidLabel
          ? "INVALID_ENABLE_LABEL"
          : "INVALID_COMPOSE_IDENTITY",
        message: invalidLabel
          ? "Container excluded: ludock.enable must be true or false (case-insensitive; surrounding spaces are allowed)."
          : "Container excluded: Compose project, service, and replica number must form a complete identity. Correct the owning manager's metadata before managing this container.",
      },
    ];
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

function toInspectedManagedContainer(
  info: Docker.ContainerInspectInfo,
): ManagedContainer {
  const labels = info.Config.Labels || {};
  const managed = {
    id: assertValidContainerId(info.Id),
    shortId: info.Id.substring(0, 12),
    name: info.Name.replace(/^\//, ""),
    displayName: labels[LABEL_NAME] || info.Name.replace(/^\//, ""),
    image: info.Config.Image,
    state: info.State.Status,
    status: `${info.State.Status}${info.State.Health ? ` (${info.State.Health.Status})` : ""}`,
    gameType: labels[LABEL_GAME]?.trim() || inferGameType(info.Config.Image),
    ports: Object.entries(info.NetworkSettings.Ports || {}).flatMap(
      ([containerPort, bindings]) => {
        if (!bindings) return [];
        const [port, type] = containerPort.split("/");
        return bindings.map((binding) => ({
          private: parseInt(port, 10),
          public: parseInt(binding.HostPort, 10),
          type: type || "tcp",
        }));
      },
    ),
    created: new Date(info.Created).getTime(),
    labels: approvedConfigurationLabels(labels),
  };
  return {
    ...managed,
    capabilities: capabilitiesForContainer(managed),
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
      ([key]) => key !== LABEL_ENABLE && key !== LABEL_NAME,
    ),
  );
  for (const entry of info.Config.Env || []) {
    const split = entry.indexOf("=");
    if (split === -1) continue;
    const key = entry.slice(0, split);
    if (
      credentialNames.has(key) ||
      /password|passwd|secret|token|credential|rconpw/i.test(key) ||
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

export async function startContainer(
  id: DockerContainerId,
  assertAccess?: (observation: ServerObservation) => void,
): Promise<void> {
  const { container, info } = await getManagedDockerContainer(id);
  assertAccess?.(toServerObservation(info, toInspectedManagedContainer(info)));
  await container.start();
}

export async function stopContainer(
  id: DockerContainerId,
  assertAccess?: (observation: ServerObservation) => void,
): Promise<void> {
  const { container, info } = await getManagedDockerContainer(id);
  assertAccess?.(toServerObservation(info, toInspectedManagedContainer(info)));
  await container.stop();
}

export async function restartContainer(
  id: DockerContainerId,
  assertAccess?: (observation: ServerObservation) => void,
): Promise<void> {
  const { container, info } = await getManagedDockerContainer(id);
  assertAccess?.(toServerObservation(info, toInspectedManagedContainer(info)));
  await container.restart();
}

export function getDockerInstance(): Docker {
  return docker;
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
  if (
    !evaluateContainerEligibility(
      info.Config.Image || "",
      info.Config.Labels || {},
    ).eligible
  ) {
    const error = new Error("Container is not managed by Ludock");
    Object.assign(error, { statusCode: 403, code: "FORBIDDEN" });
    throw error;
  }
  if (hasInvalidComposeIdentity(info.Config.Labels || {})) {
    const error = new Error(
      "Container Compose identity is incomplete; administrator review required",
    );
    Object.assign(error, { statusCode: 409, code: "INVALID_COMPOSE_IDENTITY" });
    throw error;
  }
}

function capabilitiesForContainer(
  server: Pick<ManagedContainer, "gameType" | "image" | "labels">,
): GameCapabilities {
  const capabilities = getGameCapabilities(server.gameType);
  // An explicit game override does not turn an unknown image into a recognized repository.
  capabilities.recognition = getGameCapabilities(
    inferGameType(server.image),
  ).recognition;
  const configured = server.labels["ludock.console"]?.trim().toLowerCase();
  if (configured) {
    const adapter = resolveGameConsoleAdapter(server);
    capabilities.console = {
      status: adapter ? "conditional" : "unsupported",
      description: adapter
        ? "An explicit console adapter is configured. Its protocol, address, port, and credentials must be validated independently for this image."
        : "Console access is disabled for this container.",
      evidence: ["test/game-console.test.ts"],
    };
  }
  return capabilities;
}
