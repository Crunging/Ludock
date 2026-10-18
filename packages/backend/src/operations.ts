import type { Operation } from "@ludock/shared";
import { getDatabase, findUserById, writeAuditLog } from "./database.js";
import { publicHistoryActor, publicOperationActorId } from "./auth.js";
import { AppError, publicError } from "./errors.js";
import { notifyEvent } from "./notifications.js";
import { acquireLocks, BackgroundLockConflict, nextLockRelease } from "./operation-locks.js";

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
  /** Persist recovery and an optional public result atomically with the phase. */
  progress: (phase: string, recovery?: Record<string, unknown>, result?: Record<string, unknown>) => void;
}
interface JobHandler {
  run: (context: JobContext) => Promise<Record<string, unknown> | void>;
  recover?: (context: JobContext) => Promise<Record<string, unknown> | void>;
}
const handlers = new Map<string, JobHandler>();
let enabled = false;
let pending: ReturnType<typeof setTimeout> | null = null;
let starting: Promise<void> | null = null;

/** Start, stop, and plain restart are short and never touch backup storage,
 * so they run beside each other and beside one data operation. Everything
 * else, including unknown kinds, runs one at a time. */
type Lane = "lifecycle" | "data";
const LANE_CAPACITY: Record<Lane, number> = { lifecycle: 4, data: 1 };
const laneLoad: Record<Lane, number> = { lifecycle: 0, data: 0 };
const claimed = new Set<string>();
/** Jobs that found their resources held by another job. No new work starts
 * until each retries, so a job never waits longer than serial execution. */
const yielded = new Set<string>();
const inFlight = new Set<Promise<void>>();
let jobSettled: { promise: Promise<void>; resolve: () => void } | undefined;

class ResourcesBusy extends Error {
  /** holdsQueue: the holder is another job, so no new work starts meanwhile. */
  constructor(readonly released: Promise<void>, readonly holdsQueue = true) {
    super("Operation resources are busy");
  }
}

