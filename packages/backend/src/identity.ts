import path from "node:path";
import { getDatabase, keyedBindingFingerprint } from "./database.js";
import {
  dockerContainerIdSchema,
  logicalServerIdSchema,
  type DockerContainerId,
  type LogicalServerId,
} from "@ludock/shared";

export interface ServerObservation {
  containerId: DockerContainerId;
  name: string;
  displayName: string;
  gameType: string;
  compose?: { project: string; service: string; containerNumber: string };
  projectRegistrationId?: string | null;
  mounts: Array<{
    type: string;
    source: string;
    destination: string;
    writable: boolean;
    name?: string;
  }>;
  /** Input only: values are keyed-hashed, never stored or returned by this module. */
  gameConfiguration?: Record<string, string>;
}

export type ServerBindingStatus =
  | "active"
  | "missing"
  | "ambiguous"
  | "review_required";

export interface LogicalServer {
  id: LogicalServerId;
  hostId: string;
  externalIdentity: string;
  containerId: DockerContainerId | null;
  displayName: string;
  gameType: string;
  status: ServerBindingStatus;
  bindingRevision: number;
  bindingFingerprint: string;
  pendingFingerprint: string | null;
  pendingGameType: string | null;
  reviewRequired: boolean;
  firstSeenAt: number;
  lastSeenAt: number;
}

interface ServerRow {
  id: string;
  host_id: string;
  external_identity: string;
  container_id: string | null;
  display_name: string;
  game_type: string;
  status: ServerBindingStatus;
  binding_revision: number;
  binding_fingerprint: string;
  pending_fingerprint: string | null;
  pending_game_type: string | null;
  review_required: number;
  first_seen_at: number;
  last_seen_at: number;
}

export class ServerBindingError extends Error {
  constructor(
    public readonly code: string,
    message: string,
    public readonly statusCode = 409,
  ) {
    super(message);
  }
}

export function getDockerHostId(name = "local"): string {
  const db = getDatabase();
  const existing = db
    .prepare("SELECT id FROM docker_hosts WHERE name = ?")
    .get(name) as { id: string } | null;
  if (existing) return existing.id;
  const id = crypto.randomUUID();
  db.prepare(
    "INSERT INTO docker_hosts (id, name, created_at) VALUES (?, ?, ?)",
  ).run(id, name, Date.now());
  return id;
}

function validIdentityPart(value: string): boolean {
  return (
    typeof value === "string" &&
    value.length > 0 &&
    value.length <= 512 &&
    ![...value].some(
      (character) =>
        character.charCodeAt(0) < 32 || character.charCodeAt(0) === 127,
    )
  );
}

export function externalServerIdentity(observation: ServerObservation): string {
  if (
    !validIdentityPart(observation.containerId) ||
    !validIdentityPart(observation.name)
  ) {
    throw new ServerBindingError(
      "INVALID_SERVER_OBSERVATION",
      "Invalid Docker server identity",
    );
  }
  if (observation.compose) {
    const { project, service, containerNumber } = observation.compose;
    if (
      !validIdentityPart(project) ||
      !validIdentityPart(service) ||
      !/^[1-9][0-9]*$/.test(containerNumber)
    ) {
      throw new ServerBindingError(
        "INVALID_SERVER_OBSERVATION",
        "Incomplete Compose server identity",
      );
    }
    return `compose:${encodeURIComponent(project)}:${encodeURIComponent(service)}:${containerNumber}`;
  }
  return `standalone:${encodeURIComponent(observation.name.replace(/^\//, ""))}`;
}

export function bindingFingerprint(observation: ServerObservation): string {
  const mounts = observation.mounts
    .filter((mount) => mount.writable)
    .map((mount) => ({
      type: mount.type,
      source:
        mount.type === "volume" && mount.name
          ? mount.name
          : path.posix.normalize(mount.source),
      destination: path.posix.normalize(mount.destination),
    }))
    .sort((left, right) =>
      JSON.stringify(left).localeCompare(JSON.stringify(right)),
    );
  const configuration = Object.entries(
    observation.gameConfiguration ?? {},
  ).sort(([left], [right]) => left.localeCompare(right));
  const digest = new Bun.CryptoHasher("sha256")
    .update(
      JSON.stringify({
        projectRegistrationId: observation.projectRegistrationId ?? null,
        gameType: observation.gameType,
        mounts,
        configuration,
      }),
    )
    .digest("hex");
  return keyedBindingFingerprint(digest);
}

