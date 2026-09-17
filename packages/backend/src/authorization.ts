import {
  findUserById,
  getDatabase,
  writeAuditLog,
  type SessionUser,
  type UserRecord,
} from "./database.js";
import { getLogicalServer, type LogicalServer } from "./identity.js";
import { AppError } from "./errors.js";
import { SERVER_CAPABILITIES, type ServerCapability } from "@ludock/shared";
export { SERVER_CAPABILITIES, type ServerCapability } from "@ludock/shared";

const READ_CAPABILITIES = new Set<ServerCapability>([
  "server.view",
  "logs.read",
  "files.read",
]);
const OPERATOR_CAPABILITIES = new Set<ServerCapability>([
  ...READ_CAPABILITIES,
  "server.start",
  "server.stop",
  "server.restart",
  "console.execute",
  "files.write",
  "backups.create",
  "schedules.manage",
]);

export interface ServerGrant {
  serverId: string;
  capabilities: ServerCapability[];
  updatedAt: number;
}

export interface ServerGrantInput {
  serverId: string;
  capabilities: readonly string[];
}

export class AuthorizationError extends AppError {
  constructor(
    code: string,
    message: string,
    statusCode = 403,
  ) {
    super(code, statusCode, message);
  }
}

/** Resolve current account state for requests, streams, jobs, and schedules.
 * A role carried in a queued payload is never an authorization authority. */
export function currentActor(
  actor: SessionUser | null | undefined,
): SessionUser | null {
  if (!actor) return null;
  // This principal is only constructed by the API-token authentication path.
  if (actor.id === "api-token" && actor.role === "admin") return actor;
  const user = findUserById(actor.id);
  if (!user || user.disabled) return null;
  return { id: user.id, username: user.username, role: user.role };
}

export function assertAdministrator(actor: SessionUser): SessionUser {
  const current = currentActor(actor);
  if (!current || current.role !== "admin") {
    throw new AuthorizationError(
      "FORBIDDEN",
      "Administrator permission required",
    );
  }
  return current;
}

function roleCapabilities(role: UserRecord["role"]): ServerCapability[] {
  if (role === "admin") return [...SERVER_CAPABILITIES];
  return SERVER_CAPABILITIES.filter((capability) =>
    (role === "operator" ? OPERATOR_CAPABILITIES : READ_CAPABILITIES).has(
      capability,
    ),
  );
}

export function listUserServerGrants(userId: string): ServerGrant[] {
  const rows = getDatabase()
    .query(
      `SELECT server_id, capabilities_json, updated_at
    FROM server_grants WHERE user_id = ? ORDER BY server_id`,
    )
    .all(userId) as Array<{
    server_id: string;
    capabilities_json: string;
    updated_at: number;
  }>;
  return rows.map((row) => ({
    serverId: row.server_id,
    capabilities: parseCapabilities(row.capabilities_json),
    updatedAt: row.updated_at,
  }));
}

/** Read the assigned grant without treating binding availability as revocation. */
export function getUserServerGrant(userId: string, serverId: string): ServerGrant | null {
  const row = getDatabase().query(
    "SELECT capabilities_json,updated_at FROM server_grants WHERE user_id=? AND server_id=?",
  ).get(userId, serverId) as { capabilities_json: string; updated_at: number } | null;
  return row ? {
    serverId,
    capabilities: parseCapabilities(row.capabilities_json),
    updatedAt: row.updated_at,
  } : null;
}

function parseCapabilities(json: string): ServerCapability[] {
  try {
    const value: unknown = JSON.parse(json);
    if (!Array.isArray(value)) return [];
    return SERVER_CAPABILITIES.filter((capability) =>
      value.includes(capability),
    );
  } catch {
    // A corrupt grant must never turn into broad/default authorization.
    return [];
  }
}

export function getEffectiveCapabilities(
  actor: SessionUser | null | undefined,
  target: LogicalServer | string,
): ServerCapability[] {
  const user = currentActor(actor);
  const server = getLogicalServer(
    typeof target === "string" ? target : target.id,
  );
  if (!user || !server) return [];
  if (user.role === "admin") {
    // Administrators can inspect status/review metadata even when acting on the
    // container is forbidden. This never authorizes Docker reads or mutation.
    return server.status === "active" && !server.reviewRequired
      ? [...SERVER_CAPABILITIES]
      : ["server.view"];
  }
  if (server.status !== "active" || server.reviewRequired) return [];
  const grant = getDatabase()
    .query(
      `SELECT capabilities_json FROM server_grants
    WHERE user_id = ? AND server_id = ?`,
    )
    .get(user.id, server.id) as { capabilities_json: string } | undefined;
  if (!grant) return [];
  const assigned = new Set(parseCapabilities(grant.capabilities_json));
  if (!assigned.has("server.view")) return [];
  const allowed = roleCapabilities(user.role).filter((capability) =>
    assigned.has(capability),
  );
  return allowed.filter(
    (capability) => capability !== "files.write" || assigned.has("files.read"),
  );
}