function parseObject(json: string): Record<string, unknown> {
  try {
    const value: unknown = JSON.parse(json);
    if (value && typeof value === "object" && !Array.isArray(value))
      return value as Record<string, unknown>;
  } catch {
    /* Invalid persisted JSON receives the same safe diagnostic. */
  }
  throw new AppError(
    "INVALID_OPERATION_STATE",
    409,
    "Saved operation state is invalid; administrator review is required",
  );
}
function jobIdentity(row: OperationRow) {
  return {
    id: row.id,
    serverId: row.server_id,
    kind: row.kind,
    actorId: row.actor_id,
  };
}
function toJob(row: OperationRow): Job {
  return {
    id: row.id,
    serverId: row.server_id,
    kind: row.kind,
    status: row.status,
    phase: row.phase,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
    error: row.error,
    result: row.result_json ? parseObject(row.result_json) : null,
    actorId: row.actor_id,
    bindingRevision: row.binding_revision,
    input: parseObject(row.input_json),
    recovery: parseObject(row.recovery_json),
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
    actor: publicHistoryActor(
      job.actorId,
      findUserById(job.actorId)?.username ?? null,
    ),
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
    .query("SELECT * FROM operations WHERE id = ?")
    .get(id) as OperationRow | null;
  return row ? toJob(row) : null;
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
  /** The caller owns a transaction and prunes only after committing it. */
  deferAuditPrune?: boolean;
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
      .query("SELECT * FROM operations WHERE request_key = ?")
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
    .query(
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
    .query(
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
  writeAuditLog(
    {
      userId: options.actorId,
      action: `server.${options.kind}.queued`,
      targetType: "server",
      targetId: options.serverId,
      details: { operationId: id },
    },
    { prune: !options.deferAuditPrune },
  );
  schedule();
  return publicOperation(getOperation(id)!);
}
function context(job: Job): JobContext {
  return {
    job,
    progress: (phase, recovery, result) => {
      if (recovery) job.recovery = { ...job.recovery, ...recovery };
      if (result) job.result = result;
      job.phase = phase;
      getDatabase()
        .query(
          "UPDATE operations SET phase=?, recovery_json=?, result_json=?, updated_at=? WHERE id=?",
        )
        .run(phase, JSON.stringify(job.recovery), job.result ? JSON.stringify(job.result) : null, Date.now(), job.id);
    },
  };
}
function finish(
  job: Pick<Job, "id" | "serverId" | "kind" | "actorId">,
  status: Operation["status"],
  result: Record<string, unknown> | null,
  error: string | null,
) {
  getDatabase()
    .query(
      "UPDATE operations SET status=?,phase=?,result_json=COALESCE(?,result_json),error=?,updated_at=? WHERE id=?",
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
    // The owner may have been deleted since queueing.
    userId: findUserById(job.actorId) ? job.actorId : undefined,
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
function notifyScheduledFailure(row: OperationRow): void {
  let input: Record<string, unknown>;
  try {
    input = parseObject(row.input_json);
  } catch {
    return;
  }
  if (typeof input.scheduleId !== "string") return;
  // The backup handler may already have recorded its failure. Share that event
  // key while covering failures before the handler reaches its notification.
  notifyEvent(
    row.kind === "backup" ? `backup-failed:${row.id}` : `schedule-failed:${row.id}`,
    "A scheduled Ludock action failed. Review its operation details.",
  );
}
/** Acquire a job's resources. Handlers call this before any effect: if
 * another job holds them, this job returns to the queue and runs again from
 * the start once they are released, as it would have run after that job.
 * A job cancels background work holding its resources and waits for its
 * cleanup without holding back other jobs. Conflicts with requests outside
 * the runner fail as before. */
export async function withJobLocks<T>(
  context: JobContext,
  keys: readonly string[],
  action: () => Promise<T> | T,
): Promise<T> {
  const id = context.job.id;
  let release: () => void;
  try {
    release = acquireLocks(keys);
  } catch (error) {
    if (claimed.has(id) && error instanceof BackgroundLockConflict) {
      const released = nextLockRelease();
      error.cancel();
      if (yielded.delete(id)) schedule();
      throw new ResourcesBusy(released, false);
    }
    if (claimed.has(id) && error instanceof AppError && error.code === "OPERATION_CONFLICT" &&
        [...claimed].some((other) => other !== id && !yielded.has(other)))
      throw new ResourcesBusy(nextLockRelease());
    if (yielded.delete(id)) schedule();
    throw error;
  }
  if (yielded.delete(id)) schedule();
  try {
    return await action();
  } finally {
    release();
  }
}
function laneFor(row: OperationRow): Lane {
  if (row.kind === "start" || row.kind === "stop") return "lifecycle";
  if (row.kind !== "restart") return "data";
  try {
    return parseObject(row.input_json).createBackup === true ? "data" : "lifecycle";
  } catch {
    return "data";
  }
}
function schedule() {
  if (!enabled || starting || pending) return;
  pending = setTimeout(() => {
    pending = null;
    fill();
  }, 0);
}
function fill() {
  if (!enabled || yielded.size) return;
  const rows = getDatabase()
    .query("SELECT * FROM operations WHERE status='queued' ORDER BY created_at,id")
    .all() as unknown as OperationRow[];
  for (const row of rows) {
    const lane = laneFor(row);
    if (claimed.has(row.id) || laneLoad[lane] >= LANE_CAPACITY[lane]) continue;
    claimed.add(row.id);
    laneLoad[lane]++;
    const run = runJob(row).finally(() => {
      claimed.delete(row.id);
      yielded.delete(row.id);
      laneLoad[lane]--;
      inFlight.delete(run);
      jobSettled?.resolve();
      jobSettled = undefined;
      schedule();
    });
    inFlight.add(run);
  }
}
function nextJobSettled(): Promise<void> {
  jobSettled ??= Promise.withResolvers<void>();
  return jobSettled.promise;
}
async function runJob(initial: OperationRow) {
  let row = initial;
  try {
    while (true) {
      // Damaged saved state must fail this job without wedging the runner.
      const job = toJob(row);
      getDatabase()
        .query(
          "UPDATE operations SET status='running', phase='validating', updated_at=? WHERE id=?",
        )
        .run(Date.now(), job.id);
      const handler = handlers.get(job.kind);
      if (!handler)
        throw new AppError(
          "UNSUPPORTED_OPERATION",
          409,
          "This operation type is unavailable",
        );
      try {
        const result = (await handler.run(context(job))) ?? {};
        finish(
          job,
          result.alreadyCurrent ? "already_current" : "succeeded",
          result,
          null,
        );
        return;
      } catch (error) {
        if (!(error instanceof ResourcesBusy)) throw error;
        // Nothing has changed yet. Keep the job queued, so a restart while it
        // waits runs it normally instead of reconciling an interruption.
        getDatabase()
          .query("UPDATE operations SET status='queued', phase='queued', updated_at=? WHERE id=?")
          .run(Date.now(), job.id);
        if (error.holdsQueue) yielded.add(job.id);
        await Promise.race([error.released, nextJobSettled()]);
        const current = getDatabase()
          .query("SELECT * FROM operations WHERE id = ?")
          .get(job.id) as OperationRow | null;
        if (!enabled || current?.status !== "queued") return;
        row = current;
      }
    }
  } catch (error) {
    finish(jobIdentity(row), "failed", null, publicError(error).error);
    notifyScheduledFailure(row);
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
    .query("SELECT * FROM operations WHERE status='running'")
    .all() as unknown as OperationRow[];
  for (const row of rows) {
    try {
      const job = toJob(row);
      const result = await handlers.get(job.kind)?.recover?.(context(job));
      finish(
        job,
        "interrupted",
        result ?? null,
        "Ludock restarted during this operation. Review the server state before retrying.",
      );
    } catch {
      finish(
        jobIdentity(row),
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
  // Waiting jobs stay queued for the next start.
  jobSettled?.resolve();
  jobSettled = undefined;
  while (inFlight.size) await Promise.allSettled([...inFlight]);
}
