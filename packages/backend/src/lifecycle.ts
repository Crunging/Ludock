import type { ServerCapability } from "@ludock/shared";
import { assertServerCapability } from "./authorization.js";
import { changeContainerState, getContainer, type ContainerAction } from "./docker.js";
import { AppError } from "./errors.js";
import { resolveGameConsoleAdapter } from "./game-console.js";
import { executeGameCommand } from "./game-console-runtime.js";
import { assertObservedServerBinding } from "./identity.js";
import { jobActor } from "./jobs.js";
import { setIntentionalStop, suppressMonitoring } from "./monitoring.js";
import { withLocks } from "./operation-locks.js";
import type { JobContext } from "./operations.js";
import { resolveAuthorizedServer, type ServerContext } from "./servers.js";

const SAVE_SETTLE_MS = 5_000;
const FORCE_STOP_GUIDANCE = "Force-stopped, recent progress may not have been saved.";

function capability(action: string): ServerCapability {
  if (!["start", "stop", "restart"].includes(action))
    throw new AppError("INVALID_OPERATION_STATE", 409, "Invalid lifecycle action");
  return `server.${action}` as ServerCapability;
}
function lifecycleResult(context: JobContext, updates?: Record<string, unknown>) {
  const recovery = { ...context.job.recovery, ...updates };
  return {
    action: context.job.kind,
    ...(typeof recovery.saveStatus === "string"
      ? { saveStatus: recovery.saveStatus, saveReason: recovery.saveReason } : {}),
    ...(recovery.forceStopped === true
      ? { forceStopped: true, guidance: FORCE_STOP_GUIDANCE } : {}),
  };
}
function record(context: JobContext, phase: string, recovery?: Record<string, unknown>) {
  // Only these fixed lifecycle summaries are public; console output is discarded.
  context.progress(phase, recovery, lifecycleResult(context, recovery));
}

async function saveWorld(context: JobContext, server: ServerContext, assertAccess: () => void) {
  const expected = resolveGameConsoleAdapter({ gameType: server.container.gameType, labels: {} });
  const adapter = resolveGameConsoleAdapter(server.container);
  const command = adapter?.commands.find((item) => item.label === "Save world")?.command;
  if (!command) {
    record(context, "save_skipped", {
      saveStatus: "skipped", saveReason: expected?.commands.some((item) => item.label === "Save world")
        ? "console_unavailable" : "unsupported",
    });
    return;
  }
  if (server.container.state !== "running") {
    record(context, "save_skipped", { saveStatus: "skipped", saveReason: "not_running" });
    return;
  }
  record(context, "saving_world");
  try {
    // Console transports have bounded preparation and command deadlines. Exec
    // cleanup retains this operation's locks until Docker confirms termination.
    await executeGameCommand(getContainer(server.container.id), server.container, adapter, command,
      { stdout() {}, stderr() {}, system() {} }, assertAccess);
  } catch (error) {
    // Console failure is best effort; revoked authority must still stop the job.
    assertAccess();
    record(context, "save_skipped", {
      saveStatus: "skipped",
      saveReason: error instanceof Error && /timed out/i.test(error.message) ? "timed_out" : "console_unavailable",
    });
    return;
  }
  record(context, "waiting_for_save", { saveStatus: "sent", saveReason: null });
  await new Promise<void>((resolve) => setTimeout(resolve, SAVE_SETTLE_MS));
  assertAccess();
}

export async function runLifecycle(context: JobContext, action: ContainerAction) {
  const permission = capability(action);
  const resolve = () => resolveAuthorizedServer(jobActor(context), context.job.serverId, permission, context.job.bindingRevision);
  const initial = await resolve();
  return withLocks(initial.lockKeys, async () => {
    let server = await resolve();
    let accessFailure: Error | undefined;
    const assertAccess = () => {
      if (accessFailure) throw accessFailure;
      assertServerCapability(jobActor(context), server.logical.id, permission);
      assertObservedServerBinding(server.logical.id, server.observation, context.job.bindingRevision);
    };
    let revalidating: Promise<void> | undefined;
    const revalidate = setInterval(() => {
      if (revalidating || accessFailure) return;
      revalidating = resolve().then(() => {}, (error: unknown) => {
        accessFailure = error instanceof Error ? error : new Error("Operation access could not be verified");
      })
        .finally(() => { revalidating = undefined; });
    }, 1000);
    revalidate.unref();
    const mutate = async (mutation: "start" | "stop") => {
      assertAccess();
      server = await resolve();
      assertAccess();
      record(context, mutation === "start" ? "starting" : "stopping", {
        containerId: server.container.id,
        ...(mutation === "stop" ? { stopStarted: server.container.state === "running" } : {}),
      });
      await changeContainerState(server.container.id, mutation, (observation) => {
        assertServerCapability(jobActor(context), server.logical.id, permission);
        assertObservedServerBinding(server.logical.id, observation, context.job.bindingRevision);
      });
    };
    suppressMonitoring(server.logical.id);
    try {
      if (action !== "start") {
        await saveWorld(context, server, assertAccess);
        await mutate("stop");
        // Persist the actual stop before a restart clears Docker's exit code.
        setIntentionalStop(server.logical.id, action === "stop");
        server = await resolve();
        record(context, "verifying_stop", {
          forceStopped: context.job.recovery.stopStarted === true && server.container.exit?.code === 137,
        });
      }
      if (action !== "stop") {
        await mutate("start");
        setIntentionalStop(server.logical.id, false);
      }
      assertAccess();
      return lifecycleResult(context);
    } finally {
      clearInterval(revalidate);
      await revalidating;
      suppressMonitoring(server.logical.id);
    }
  });
}

export async function recoverLifecycle(context: JobContext) {
  const permission = capability(context.job.kind);
  const resolve = () => resolveAuthorizedServer(jobActor(context), context.job.serverId, permission, context.job.bindingRevision);
  const initial = await resolve();
  return withLocks(initial.lockKeys, async () => {
    const server = await resolve();
    // Observe only. An interrupted restart must never start or stop again.
    if (context.job.recovery.stopStarted === true &&
        context.job.recovery.containerId === server.container.id && server.container.exit) {
      setIntentionalStop(server.logical.id, context.job.kind === "stop");
      record(context, "reconciling", {
        forceStopped: context.job.recovery.forceStopped === true || server.container.exit.code === 137,
      });
    }
    suppressMonitoring(server.logical.id);
    return lifecycleResult(context);
  });
}
