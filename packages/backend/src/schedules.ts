import {
  nextScheduleRun,
  scheduleEnabledRequestSchema,
  scheduleSchema,
  scheduleSlot,
  updateScheduleRequestSchema,
  type ScheduleInput,
} from "@ludock/shared";
import {
  getDatabase,
  findUserById,
  writeAuditLog,
  pruneAuditLogIfNeeded,
  type SessionUser,
} from "./database.js";
import {
  assertServerCapability,
  currentActor,
  hasServerCapability,
} from "./authorization.js";
import { resolveServerBinding } from "./identity.js";
import { enqueueOperation } from "./operations.js";
import { AppError } from "./errors.js";
import { notifyEvent } from "./notifications.js";
import { createLogger } from "./logger.js";
import type { ServerCapability } from "@ludock/shared";

const logger = createLogger("schedules");
export { scheduleSlot } from "@ludock/shared";

interface ScheduleRow {
  id: string;
  server_id: string;
  owner_id: string;
  input_json: string;
  binding_revision: number;
  revision: number;
  last_slot: string | null;
  last_result: string | null;
  created_at: number;
}

export const MAX_SCHEDULE_PAYLOAD_BYTES = 4 * 1024;
export const MAX_SCHEDULES_PER_SERVER = 100;
export const MAX_SCHEDULES_TOTAL = 1_000;

const actionCapability = (action: ScheduleInput["action"]): ServerCapability =>
  action === "backup" ? "backups.create" : `server.${action}`;