export function hasServerCapability(
  actor: SessionUser | null | undefined,
  target: LogicalServer | string,
  capability: ServerCapability,
): boolean {
  return getEffectiveCapabilities(actor, target).includes(capability);
}

export function assertServerCapability(
  actor: SessionUser,
  serverId: string,
  capability: ServerCapability,
): LogicalServer {
  const capabilities = getEffectiveCapabilities(actor, serverId);
  if (!capabilities.includes("server.view")) {
    auditDenial(actor, serverId, capability);
    // The same response covers a nonexistent server, suspended identity, and an
    // unassigned server, without disclosing another user's resource identifiers.
    throw new AuthorizationError("SERVER_NOT_FOUND", "Server not found", 404);
  }
  if (!capabilities.includes(capability)) {
    auditDenial(actor, serverId, capability);
    throw new AuthorizationError(
      "FORBIDDEN",
      "Insufficient server permissions",
    );
  }
  return getLogicalServer(serverId)!;
}

function auditDenial(
  actor: SessionUser,
  serverId: string,
  capability: ServerCapability,
): void {
  if (READ_CAPABILITIES.has(capability)) return;
  const current = currentActor(actor);
  writeAuditLog({
    userId: current?.id === "api-token" ? undefined : current?.id,
    action: "authorization.denied",
    targetType: "server",
    targetId: serverId,
    details: { capability },
  });
}

function validateGrant(
  role: UserRecord["role"],
  input: ServerGrantInput,
): ServerCapability[] {
  if (!getLogicalServer(input.serverId)) {
    throw new AuthorizationError("SERVER_NOT_FOUND", "Server not found", 404);
  }
  const ceiling = new Set<string>(roleCapabilities(role));
  if (input.capabilities.some((capability) => !ceiling.has(capability))) {
    throw new AuthorizationError(
      "INVALID_GRANT",
      "A capability exceeds this user's role",
      400,
    );
  }
  const capabilities = SERVER_CAPABILITIES.filter((capability) =>
    input.capabilities.includes(capability),
  );
  if (capabilities.length > 0 && !capabilities.includes("server.view")) {
    throw new AuthorizationError(
      "INVALID_GRANT",
      "Server access is required for every server capability",
      400,
    );
  }
  if (
    capabilities.includes("files.write") &&
    !capabilities.includes("files.read")
  ) {
    throw new AuthorizationError(
      "INVALID_GRANT",
      "File writing also requires file reading",
      400,
    );
  }
  return capabilities;
}

/** Replace the user's complete assignment set atomically. Empty grants revoke
 * access. This does not infer additional permissions from UI presets. */
export function setUserServerGrants(
  userId: string,
  inputs: readonly ServerGrantInput[],
  actor: SessionUser,
): ServerGrant[] {
  const administrator = assertAdministrator(actor);
  const user = findUserById(userId);
  if (!user)
    throw new AuthorizationError("USER_NOT_FOUND", "User not found", 404);
  if (user.role === "admin") {
    throw new AuthorizationError(
      "INVALID_GRANT",
      "Administrators already have access to all eligible servers",
      400,
    );
  }
  if (new Set(inputs.map((input) => input.serverId)).size !== inputs.length) {
    throw new AuthorizationError(
      "INVALID_GRANT",
      "Duplicate server assignment",
      400,
    );
  }
  const validated = inputs.map((input) => ({
    ...input,
    capabilities: validateGrant(user.role, input),
  }));
  const db = getDatabase();
  const now = Date.now();
  db.exec("BEGIN IMMEDIATE");
  try {
    db.query("DELETE FROM server_grants WHERE user_id = ?").run(userId);
    const insert = db.query(`INSERT INTO server_grants
      (user_id, server_id, capabilities_json, updated_at) VALUES (?, ?, ?, ?)`);
    for (const grant of validated) {
      if (grant.capabilities.length === 0) continue;
      insert.run(
        userId,
        grant.serverId,
        JSON.stringify(grant.capabilities),
        now,
      );
    }
    writeAuditLog({
      userId: administrator.id === "api-token" ? undefined : administrator.id,
      action: "users.server_grants.updated",
      targetType: "user",
      targetId: userId,
      details: {
        grants: validated.map(({ serverId, capabilities }) => ({
          serverId,
          capabilities,
        })),
      },
    });
    db.exec("COMMIT");
  } catch (error) {
    db.exec("ROLLBACK");
    throw error;
  }
  return listUserServerGrants(userId);
}

export function setServerGrant(
  userId: string,
  serverId: string,
  capabilities: readonly string[],
  actor: SessionUser,
): ServerGrant[] {
  const current = listUserServerGrants(userId).filter(
    (grant) => grant.serverId !== serverId,
  );
  return setUserServerGrants(
    userId,
    [...current, { serverId, capabilities }],
    actor,
  );
}
