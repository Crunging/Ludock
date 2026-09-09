import path from "node:path";
import {
  serverSchema,
  type Server,
  type ServerCapability,
  type DockerContainerId,
} from "@ludock/shared";
import type { SessionUser } from "./database.js";
import {
  listManagedContainerObservations,
  getManagedContainerObservation,
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
  container?: ManagedContainer,
): Server {
  const permissions = getEffectiveCapabilities(actor, logical);
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
  return listLogicalServers()
    .filter((server) =>
      getEffectiveCapabilities(actor, server).includes("server.view"),
    )
    .map((server) =>
      toPublicServer(actor, server, current.get(server.containerId ?? "")),
    );
}
export async function getServer(
  actor: SessionUser,
  id: string,
): Promise<Server> {
  const current = await refreshServers();
  const server = assertServerCapability(actor, id, "server.view");
  return toPublicServer(actor, server, current.get(server.containerId ?? ""));
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
