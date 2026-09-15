import { z } from "zod";
import {
  operationStatusSchema,
  type AuditEntry,
  type AuditHistoryQuery,
  type Operation,
  type OperationHistoryQuery,
} from "@ludock/shared";
import { hasServerCapability } from "./authorization.js";
import { publicHistoryActor, publicOperationActorId } from "./auth.js";
import { getDatabase, type SessionUser } from "./database.js";
import { AppError } from "./errors.js";
import { listLogicalServers } from "./identity.js";

type HistoryQuery = AuditHistoryQuery | OperationHistoryQuery;
type SqlValue = string | number;
const cursorSchema = z.object({
  scope: z.string().length(64),
  createdAt: z.number().int().nonnegative().max(Number.MAX_SAFE_INTEGER),
  id: z.union([z.string().uuid(), z.number().int().positive()]),
});

function cursorScope(kind: string, query: HistoryQuery): string {
  const filters = Object.fromEntries(Object.entries(query)
    .filter(([key]) => key !== "cursor" && key !== "limit")
    .sort(([left], [right]) => left.localeCompare(right)));
  return new Bun.CryptoHasher("sha256").update(JSON.stringify([kind, filters])).digest("hex");
}

function pageConditions(kind: "audit" | "operations", query: HistoryQuery, alias: string) {
  const conditions: string[] = [];
  const values: SqlValue[] = [];
  const scope = cursorScope(kind, query);
  if (query.cursor) {
    let cursor: z.infer<typeof cursorSchema>;
    try {
      cursor = cursorSchema.parse(JSON.parse(Buffer.from(query.cursor, "base64url").toString("utf8")));
      if (cursor.scope !== scope || typeof cursor.id !== (kind === "audit" ? "number" : "string"))
        throw new Error("Cursor scope mismatch");
    } catch {
      throw new AppError("INVALID_HISTORY_CURSOR", 400, "This history cursor is invalid for the selected filters");
    }
    conditions.push(`(${alias}.created_at < ? OR (${alias}.created_at = ? AND ${alias}.id < ?))`);
    values.push(cursor.createdAt, cursor.createdAt, cursor.id);
  }
  if (query.from !== undefined) {
    conditions.push(`${alias}.created_at >= ?`);
    values.push(query.from);
  }
  if (query.to !== undefined) {
    conditions.push(`${alias}.created_at <= ?`);
    values.push(query.to);
  }
  return { conditions, values, scope };
}

function nextCursor<T extends { id: string | number; createdAt: number }>(rows: T[], limit: number, scope: string): string | null {
  const last = rows[limit - 1];
  return rows.length > limit && last
    ? Buffer.from(JSON.stringify({ scope, createdAt: last.createdAt, id: last.id })).toString("base64url")
    : null;
}

// Only the safe public token label participates in search or appears in output.
const safeActorSql = (value: string) =>
  `CASE WHEN ${value} = 'api-token' OR ${value} GLOB 'api-token:*' THEN 'api-token' ELSE ${value} END`;

interface OperationHistoryRow {
  id: string;
  server_id: string;
  actor_id: string;
  username: string | null;
  kind: string;
  status: Operation["status"];
  phase: string;
  created_at: number;
  updated_at: number;
  error: string | null;
  result_json: string | null;
}

/** Restrict readable server IDs before ordering, limiting, or making a cursor. */
export function listOperationHistory(actor: SessionUser, query: OperationHistoryQuery): { operations: Operation[]; nextCursor: string | null } {
  const { conditions, values, scope } = pageConditions("operations", query, "o");
  const candidates = query.serverId ? [query.serverId] : listLogicalServers();
  const serverIds = candidates.filter((server) => hasServerCapability(actor, server, "server.view"))
    .map((server) => typeof server === "string" ? server : server.id);
  if (serverIds.length === 0) return { operations: [], nextCursor: null };
  conditions.push(`o.server_id IN (${serverIds.map(() => "?").join(",")})`);
  values.push(...serverIds);
  if (query.action) {
    conditions.push("instr(lower(o.kind), lower(?)) > 0");
    values.push(query.action);
  }
  if (query.status) {
    conditions.push("o.status = ?");
    values.push(query.status);
  }
  if (query.actor) {
    conditions.push(`(${safeActorSql("o.actor_id")} = ? COLLATE NOCASE OR instr(lower(u.username), lower(?)) > 0 OR (${safeActorSql("o.actor_id")} = 'api-token' AND instr('api token', lower(?)) > 0))`);
    values.push(query.actor, query.actor, query.actor);
  }
  const rows = getDatabase().prepare(`
    SELECT o.id, o.server_id, ${safeActorSql("o.actor_id")} AS actor_id, u.username,
      o.kind, o.status, o.phase, o.created_at, o.updated_at, o.error, o.result_json
    FROM operations o LEFT JOIN users u ON u.id = o.actor_id
    WHERE ${conditions.join(" AND ")}
    ORDER BY o.created_at DESC, o.id DESC LIMIT ?
  `).all(...values, query.limit + 1) as OperationHistoryRow[];
  const operations = rows.map((row): Operation => ({
    id: row.id,
    serverId: row.server_id,
    actor: publicHistoryActor(row.actor_id, row.username),
    kind: row.kind,
    status: row.status,
    phase: row.phase,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
    error: row.error,
    result: row.result_json ? JSON.parse(row.result_json) as Record<string, unknown> : null,
  }));
  return { operations: operations.slice(0, query.limit), nextCursor: nextCursor(operations, query.limit, scope) };
}