function scheduleAuthority(row: ScheduleRow, data: ScheduleInput) {
  const user = findUserById(row.owner_id);
  if (
    !user ||
    user.disabled ||
    !hasServerCapability(user, row.server_id, "schedules.manage") ||
    !hasServerCapability(user, row.server_id, actionCapability(data.action))
  )
    throw new AppError(
      "SCHEDULE_REVOKED",
      403,
      "Suspended: required access has been removed",
    );
  const binding = resolveServerBinding(row.server_id);
  const original = getDatabase()
    .prepare(
      "SELECT binding_fingerprint FROM server_bindings WHERE server_id=? AND binding_revision=? AND accepted=1 ORDER BY id DESC LIMIT 1",
    )
    .get(row.server_id, row.binding_revision) as
    | { binding_fingerprint: string }
    | undefined;
  if (!original || original.binding_fingerprint !== binding.bindingFingerprint)
    throw new AppError(
      "SCHEDULE_BINDING_CHANGED",
      409,
      "Suspended: server configuration changed; recreate this schedule after reviewing the server",
    );
  return { user, binding };
}
function publicSchedule(row: ScheduleRow, now = Date.now()) {
  const data = scheduleSchema.parse(JSON.parse(row.input_json));
  let nextRunAt: number | null = null;
  if (data.enabled) {
    try {
      scheduleAuthority(row, data);
      nextRunAt = nextScheduleRun(data, now, row.last_slot);
    } catch {
      // Preview unavailable work without exposing private binding/access details.
    }
  }
  return {
    ...data,
    id: row.id,
    serverId: row.server_id,
    ownerId: row.owner_id,
    lastResult: row.last_result,
    lastSlot: row.last_slot,
    revision: row.revision,
    nextRunAt,
  };
}
export function listSchedules(actor: SessionUser, serverId: string) {
  assertServerCapability(actor, serverId, "schedules.manage");
  const current = currentActor(actor)!;
  const query = current.role === "admin"
    ? "SELECT * FROM schedules WHERE server_id=? ORDER BY created_at,id LIMIT ?"
    : "SELECT * FROM schedules WHERE server_id=? AND owner_id=? ORDER BY created_at,id LIMIT ?";
  const rows = current.role === "admin"
    ? getDatabase().prepare(query).all(serverId, MAX_SCHEDULES_PER_SERVER)
    : getDatabase()
        .prepare(query)
        .all(serverId, current.id, MAX_SCHEDULES_PER_SERVER);
  const now = Date.now();
  return (rows as unknown as ScheduleRow[]).map((row) => publicSchedule(row, now));
}
function assertPayloadSize(input: unknown): void {
  const inputBytes = new TextEncoder().encode(JSON.stringify(input) ?? "").byteLength;
  if (inputBytes > MAX_SCHEDULE_PAYLOAD_BYTES)
    throw new AppError(
      "SCHEDULE_PAYLOAD_TOO_LARGE",
      413,
      `Schedule payloads cannot exceed ${MAX_SCHEDULE_PAYLOAD_BYTES} bytes`,
    );
}
export function createSchedule(
  actor: SessionUser,
  serverId: string,
  input: unknown,
) {
  assertPayloadSize(input);
  const data = scheduleSchema.parse(input);
  assertServerCapability(actor, serverId, "schedules.manage");
  assertServerCapability(actor, serverId, actionCapability(data.action));
  if (actor.id === "api-token")
    throw new AppError(
      "SCHEDULE_OWNER_REQUIRED",
      400,
      "Sign in as a user to own a schedule",
    );
  const binding = resolveServerBinding(serverId);
  const id = crypto.randomUUID();
  const db = getDatabase();
  db.exec("BEGIN IMMEDIATE");
  try {
    // Check the global boundary first. If an older database already exceeds it,
    // this query still stops after a bounded number of rows.
    const globalLimitReached = db
      .prepare("SELECT 1 AS present FROM schedules ORDER BY rowid LIMIT 1 OFFSET ?")
      .get(MAX_SCHEDULES_TOTAL - 1);
    if (globalLimitReached)
      throw new AppError(
        "SCHEDULE_LIMIT_REACHED",
        409,
        `Ludock supports up to ${MAX_SCHEDULES_TOTAL} schedules`,
      );
    const count = db
      .prepare("SELECT COUNT(*) AS count FROM schedules WHERE server_id=?")
      .get(serverId) as { count: number };
    if (count.count >= MAX_SCHEDULES_PER_SERVER)
      throw new AppError(
        "SCHEDULE_LIMIT_REACHED",
        409,
        `A server can have up to ${MAX_SCHEDULES_PER_SERVER} schedules`,
      );
    db.prepare(
      "INSERT INTO schedules(id,server_id,owner_id,input_json,binding_revision,created_at) VALUES(?,?,?,?,?,?)",
    ).run(
      id,
      serverId,
      actor.id,
      JSON.stringify(data),
      binding.bindingRevision,
      Date.now(),
    );
    writeAuditLog(
      {
        userId: actor.id,
        action: "schedule.created",
        targetType: "server",
        targetId: serverId,
        details: { scheduleId: id, action: data.action },
      },
      { prune: false },
    );
    db.exec("COMMIT");
  } catch (error) {
    try {
      db.exec("ROLLBACK");
    } catch {
      // A failed commit may already have ended the transaction. Keep its cause.
    }
    throw error;
  }
  try {
    pruneAuditLogIfNeeded();
  } catch {
    logger.warn("Audit retention cleanup failed after schedule creation; it will be retried");
  }
  return publicSchedule(
    db.prepare("SELECT * FROM schedules WHERE id=?").get(id) as ScheduleRow,
  );
}
export function deleteSchedule(
  actor: SessionUser,
  serverId: string,
  id: string,
): void {
  assertServerCapability(actor, serverId, "schedules.manage");
  const row = getDatabase()
    .prepare("SELECT * FROM schedules WHERE id=? AND server_id=?")
    .get(id, serverId) as ScheduleRow | null;
  const current = currentActor(actor)!;
  if (!row || (current.role !== "admin" && row.owner_id !== current.id))
    throw new AppError("NOT_FOUND", 404, "Schedule not found");
  getDatabase().prepare("DELETE FROM schedules WHERE id=?").run(id);
  writeAuditLog({
    userId: actor.id === "api-token" ? undefined : actor.id,
    action: "schedule.deleted",
    targetType: "server",
    targetId: serverId,
    details: { scheduleId: id },
  });
}

