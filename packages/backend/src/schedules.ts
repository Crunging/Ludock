import { scheduleSchema, type ScheduleInput } from "@ludock/shared";
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

interface ScheduleRow {
  id: string;
  server_id: string;
  owner_id: string;
  input_json: string;
  binding_revision: number;
  last_slot: string | null;
  last_result: string | null;
  created_at: number;
}

export const MAX_SCHEDULE_PAYLOAD_BYTES = 4 * 1024;
export const MAX_SCHEDULES_PER_SERVER = 100;
export const MAX_SCHEDULES_TOTAL = 1_000;

const actionCapability = (action: ScheduleInput["action"]): ServerCapability =>
  action === "backup" ? "backups.create" : `server.${action}`;
function publicSchedule(row: ScheduleRow) {
  return {
    ...scheduleSchema.parse(JSON.parse(row.input_json)),
    id: row.id,
    serverId: row.server_id,
    ownerId: row.owner_id,
    lastResult: row.last_result,
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
  return (rows as unknown as ScheduleRow[]).map(publicSchedule);
}
export function createSchedule(
  actor: SessionUser,
  serverId: string,
  input: unknown,
) {
  const inputBytes = new TextEncoder().encode(JSON.stringify(input) ?? "").byteLength;
  if (inputBytes > MAX_SCHEDULE_PAYLOAD_BYTES)
    throw new AppError(
      "SCHEDULE_PAYLOAD_TOO_LARGE",
      413,
      `Schedule payloads cannot exceed ${MAX_SCHEDULE_PAYLOAD_BYTES} bytes`,
    );
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
export function scheduleSlot(input: ScheduleInput, now: number): string | null {
  const parts = new Intl.DateTimeFormat("en-US", {
    timeZone: input.timezone,
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    weekday: "short",
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
  }).formatToParts(now);
  const part = (type: string) =>
    parts.find((p) => p.type === type)?.value ?? "";
  const day = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"].indexOf(
    part("weekday"),
  );
  if (
    !input.days.includes(day) ||
    `${part("hour")}:${part("minute")}` !== input.time
  )
    return null;
  // No offset in the slot: repeated fall-back clock time runs once. Spring-forward
  // gaps and missed slots are skipped; destructive actions are never caught up.
  return `${part("year")}-${part("month")}-${part("day")}T${input.time}:${input.timezone}`;
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
      if (
        !original ||
        original.binding_fingerprint !== binding.bindingFingerprint
      )
        throw new AppError(
          "SCHEDULE_BINDING_CHANGED",
          409,
          "Suspended: server configuration changed; recreate this schedule after reviewing the server",
        );
      const op = enqueueOperation({
        serverId: row.server_id,
        actorId: user.id,
        kind: data.action,
        bindingRevision: binding.bindingRevision,
        input: { scheduleId: row.id },
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
