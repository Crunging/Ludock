import {
  nextScheduleRun,
  operationSchema,
  scheduleEnabledRequestSchema,
  scheduleSchema,
  scheduleSlot,
  updateScheduleRequestSchema,
  type NextRunUnavailableReason,
  type Operation,
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
  getUserServerGrant,
} from "./authorization.js";
import { getLogicalServer, resolveServerBinding, ServerBindingError } from "./identity.js";
import { enqueueOperation, getOperation, publicOperation } from "./operations.js";
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
  last_operation_id: string | null;
  last_run_at: number | null;
  created_at: number;
}

export const MAX_SCHEDULE_PAYLOAD_BYTES = 4 * 1024;
export const MAX_SCHEDULES_PER_SERVER = 100;
export const MAX_SCHEDULES_TOTAL = 1_000;

const actionCapability = (action: ScheduleInput["action"]): ServerCapability =>
  action === "backup" ? "backups.create" : `server.${action}`;

const unavailableMessages = {
  owner_missing: "Suspended: the schedule owner no longer exists",
  owner_disabled: "Suspended: the schedule owner's account is disabled",
  owner_access_removed: "Suspended: required access to manage schedules has been removed",
  action_access_removed: "Suspended: required access for this scheduled action has been removed",
  binding_changed: "Suspended: server configuration changed; recreate this schedule after reviewing the server",
  binding_unavailable: "Suspended: the server binding is unavailable or requires administrator review",
  unavailable: "Suspended: this schedule cannot currently run; review the server and schedule",
} satisfies Record<NextRunUnavailableReason, string>;

class ScheduleUnavailableError extends AppError {
  constructor(readonly reason: NextRunUnavailableReason) {
    const binding = reason === "binding_changed" || reason === "binding_unavailable";
    super(
      reason === "binding_changed" ? "SCHEDULE_BINDING_CHANGED"
        : binding ? "SCHEDULE_BINDING_UNAVAILABLE" : "SCHEDULE_REVOKED",
      binding ? 409 : 403,
      unavailableMessages[reason],
    );
  }
}

function scheduleAuthority(row: ScheduleRow, data: ScheduleInput) {
  const user = findUserById(row.owner_id);
  if (!user) throw new ScheduleUnavailableError("owner_missing");
  if (user.disabled) throw new ScheduleUnavailableError("owner_disabled");
  // Inspect assigned grants before binding availability so a suspended binding
  // is not misreported as a removed grant by the effective-capability mask.
  if (user.role !== "admin") {
    const grant = getUserServerGrant(user.id, row.server_id);
    if (user.role !== "operator" ||
      !grant?.capabilities.includes("server.view") ||
      !grant.capabilities.includes("schedules.manage"))
      throw new ScheduleUnavailableError("owner_access_removed");
    if (!grant.capabilities.includes(actionCapability(data.action)))
      throw new ScheduleUnavailableError("action_access_removed");
  }
  const logical = getLogicalServer(row.server_id);
  if (!logical) throw new ScheduleUnavailableError("binding_unavailable");
  const original = getDatabase()
    .query(
      "SELECT binding_fingerprint FROM server_bindings WHERE server_id=? AND binding_revision=? AND accepted=1 ORDER BY id DESC LIMIT 1",
    )
    .get(row.server_id, row.binding_revision) as
    | { binding_fingerprint: string }
    | undefined;
  if (!original || original.binding_fingerprint !== logical.bindingFingerprint)
    throw new ScheduleUnavailableError("binding_changed");
  let binding: ReturnType<typeof resolveServerBinding>;
  try {
    binding = resolveServerBinding(row.server_id);
  } catch (error) {
    if (error instanceof ServerBindingError)
      throw new ScheduleUnavailableError("binding_unavailable");
    throw error;
  }
  // Keep the normal current role, grant, and binding authority as the final gate.
  if (!hasServerCapability(user, row.server_id, "schedules.manage"))
    throw new ScheduleUnavailableError("owner_access_removed");
  if (!hasServerCapability(user, row.server_id, actionCapability(data.action)))
    throw new ScheduleUnavailableError("action_access_removed");
  return { user, binding };
}

type PreviewAuthority = (row: ScheduleRow, data: ScheduleInput) => void;

function createPreviewAuthority(): PreviewAuthority {
  // Reuse only complete outcomes within one synchronous list serialization.
  // Execution and mutation paths always call scheduleAuthority afresh.
  const outcomes = new Map<string, { error: unknown } | null>();
  return (row, data) => {
    const key = JSON.stringify([row.owner_id, row.server_id, row.binding_revision, data.action]);
    const cached = outcomes.get(key);
    if (cached !== undefined) {
      if (cached !== null) throw cached.error;
      return;
    }
    try {
      scheduleAuthority(row, data);
      outcomes.set(key, null);
    } catch (error) {
      outcomes.set(key, { error });
      throw error;
    }
  };
}

function latestOperation(row: ScheduleRow): Operation | null {
  if (!row.last_operation_id) return null;
  try {
    const job = getOperation(row.last_operation_id);
    if (!job || job.serverId !== row.server_id || job.actorId !== row.owner_id ||
      job.input.scheduleId !== row.id) return null;
    const result = operationSchema.safeParse(publicOperation(job));
    return result.success ? result.data : null;
  } catch {
    // Corrupt or unrelated history never exposes an operation's private state.
    return null;
  }
}