// Old events may have arbitrary JSON details. Do not interpret invalid JSON as links.
const auditOperationIdSql = "CASE WHEN json_valid(a.details_json) THEN CASE WHEN json_type(a.details_json, '$.operationId') = 'text' THEN json_extract(a.details_json, '$.operationId') END END";
const auditDetailsActorSql = "CASE WHEN json_valid(a.details_json) THEN CASE WHEN json_type(a.details_json, '$.actorId') = 'text' THEN json_extract(a.details_json, '$.actorId') END END";
const auditActorSql = safeActorSql(`COALESCE(a.user_id, ${auditDetailsActorSql}, o.actor_id)`);
// This is the event outcome recorded in the action, not the linked operation's current state.
const auditStatusSql = `CASE ${operationStatusSchema.options.map((status) => `WHEN substr(a.action, -${status.length + 1}) = '.${status}' THEN '${status}'`).join(" ")} END`;

interface AuditHistoryRow {
  id: number;
  actor_id: string | null;
  username: string | null;
  actor_name: string | null;
  action: string;
  status: AuditEntry["status"];
  operation_id: string | null;
  target_type: string | null;
  target_id: string | null;
  details_json: string | null;
  ip_address: string | null;
  created_at: number;
}

export function listAuditHistory(query: AuditHistoryQuery): { entries: AuditEntry[]; nextCursor: string | null } {
  const { conditions, values, scope } = pageConditions("audit", query, "a");
  if (query.serverId) {
    conditions.push("a.target_type = 'server' AND a.target_id = ?");
    values.push(query.serverId);
  }
  if (query.operationId) {
    conditions.push(`${auditOperationIdSql} = ?`);
    values.push(query.operationId);
  }
  if (query.action) {
    conditions.push("instr(lower(a.action), lower(?)) > 0");
    values.push(query.action);
  }
  if (query.status) {
    conditions.push(`${auditStatusSql} = ?`);
    values.push(query.status);
  }
  if (query.actor) {
    conditions.push(`(${auditActorSql} = ? COLLATE NOCASE OR instr(lower(COALESCE(u.username, actor_user.username)), lower(?)) > 0 OR (${auditActorSql} = 'api-token' AND instr('api token', lower(?)) > 0))`);
    values.push(query.actor, query.actor, query.actor);
  }
  const rows = getDatabase().prepare(`
    SELECT a.id, ${auditActorSql} AS actor_id, u.username,
      COALESCE(u.username, actor_user.username) AS actor_name,
      a.action, ${auditStatusSql} AS status, ${auditOperationIdSql} AS operation_id,
      a.target_type, a.target_id, a.details_json, a.ip_address, a.created_at
    FROM audit_log a LEFT JOIN users u ON u.id = a.user_id
    LEFT JOIN operations o ON o.id = ${auditOperationIdSql}
      AND a.target_type = 'server' AND o.server_id = a.target_id
    LEFT JOIN users actor_user ON actor_user.id = COALESCE(${auditDetailsActorSql}, o.actor_id)
    ${conditions.length ? `WHERE ${conditions.join(" AND ")}` : ""}
    ORDER BY a.created_at DESC, a.id DESC LIMIT ?
  `).all(...values, query.limit + 1) as AuditHistoryRow[];
  const entries = rows.map((row): AuditEntry => {
    let details: unknown = null;
    try { details = row.details_json ? JSON.parse(row.details_json) : null; } catch { /* Preserve unreadable historical events without breaking the page. */ }
    if (details && typeof details === "object" && !Array.isArray(details) && "actorId" in details && typeof details.actorId === "string")
      details = { ...details, actorId: publicOperationActorId(details.actorId) };
    const operationId = z.string().uuid().safeParse(row.operation_id);
    return {
      id: row.id,
      username: row.username,
      actor: publicHistoryActor(row.actor_id, row.actor_name),
      action: row.action,
      status: row.status ?? null,
      operationId: operationId.success ? operationId.data : null,
      targetType: row.target_type,
      targetId: row.target_id,
      details,
      ipAddress: row.ip_address,
      createdAt: row.created_at,
    };
  });
  return { entries: entries.slice(0, query.limit), nextCursor: nextCursor(entries, query.limit, scope) };
}
