import Docker from "dockerode";
import { docker } from "./docker-client.js";
import {
  getGameConsoleAdapterSummary,
  type GameConsoleAdapterId,
} from "./game-console.js";
import { getFileRoots, type FileRoot } from "./file-storage.js";

export const LABEL_PREFIX = "game-panel";
export const LABEL_ENABLE = `${LABEL_PREFIX}.enable`;
export const LABEL_NAME = `${LABEL_PREFIX}.name`;
export const LABEL_GAME = `${LABEL_PREFIX}.game`;

export interface ManagedContainer {
  id: string;
  shortId: string;
  name: string;
  displayName: string;
  image: string;
  state: string;
  status: string;
  gameType: string;
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

export async function listManagedContainers(): Promise<ManagedContainer[]> {
  const containers = await docker.listContainers({
    all: true,
    filters: {
      label: [`${LABEL_ENABLE}=true`],
    },
  });

  return containers.map(toManagedContainer);
}

export async function getManagedContainer(
  id: string
): Promise<ManagedContainer> {
  const container = docker.getContainer(id);
  const info = await container.inspect();

  const labels = info.Config.Labels || {};
  if (labels[LABEL_ENABLE] !== "true") {
    throw new Error(`Container ${id} is not managed by game-panel`);
  }

  const managed = {
    id: info.Id,
    shortId: info.Id.substring(0, 12),
    name: info.Name.replace(/^\//, ""),
    displayName:
      labels[LABEL_NAME] || info.Name.replace(/^\//, ""),
    image: info.Config.Image,
    state: info.State.Status,
    status: `${info.State.Status}${info.State.Health ? ` (${info.State.Health.Status})` : ""}`,
    gameType: labels[LABEL_GAME] || "unknown",
    ports: Object.entries(info.NetworkSettings.Ports || {}).flatMap(
      ([containerPort, bindings]) => {
        if (!bindings) return [];
        const [port, type] = containerPort.split("/");
        return bindings.map((b) => ({
          private: parseInt(port, 10),
          public: parseInt(b.HostPort, 10),
          type: type || "tcp",
        }));
      }
    ),
    created: new Date(info.Created).getTime(),
    labels: panelLabels(labels),
  };
  return {
    ...managed,
    gameConsole: getGameConsoleAdapterSummary(managed),
    fileRoots: getFileRoots(managed),
  };
}

export async function getContainerStats(
  id: string
): Promise<{ cpuPercent: number; memUsageMB: number; memLimitMB: number }> {
  const container = await getManagedDockerContainer(id);
  const stats = (await container.stats({ stream: false })) as Docker.ContainerStats;

  const cpuDelta =
    stats.cpu_stats.cpu_usage.total_usage -
    stats.precpu_stats.cpu_usage.total_usage;
  const systemDelta =
    stats.cpu_stats.system_cpu_usage! - stats.precpu_stats.system_cpu_usage!;
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

export async function startContainer(id: string): Promise<void> {
  const container = await getManagedDockerContainer(id);
  await container.start();
}

export async function stopContainer(id: string): Promise<void> {
  const container = await getManagedDockerContainer(id);
  await container.stop();
}

export async function restartContainer(id: string): Promise<void> {
  const container = await getManagedDockerContainer(id);
  await container.restart();
}

export function getDockerInstance(): Docker {
  return docker;
}

export async function checkDockerConnection(): Promise<void> {
  await docker.ping();
}

export function getContainer(id: string): Docker.Container {
  return docker.getContainer(id);
}

async function getManagedDockerContainer(id: string): Promise<Docker.Container> {
  const container = docker.getContainer(id);
  const info = await container.inspect();
  const labels = info.Config.Labels || {};

  if (labels[LABEL_ENABLE] !== "true") {
    const error = new Error(`Container ${id} is not managed by game-panel`);
    Object.assign(error, { statusCode: 403, code: "FORBIDDEN" });
    throw error;
  }

  return container;
}

function toManagedContainer(
  container: Docker.ContainerInfo
): ManagedContainer {
  const labels = container.Labels || {};
  const name = (container.Names[0] || "").replace(/^\//, "");

  const managed = {
    id: container.Id,
    shortId: container.Id.substring(0, 12),
    name,
    displayName: labels[LABEL_NAME] || name,
    image: container.Image,
    state: container.State,
    status: container.Status,
    gameType: labels[LABEL_GAME] || "unknown",
    ports: (container.Ports || []).map((p) => ({
      private: p.PrivatePort,
      public: p.PublicPort || 0,
      type: p.Type || "tcp",
    })),
    created: container.Created * 1000,
    labels: panelLabels(labels),
  };
  return {
    ...managed,
    gameConsole: getGameConsoleAdapterSummary(managed),
    fileRoots: getFileRoots(managed),
  };
}

function panelLabels(labels: Record<string, string>): Record<string, string> {
  return Object.fromEntries(
    Object.entries(labels).filter(([key]) =>
      key.startsWith(`${LABEL_PREFIX}.`)
    )
  );
}