function mutateSchedule(
  actor: SessionUser,
  serverId: string,
  id: string,
  expectedRevision: number,
  update: ScheduleInput | boolean,
) {
  assertServerCapability(actor, serverId, "schedules.manage");
  const current = currentActor(actor)!;
  const db = getDatabase();
  let result: ScheduleRow;
  let changed: boolean;
  db.exec("BEGIN IMMEDIATE");
  try {
    const row = db
      .prepare("SELECT * FROM schedules WHERE id=? AND server_id=?")
      .get(id, serverId) as ScheduleRow | null;
    if (!row || (current.role !== "admin" && row.owner_id !== current.id))
      throw new AppError("NOT_FOUND", 404, "Schedule not found");
    if (row.revision !== expectedRevision)
      throw new AppError(
        "SCHEDULE_CHANGED",
        409,
        "This schedule changed. Reload it before trying again.",
      );
    const previous = scheduleSchema.parse(JSON.parse(row.input_json));
    const data = typeof update === "boolean"
      ? { ...previous, enabled: update }
      : update;
    // Pausing must remain possible after the action grant or original binding
    // has been revoked. Editing/resuming requires both requester and owner access.
    if (update !== false) {
      assertServerCapability(actor, serverId, actionCapability(data.action));
      scheduleAuthority(row, data);
    }
    changed = previous.action !== data.action ||
      previous.enabled !== data.enabled ||
      previous.time !== data.time ||
      previous.timezone !== data.timezone ||
      previous.days.some((day) => !data.days.includes(day)) ||
      data.days.some((day) => !previous.days.includes(day));
    result = row;
    if (changed) {
      const inputJson = JSON.stringify(data);
      // Keep the original binding baseline, consumed clock slot, and history.
      // Every mutation invalidates work queued from an earlier configuration.
      db.prepare("UPDATE schedules SET input_json=?,revision=revision+1 WHERE id=?")
        .run(inputJson, id);
      result = { ...row, input_json: inputJson, revision: row.revision + 1 };
      writeAuditLog({
        userId: current.id === "api-token" ? undefined : current.id,
        action: typeof update === "boolean"
          ? update ? "schedule.resumed" : "schedule.paused"
          : "schedule.updated",
        targetType: "server",
        targetId: serverId,
        details: { scheduleId: id, action: data.action, revision: result.revision },
      }, { prune: false });
    }
    db.exec("COMMIT");
  } catch (error) {
    try {
      db.exec("ROLLBACK");
    } catch {
      // Keep the original failure if SQLite has already rolled back.
    }
    throw error;
  }
  if (changed) {
    try {
      pruneAuditLogIfNeeded();
    } catch {
      logger.warn("Audit retention cleanup failed after schedule update; it will be retried");
    }
  }
  return publicSchedule(result);
}

export function updateSchedule(
  actor: SessionUser,
  serverId: string,
  id: string,
  input: unknown,
) {
  assertPayloadSize(input);
  const { revision, ...data } = updateScheduleRequestSchema.parse(input);
  return mutateSchedule(actor, serverId, id, revision, data);
}

export function setScheduleEnabled(
  actor: SessionUser,
  serverId: string,
  id: string,
  input: unknown,
) {
  assertPayloadSize(input);
  const { revision, enabled } = scheduleEnabledRequestSchema.parse(input);
  return mutateSchedule(actor, serverId, id, revision, enabled);
}
export function runSchedules(now = Date.now()): void {
  const rows = getDatabase()
    .prepare("SELECT * FROM schedules ORDER BY rowid LIMIT ?")
    .all(MAX_SCHEDULES_TOTAL + 1) as unknown as ScheduleRow[];
  if (rows.length > MAX_SCHEDULES_TOTAL)
    logger.warn("Schedule limit exceeded; schedules beyond the limit are not evaluated", {
      limit: MAX_SCHEDULES_TOTAL,
    });
  for (const row of rows.slice(0, MAX_SCHEDULES_TOTAL)) {
    let slot: string | null = null;
    let configurationValid = false;
    try {
      const data = scheduleSchema.parse(JSON.parse(row.input_json));
      if (!data.enabled) continue;
      slot = scheduleSlot(data, now);
      configurationValid = true;
      if (!slot || slot === row.last_slot) continue;
      getDatabase()
        .prepare("UPDATE schedules SET last_slot=? WHERE id=?")
        .run(slot, row.id);
      const { user, binding } = scheduleAuthority(row, data);
      const op = enqueueOperation({
        serverId: row.server_id,
        actorId: user.id,
        kind: data.action,
        bindingRevision: binding.bindingRevision,
        input: { scheduleId: row.id, scheduleRevision: row.revision },
        idempotencyKey: `schedule:${row.id}:${slot}`,
      });
      getDatabase()
        .prepare("UPDATE schedules SET last_result=? WHERE id=?")
        .run(`Queued operation ${op.id}`, row.id);
    } catch (error) {
      const reason = !configurationValid
        ? "Suspended: saved schedule configuration is invalid; recreate this schedule"
        : error instanceof AppError &&
        (error.code === "SCHEDULE_REVOKED" ||
          error.code === "SCHEDULE_BINDING_CHANGED")
          ? error.message
          : "Skipped: server state or a conflicting operation prevented this run";
      try {
        getDatabase()
          .prepare("UPDATE schedules SET last_result=? WHERE id=?")
          .run(reason, row.id);
        if (!configurationValid && row.last_result !== reason)
          logger.warn("Saved schedule configuration is invalid; recreate the schedule", {
            scheduleId: row.id,
          });
        notifyEvent(
          `schedule:${row.id}:${slot ?? "invalid"}`,
          "A scheduled Ludock action could not run. Review its server and permissions.",
        );
      } catch {
        // Diagnostics for one row must not prevent unrelated schedules running.
        logger.warn("Could not record or notify a skipped schedule run", {
          scheduleId: row.id,
        });
      }
    }
  }
}