function toLogicalServer(row: ServerRow): LogicalServer {
  const id = logicalServerIdSchema.safeParse(row.id);
  const containerId = dockerContainerIdSchema
    .nullable()
    .safeParse(row.container_id);
  if (!id.success || !containerId.success) {
    throw new ServerBindingError(
      "INVALID_STORED_IDENTITY",
      "Stored server identity is invalid; review application data.",
      500,
    );
  }
  return {
    id: id.data,
    hostId: row.host_id,
    externalIdentity: row.external_identity,
    containerId: containerId.data,
    displayName: row.display_name,
    gameType: row.game_type,
    status: row.status,
    bindingRevision: row.binding_revision,
    bindingFingerprint: row.binding_fingerprint,
    pendingFingerprint: row.pending_fingerprint,
    pendingGameType: row.pending_game_type,
    reviewRequired: row.review_required === 1,
    firstSeenAt: row.first_seen_at,
    lastSeenAt: row.last_seen_at,
  };
}

export function listLogicalServers(hostId?: string): LogicalServer[] {
  const db = getDatabase();
  const rows = (hostId
    ? db
        .prepare(
          "SELECT * FROM logical_servers WHERE host_id = ? ORDER BY display_name COLLATE NOCASE, id",
        )
        .all(hostId)
    : db
        .prepare(
          "SELECT * FROM logical_servers ORDER BY display_name COLLATE NOCASE, id",
        )
        .all()) as unknown as ServerRow[];
  return rows.map(toLogicalServer);
}

export function getLogicalServer(serverId: string): LogicalServer | null {
  const row = getDatabase()
    .prepare("SELECT * FROM logical_servers WHERE id = ?")
    .get(serverId) as ServerRow | null;
  return row ? toLogicalServer(row) : null;
}

export function resolveServerBinding(
  serverId: string,
  expectedRevision?: number,
): LogicalServer & { containerId: DockerContainerId } {
  const server = getLogicalServer(serverId);
  if (!server)
    throw new ServerBindingError("SERVER_NOT_FOUND", "Server not found", 404);
  if (
    server.status !== "active" ||
    server.reviewRequired ||
    !server.containerId
  ) {
    throw new ServerBindingError(
      "SERVER_BINDING_UNAVAILABLE",
      "Server binding is unavailable or requires administrator review",
    );
  }
  if (
    expectedRevision !== undefined &&
    server.bindingRevision !== expectedRevision
  ) {
    throw new ServerBindingError(
      "SERVER_BINDING_CHANGED",
      "The server binding changed; reload before trying again",
    );
  }
  return { ...server, containerId: server.containerId };
}

/** Validate a fresh inspect immediately before a mutation. Does not reconcile a
 * replacement silently into an operation that was authorized for an older one. */
export function assertObservedServerBinding(
  serverId: string,
  observation: ServerObservation,
  expectedRevision?: number,
): LogicalServer & { containerId: DockerContainerId } {
  const server = resolveServerBinding(serverId, expectedRevision);
  if (
    server.containerId !== observation.containerId ||
    server.externalIdentity !== externalServerIdentity(observation) ||
    server.bindingFingerprint !== bindingFingerprint(observation)
  ) {
    throw new ServerBindingError(
      "SERVER_BINDING_CHANGED",
      "Docker server identity changed; refresh and review before continuing",
    );
  }
  return server;
}

/** Call only with a complete, successful eligible-container snapshot for one
 * host. A failed/partial Docker listing must not make healthy servers missing. */
