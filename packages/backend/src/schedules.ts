import { scheduleSchema, type ScheduleInput } from "@ludock/shared";
import {
  getDatabase,
  findUserById,
  writeAuditLog,
  type SessionUser,
} from "./database.js";
import {
  assertServerCapability,
  hasServerCapability,
} from "./authorization.js";
import { resolveServerBinding } from "./identity.js";
import { enqueueOperation } from "./operations.js";
import { AppError } from "./errors.js";
import { notifyEvent } from "./notifications.js";
import type { ServerCapability } from "@ludock/shared";

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
  return (
    getDatabase()
      .prepare("SELECT * FROM schedules WHERE server_id=? ORDER BY created_at")
      .all(serverId) as unknown as ScheduleRow[]
  )
    .filter((row) => actor.role === "admin" || row.owner_id === actor.id)
    .map(publicSchedule);
}
export function createSchedule(
  actor: SessionUser,
  serverId: string,
  input: unknown,
) {
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
  getDatabase()
    .prepare(
      "INSERT INTO schedules(id,server_id,owner_id,input_json,binding_revision,created_at) VALUES(?,?,?,?,?,?)",
    )
    .run(
      id,
      serverId,
      actor.id,
      JSON.stringify(data),
      binding.bindingRevision,
      Date.now(),
    );
  writeAuditLog({
    userId: actor.id,
    action: "schedule.created",
    targetType: "server",
    targetId: serverId,
    details: { scheduleId: id, action: data.action },
  });
  return publicSchedule(
    getDatabase()
      .prepare("SELECT * FROM schedules WHERE id=?")
      .get(id) as ScheduleRow,
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
  if (!row || (actor.role !== "admin" && row.owner_id !== actor.id))
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
    .prepare("SELECT * FROM schedules")
    .all() as unknown as ScheduleRow[];
  for (const row of rows) {
    const data = scheduleSchema.parse(JSON.parse(row.input_json));
    if (!data.enabled) continue;
    const slot = scheduleSlot(data, now);
    if (!slot || slot === row.last_slot) continue;
    getDatabase()
      .prepare("UPDATE schedules SET last_slot=? WHERE id=?")
      .run(slot, row.id);
    try {
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
      const reason =
        error instanceof AppError &&
        (error.code === "SCHEDULE_REVOKED" ||
          error.code === "SCHEDULE_BINDING_CHANGED")
          ? error.message
          : "Skipped: server state or a conflicting operation prevented this run";
      getDatabase()
        .prepare("UPDATE schedules SET last_result=? WHERE id=?")
        .run(reason, row.id);
      notifyEvent(
        `schedule:${row.id}:${slot}`,
        "A scheduled Ludock action could not run. Review its server and permissions.",
      );
    }
  }
}
