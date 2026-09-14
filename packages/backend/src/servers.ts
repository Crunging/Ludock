import path from "node:path";
import {
  serverSchema,
  serverStatsSchema,
  type Server,
  type ServerStats,
  type ServerCapability,
  type DockerContainerId,
} from "@ludock/shared";
import { getDatabase, type SessionUser } from "./database.js";
import {
  listManagedContainerObservations,
  getManagedContainerObservation,
  getContainerStats,
  type ManagedContainer,
} from "./docker.js";
import {
  reconcileServers,
  listLogicalServers,
  resolveServerBinding,
  assertObservedServerBinding,
  type LogicalServer,
  type ServerObservation,
} from "./identity.js";
import {
  assertServerCapability,
  currentActor,
  getEffectiveCapabilities,
} from "./authorization.js";

let refreshing: Promise<Map<string, ManagedContainer>> | null = null;
export async function refreshServers(): Promise<Map<string, ManagedContainer>> {
  refreshing ??= (async () => {
    const observed = await listManagedContainerObservations();
    reconcileServers(observed.map((entry) => entry.observation));
    return new Map(
      observed.map((entry) => [entry.container.id, entry.container]),
    );
  })().finally(() => {
    refreshing = null;
  });
  return refreshing;
}
function toPublicServer(
  actor: SessionUser,
  logical: LogicalServer,
  permissions: ServerCapability[],
  container?: ManagedContainer,
): Server {
  return serverSchema.parse({
    ...(container ?? {
      shortId: "",
      name: logical.displayName,
      displayName: logical.displayName,
      image: "",
      state: logical.status,
      status: logical.status.replaceAll("_", " "),
      gameType: logical.gameType,
      gameConsole: null,
      fileRoots: [],
      ports: [],
      created: logical.firstSeenAt,
      labels: {},
    }),
    id: logical.id,
    bindingStatus: logical.status,
    permissions,
    // Administrators retain durable backup history when a container binding is
    // unavailable. A create grant exposes only this summary, never archive data.
    latestBackup:
      currentActor(actor)?.role === "admin" ||
      permissions.includes("backups.read") ||
      permissions.includes("backups.create")
        ? (getDatabase().prepare(
            `SELECT created_at AS createdAt,size FROM backups
             WHERE server_id=? AND state='complete'
             ORDER BY created_at DESC,id LIMIT 1`,
          ).get(logical.id) ?? null)
        : null,
    fileRoots: permissions.includes("files.read")
      ? (container?.fileRoots ?? [])
      : [],
    // Labels configure integration internals; the public contract exposes only
    // selected names and capability summaries, never arbitrary label values.
    labels: {},
  });
}
export async function listServers(actor: SessionUser): Promise<Server[]> {
  const current = await refreshServers();
  return listLogicalServers().flatMap((server) => {
    const permissions = getEffectiveCapabilities(actor, server);
    return permissions.includes("server.view")
      ? [toPublicServer(actor, server, permissions, current.get(server.containerId ?? ""))]
      : [];
  });
}
export async function getServer(
  actor: SessionUser,
  id: string,
): Promise<Server> {
  const current = await refreshServers();
  const server = assertServerCapability(actor, id, "server.view");
  return toPublicServer(
    actor,
    server,
    getEffectiveCapabilities(actor, server),
    current.get(server.containerId ?? ""),
  );
}

/** Saved history remains inspectable during a daemon outage. A saved logical
 * binding does not establish the current container's identity or live state. */
export async function getServerSnapshot(
  actor: SessionUser,
  id: string,
): Promise<{ server: Server; stats: ServerStats | null; discoveryUnavailable: boolean }> {
  const current = await refreshServers().catch(() => null);
  const original = assertServerCapability(actor, id, "server.view");
  let stats: ServerStats | null = null;
  if (current && original.status === "active") {
    try {
      const context = await resolveAuthorizedServer(actor, id, "server.view");
      stats = serverStatsSchema.parse(await getContainerStats(context.container.id));
    } catch {
      // Saved history remains inspectable when live statistics are unavailable.
    }
  }
  // Statistics involve additional asynchronous reads. Recheck access and the
  // observed binding before responding, without starting another Docker read.
  const logical = assertServerCapability(actor, id, "server.view");
  const discoveryUnavailable = current === null ||
    logical.containerId !== original.containerId ||
    logical.bindingRevision !== original.bindingRevision ||
    logical.bindingFingerprint !== original.bindingFingerprint ||
    logical.status !== original.status || logical.reviewRequired !== original.reviewRequired;
  const server = toPublicServer(
    actor,
    logical,
    getEffectiveCapabilities(actor, logical),
    discoveryUnavailable ? undefined : current.get(logical.containerId ?? ""),
  );
  return {
    server: discoveryUnavailable ? {
      ...server,
      state: "unknown",
      status: "Live status unavailable",
    } : server,
    stats: discoveryUnavailable ? null : stats,
    discoveryUnavailable,
  };
}

export interface ServerContext {
  logical: LogicalServer & { containerId: DockerContainerId };
  container: ManagedContainer;
  observation: ServerObservation;
  lockKeys: string[];
}
export async function resolveAuthorizedServer(
  actor: SessionUser,
  id: string,
  capability: ServerCapability,
  expectedRevision?: number,
): Promise<ServerContext> {
  await refreshServers();
  assertServerCapability(actor, id, capability);
  const logical = resolveServerBinding(id, expectedRevision);
  const observed = await getManagedContainerObservation(logical.containerId);
  assertServerCapability(actor, id, capability);
  assertObservedServerBinding(
    id,
    observed.observation,
    logical.bindingRevision,
  );
  return {
    logical,
    ...observed,
    lockKeys: serverLockKeys(id, observed.observation),
  };
}
export function serverLockKeys(
  id: string,
  observation: ServerObservation,
): string[] {
  return [
    `server:${id}`,
    ...(observation.compose ? [`project:${observation.compose.project}`] : []),
    ...observation.mounts
      .filter((mount) => mount.writable)
      .flatMap((mount) =>
        mount.type === "volume"
          ? [
              `volume:${mount.name || mount.source}`,
              ...(path.posix.isAbsolute(mount.source)
                ? [`path:${path.posix.normalize(mount.source)}`]
                : []),
            ]
          : [`path:${path.posix.normalize(mount.source)}`],
      ),
  ];
}