function publicSchedule(
  row: ScheduleRow,
  now = Date.now(),
  checkAuthority: PreviewAuthority = scheduleAuthority,
) {
  const data = scheduleSchema.parse(JSON.parse(row.input_json));
  let nextRunAt: number | null = null;
  let nextRunUnavailableReason: NextRunUnavailableReason | null = null;
  if (data.enabled) {
    try {
      checkAuthority(row, data);
      nextRunAt = nextScheduleRun(data, now, row.last_slot);
      if (nextRunAt === null) nextRunUnavailableReason = "unavailable";
    } catch (error) {
      nextRunUnavailableReason = error instanceof ScheduleUnavailableError
        ? error.reason : "unavailable";
    }
  }
  return {
    ...data,
    id: row.id,
    serverId: row.server_id,
    ownerId: row.owner_id,
    lastResult: row.last_result,
    lastOperation: latestOperation(row),
    lastRunAt: row.last_run_at,
    lastSlot: row.last_slot,
    revision: row.revision,
    nextRunAt,
    nextRunUnavailableReason,
  };
}
export function listSchedules(actor: SessionUser, serverId: string) {
  assertServerCapability(actor, serverId, "schedules.manage");
  const current = currentActor(actor)!;
  const query = current.role === "admin"
    ? "SELECT * FROM schedules WHERE server_id=? ORDER BY created_at,id LIMIT ?"
    : "SELECT * FROM schedules WHERE server_id=? AND owner_id=? ORDER BY created_at,id LIMIT ?";
  const rows = current.role === "admin"
    ? getDatabase().query(query).all(serverId, MAX_SCHEDULES_PER_SERVER)
    : getDatabase()
        .query(query)
        .all(serverId, current.id, MAX_SCHEDULES_PER_SERVER);
  const now = Date.now();
  const checkAuthority = createPreviewAuthority();
  return (rows as unknown as ScheduleRow[]).map((row) => publicSchedule(row, now, checkAuthority));
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
      .query("SELECT 1 AS present FROM schedules ORDER BY rowid LIMIT 1 OFFSET ?")
      .get(MAX_SCHEDULES_TOTAL - 1);
    if (globalLimitReached)
      throw new AppError(
        "SCHEDULE_LIMIT_REACHED",
        409,
        `Ludock supports up to ${MAX_SCHEDULES_TOTAL} schedules`,
      );
    const count = db
      .query("SELECT COUNT(*) AS count FROM schedules WHERE server_id=?")
      .get(serverId) as { count: number };
    if (count.count >= MAX_SCHEDULES_PER_SERVER)
      throw new AppError(
        "SCHEDULE_LIMIT_REACHED",
        409,
        `A server can have up to ${MAX_SCHEDULES_PER_SERVER} schedules`,
      );
    db.query(
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
    db.query("SELECT * FROM schedules WHERE id=?").get(id) as ScheduleRow,
  );
}
export function deleteSchedule(
  actor: SessionUser,
  serverId: string,
  id: string,
): void {
  assertServerCapability(actor, serverId, "schedules.manage");
  const row = getDatabase()
    .query("SELECT * FROM schedules WHERE id=? AND server_id=?")
    .get(id, serverId) as ScheduleRow | null;
  const current = currentActor(actor)!;
  if (!row || (current.role !== "admin" && row.owner_id !== current.id))
    throw new AppError("NOT_FOUND", 404, "Schedule not found");
  getDatabase().query("DELETE FROM schedules WHERE id=?").run(id);
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
      .query("SELECT * FROM schedules WHERE id=? AND server_id=?")
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
      db.query("UPDATE schedules SET input_json=?,revision=revision+1 WHERE id=?")
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
    .query("SELECT * FROM schedules ORDER BY rowid LIMIT ?")
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
      const db = getDatabase();
      db.exec("BEGIN IMMEDIATE");
      try {
        const { user, binding } = scheduleAuthority(row, data);
        const op = enqueueOperation({
          serverId: row.server_id,
          actorId: user.id,
          kind: data.action,
          bindingRevision: binding.bindingRevision,
          input: { scheduleId: row.id, scheduleRevision: row.revision },
          idempotencyKey: `schedule:${row.id}:${slot}`,
          deferAuditPrune: true,
        });
        // Queue and associate the same attempt atomically. Completing an older
        // operation never changes this association or a newer attempt's result.
        db.query(
          "UPDATE schedules SET last_slot=?,last_run_at=?,last_operation_id=?,last_result=? WHERE id=?",
        ).run(slot, now, op.id, `Queued operation ${op.id}`, row.id);
        db.exec("COMMIT");
      } catch (error) {
        try {
          db.exec("ROLLBACK");
        } catch {
          // Preserve the cause if SQLite already rolled the transaction back.
        }
        throw error;
      }
      try {
        pruneAuditLogIfNeeded();
      } catch {
        logger.warn("Audit retention cleanup failed after scheduling; it will be retried");
      }
    } catch (error) {
      const reason = !configurationValid
        ? "Suspended: saved schedule configuration is invalid; recreate this schedule"
        : error instanceof ScheduleUnavailableError
          ? error.message
          : "Skipped: server state or a conflicting operation prevented this run";
      try {
        if (slot) {
          getDatabase().query(
            "UPDATE schedules SET last_slot=?,last_run_at=?,last_operation_id=NULL,last_result=? WHERE id=?",
          ).run(slot, now, reason, row.id);
        } else {
          getDatabase().query("UPDATE schedules SET last_result=? WHERE id=?")
            .run(reason, row.id);
        }
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