export function reconcileServers(
  observations: readonly ServerObservation[],
  options: { hostId?: string; now?: number } = {},
): LogicalServer[] {
  const db = getDatabase();
  const hostId = options.hostId ?? getDockerHostId();
  const now = options.now ?? Date.now();
  const groups = new Map<string, ServerObservation[]>();
  const seenContainers = new Set<string>();
  for (const observation of observations) {
    const key = externalServerIdentity(observation);
    // Docker can never report one container twice in a coherent snapshot.
    if (seenContainers.has(observation.containerId)) {
      throw new ServerBindingError(
        "INVALID_SERVER_SNAPSHOT",
        "Duplicate container in Docker snapshot",
      );
    }
    seenContainers.add(observation.containerId);
    const group = groups.get(key) ?? [];
    group.push(observation);
    groups.set(key, group);
  }
  db.exec("BEGIN IMMEDIATE");
  try {
    const current = new Map(
      listLogicalServers(hostId).map((server) => [
        server.externalIdentity,
        server,
      ]),
    );
    for (const [externalIdentity, group] of groups) {
      let server = current.get(externalIdentity);
      const observation = group[0];
      const ambiguous = group.length !== 1;
      if (!server) {
        const id = crypto.randomUUID();
        const fingerprint = ambiguous ? "" : bindingFingerprint(observation);
        db.prepare(
          `INSERT INTO logical_servers
          (id, host_id, external_identity, container_id, display_name, game_type,
           status, binding_revision, binding_fingerprint, first_seen_at, last_seen_at)
          VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
        ).run(
          id,
          hostId,
          externalIdentity,
          ambiguous ? null : observation.containerId,
          observation.displayName,
          ambiguous ? "unknown" : observation.gameType,
          ambiguous ? "ambiguous" : "active",
          ambiguous ? 0 : 1,
          fingerprint,
          now,
          now,
        );
        if (!ambiguous)
          recordBinding(id, 1, observation, fingerprint, now, true);
        continue;
      }
      if (ambiguous) {
        db.prepare(
          "UPDATE logical_servers SET status = 'ambiguous', container_id = NULL, last_seen_at = ? WHERE id = ?",
        ).run(now, server.id);
        continue;
      }
      const fingerprint = bindingFingerprint(observation);
      const materialChange =
        server.bindingFingerprint !== "" &&
        fingerprint !== server.bindingFingerprint;
      const reviewRequired = server.reviewRequired || materialChange;
      const changed =
        server.containerId !== observation.containerId ||
        (reviewRequired
          ? server.pendingFingerprint !== fingerprint
          : server.bindingFingerprint !== fingerprint);
      const revision = changed
        ? server.bindingRevision + 1
        : server.bindingRevision;
      if (reviewRequired) {
        db.prepare(
          `UPDATE logical_servers SET container_id = ?, display_name = ?,
          status = 'review_required', binding_revision = ?, pending_fingerprint = ?,
          pending_game_type = ?, review_required = 1, last_seen_at = ? WHERE id = ?`,
        ).run(
          observation.containerId,
          observation.displayName,
          revision,
          fingerprint,
          observation.gameType,
          now,
          server.id,
        );
      } else {
        db.prepare(
          `UPDATE logical_servers SET container_id = ?, display_name = ?, game_type = ?,
          status = 'active', binding_revision = ?, binding_fingerprint = ?, last_seen_at = ? WHERE id = ?`,
        ).run(
          observation.containerId,
          observation.displayName,
          observation.gameType,
          revision,
          fingerprint,
          now,
          server.id,
        );
      }
      if (changed)
        recordBinding(
          server.id,
          revision,
          observation,
          fingerprint,
          now,
          !reviewRequired,
        );
      server = getLogicalServer(server.id)!;
      current.set(externalIdentity, server);
    }
    for (const server of current.values()) {
      if (!groups.has(server.externalIdentity)) {
        db.prepare(
          "UPDATE logical_servers SET status = 'missing', container_id = NULL WHERE id = ?",
        ).run(server.id);
      }
    }
    db.exec("COMMIT");
  } catch (error) {
    db.exec("ROLLBACK");
    throw error;
  }
  return listLogicalServers(hostId);
}

function recordBinding(
  serverId: string,
  revision: number,
  observation: ServerObservation,
  fingerprint: string,
  now: number,
  accepted: boolean,
): void {
  getDatabase()
    .prepare(
      `INSERT INTO server_bindings
    (server_id, binding_revision, container_id, binding_fingerprint, observed_at, accepted)
    VALUES (?, ?, ?, ?, ?, ?)`,
    )
    .run(
      serverId,
      revision,
      observation.containerId,
      fingerprint,
      now,
      accepted ? 1 : 0,
    );
}

/** The caller must authorize an administrator and freshly reconcile Docker
 * before presenting/accepting this exact pending fingerprint. */
export function reviewServerBinding(
  serverId: string,
  expectedFingerprint: string,
): LogicalServer {
  const server = getLogicalServer(serverId);
  if (!server)
    throw new ServerBindingError("SERVER_NOT_FOUND", "Server not found", 404);
  if (
    server.status !== "review_required" ||
    !server.containerId ||
    !server.pendingFingerprint ||
    server.pendingFingerprint !== expectedFingerprint
  ) {
    throw new ServerBindingError(
      "SERVER_REVIEW_CHANGED",
      "The pending server binding changed; reload the review",
    );
  }
  const db = getDatabase();
  db.exec("BEGIN IMMEDIATE");
  try {
    db.prepare(
      `UPDATE logical_servers SET status = 'active', binding_revision = binding_revision + 1,
      binding_fingerprint = pending_fingerprint, game_type = pending_game_type,
      pending_fingerprint = NULL, pending_game_type = NULL, review_required = 0 WHERE id = ?`,
    ).run(serverId);
    db.prepare(
      `INSERT INTO server_bindings
      (server_id, binding_revision, container_id, binding_fingerprint, observed_at, accepted)
      VALUES (?, ?, ?, ?, ?, 1)`,
    ).run(
      serverId,
      server.bindingRevision + 1,
      server.containerId,
      expectedFingerprint,
      Date.now(),
    );
    db.exec("COMMIT");
  } catch (error) {
    db.exec("ROLLBACK");
    throw error;
  }
  return getLogicalServer(serverId)!;
}
