import type { Operation } from "@ludock/shared";
import { getDatabase, findUserById, writeAuditLog } from "./database.js";
import { isApiTokenOperationActor, publicOperationActorId } from "./auth.js";
import { AppError, publicError } from "./errors.js";

interface OperationRow {
  id: string;
  server_id: string;
  actor_id: string;
  kind: string;
  status: Operation["status"];
  phase: string;
  input_json: string;
  recovery_json: string;
  binding_revision: number;
  created_at: number;
  updated_at: number;
  error: string | null;
  result_json: string | null;
}
export interface Job extends Operation {
  actorId: string;
  input: Record<string, unknown>;
  recovery: Record<string, unknown>;
  bindingRevision: number;
}
export interface JobContext {
  job: Job;
  progress: (phase: string, recovery?: Record<string, unknown>) => void;
}
export interface JobHandler {
  run: (context: JobContext) => Promise<Record<string, unknown> | void>;
  recover?: (context: JobContext) => Promise<void>;
}
const handlers = new Map<string, JobHandler>();
let running = false;
let enabled = false;
let pending: ReturnType<typeof setTimeout> | null = null;
let settled: Promise<void> = Promise.resolve();
let starting: Promise<void> | null = null;

function parseObject(json: string): Record<string, unknown> {
  const value: unknown = JSON.parse(json);
  if (!value || typeof value !== "object" || Array.isArray(value))
    throw new AppError(
      "INVALID_OPERATION_STATE",
      409,
      "Saved operation state is invalid; administrator review is required",
    );
  return value as Record<string, unknown>;
}
function toJob(row: OperationRow): Job {
  return {
    id: row.id,
    serverId: row.server_id,
    actorId: row.actor_id,
    kind: row.kind,
    status: row.status,
    phase: row.phase,
    bindingRevision: row.binding_revision,
    input: parseObject(row.input_json),
    recovery: parseObject(row.recovery_json),
    createdAt: row.created_at,
    updatedAt: row.updated_at,
    error: row.error,
    result: row.result_json ? parseObject(row.result_json) : null,
  };
}
export function publicOperation(job: Job): Operation {
  const {
    id,
    serverId,
    kind,
    status,
    phase,
    createdAt,
    updatedAt,
    error,
    result,
  } = job;
  return {
    id,
    serverId,
    kind,
    status,
    phase,
    createdAt,
    updatedAt,
    error,
    result,
  };
}
export function getOperation(id: string): Job | null {
  const row = getDatabase()
    .prepare("SELECT * FROM operations WHERE id = ?")
    .get(id) as OperationRow | null;
  return row ? toJob(row) : null;
}
export function listOperations(serverId: string): Operation[] {
  return (
    getDatabase()
      .prepare(
        "SELECT * FROM operations WHERE server_id = ? ORDER BY created_at DESC LIMIT 100",
      )
      .all(serverId) as unknown as OperationRow[]
  ).map((row) => publicOperation(toJob(row)));
}
export function registerJobHandler(kind: string, handler: JobHandler): void {
  handlers.set(kind, handler);
}
export function enqueueOperation(options: {
  serverId: string;
  actorId: string;
  kind: string;
  bindingRevision: number;
  input?: Record<string, unknown>;
  idempotencyKey?: string;
}): Operation {
  const input = JSON.stringify(options.input ?? {});
  const key = options.idempotencyKey
    ? new Bun.CryptoHasher("sha256")
        .update(
          `${options.actorId}:${options.serverId}:${options.kind}:${options.idempotencyKey}`,
        )
        .digest("hex")
    : null;
  if (key) {
    const prior = getDatabase()
      .prepare("SELECT * FROM operations WHERE request_key = ?")
      .get(key) as OperationRow | null;
    if (prior) {
      if (prior.input_json !== input)
        throw new AppError(
          "IDEMPOTENCY_CONFLICT",
          409,
          "This request key was used for different operation settings",
        );
      return publicOperation(toJob(prior));
    }
  }
  const conflict = getDatabase()
    .prepare(
      "SELECT id FROM operations WHERE server_id = ? AND status IN ('queued','running')",
    )
    .get(options.serverId);
  if (conflict)
    throw new AppError(
      "OPERATION_CONFLICT",
      409,
      "An operation is already queued or running for this server",
    );
  const id = crypto.randomUUID();
  const now = Date.now();
  getDatabase()
    .prepare(
      `INSERT INTO operations (id,server_id,actor_id,kind,status,phase,input_json,binding_revision,request_key,created_at,updated_at) VALUES (?,?,?,?,'queued','queued',?,?,?,?,?)`,
    )
    .run(
      id,
      options.serverId,
      options.actorId,
      options.kind,
      input,
      options.bindingRevision,
      key,
      now,
      now,
    );
  writeAuditLog({
    userId: isApiTokenOperationActor(options.actorId)
      ? undefined
      : options.actorId,
    action: `server.${options.kind}.queued`,
    targetType: "server",
    targetId: options.serverId,
    details: { operationId: id },
  });
  schedule();
  return publicOperation(getOperation(id)!);
}
function context(job: Job): JobContext {
  return {
    job,
    progress: (phase, recovery) => {
      if (recovery) job.recovery = { ...job.recovery, ...recovery };
      job.phase = phase;
      getDatabase()
        .prepare(
          "UPDATE operations SET phase=?, recovery_json=?, updated_at=? WHERE id=?",
        )
        .run(phase, JSON.stringify(job.recovery), Date.now(), job.id);
    },
  };
}
function finish(
  job: Job,
  status: Operation["status"],
  result: Record<string, unknown> | null,
  error: string | null,
) {
  getDatabase()
    .prepare(
      "UPDATE operations SET status=?,phase=?,result_json=?,error=?,updated_at=? WHERE id=?",
    )
    .run(
      status,
      status,
      result ? JSON.stringify(result) : null,
      error,
      Date.now(),
      job.id,
    );
  writeAuditLog({
    userId:
      !isApiTokenOperationActor(job.actorId) && findUserById(job.actorId)
        ? job.actorId
        : undefined,
    action: `server.${job.kind}.${status}`,
    targetType: "server",
    targetId: job.serverId,
    details: {
      operationId: job.id,
      actorId: publicOperationActorId(job.actorId),
      ...(error ? { error } : {}),
    },
  });
}
function schedule() {
  if (!enabled || starting || running || pending) return;
  pending = setTimeout(() => {
    pending = null;
    settled = runNext();
  }, 0);
}
async function runNext() {
  if (!enabled || running) return;
  const row = getDatabase()
    .prepare(
      "SELECT * FROM operations WHERE status='queued' ORDER BY created_at,id LIMIT 1",
    )
    .get() as OperationRow | null;
  if (!row) return;
  running = true;
  const job = toJob(row);
  getDatabase()
    .prepare(
      "UPDATE operations SET status='running', phase='validating', updated_at=? WHERE id=?",
    )
    .run(Date.now(), job.id);
  try {
    const handler = handlers.get(job.kind);
    if (!handler)
      throw new AppError(
        "UNSUPPORTED_OPERATION",
        409,
        "This operation type is unavailable",
      );
    const result = (await handler.run(context(job))) ?? {};
    finish(
      job,
      result.alreadyCurrent ? "already_current" : "succeeded",
      result,
      null,
    );
  } catch (error) {
    finish(job, "failed", null, publicError(error).error);
  } finally {
    running = false;
    schedule();
  }
}
export async function startOperationRunner(): Promise<void> {
  if (starting) return starting;
  if (enabled) return;
  enabled = true;
  starting = recoverInterruptedOperations()
    .catch((error: unknown) => {
      enabled = false;
      throw error;
    })
    .finally(() => {
      starting = null;
      schedule();
    });
  return starting;
}
async function recoverInterruptedOperations(): Promise<void> {
  // Interrupted mutations are reconciled, never replayed.
  const rows = getDatabase()
    .prepare("SELECT * FROM operations WHERE status='running'")
    .all() as unknown as OperationRow[];
  for (const row of rows) {
    const job = toJob(row);
    try {
      await handlers.get(job.kind)?.recover?.(context(job));
      finish(
        job,
        "interrupted",
        null,
        "Ludock restarted during this operation. Review the server state before retrying.",
      );
    } catch {
      finish(
        job,
        "interrupted",
        null,
        "Recovery requires administrator attention. The server has been left stopped where possible.",
      );
    }
  }
}
export async function stopOperationRunner(): Promise<void> {
  enabled = false;
  if (pending) {
    clearTimeout(pending);
    pending = null;
  }
  // Recovery can still be restoring a stopped server when shutdown arrives.
  // Wait for it before allowing the database and Docker connections to close;
  // its completion must never re-enable queued work after this stop request.
  await starting?.catch(() => {});
  await settled;
}
