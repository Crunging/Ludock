import {
  type ServerCapability,
  updateRequestSchema,
  scheduleSchema,
} from "@ludock/shared";
import { findUserById, getDatabase, type SessionUser } from "./database.js";
import { assertServerCapability } from "./authorization.js";
import {
  getDockerInstance,
  startContainer,
  stopContainer,
  restartContainer,
} from "./docker.js";
import { resolveAuthorizedServer, refreshServers } from "./servers.js";
import { withLocks } from "./operation-locks.js";
import { registerJobHandler, type JobContext } from "./operations.js";
import {
  runBackup,
  recoverBackup,
  runRestore,
  recoverRestore,
  createStoppedBackup,
  stopForDataOperation,
  restoreInitialRunningState,
} from "./backups.js";
import {
  composeSnapshotArgs,
  runCompose,
  validatedProject,
  assertSingleServiceContainer,
} from "./compose.js";
import { AppError } from "./errors.js";
import { notifyEvent } from "./notifications.js";
import { suppressMonitoring, setIntentionalStop } from "./monitoring.js";
import { ludockApiToken } from "./auth.js";

export function jobActor(context: JobContext): SessionUser {
  const id = context.job.actorId;
  const actor =
    id === "api-token" && ludockApiToken()
      ? { id, username: "API token", role: "admin" as const }
      : findUserById(id);
  if (!actor || ("disabled" in actor && actor.disabled))
    throw new AppError(
      "ACCESS_REVOKED",
      403,
      "The operation owner no longer has access",
    );
  if (typeof context.job.input.scheduleId === "string") {
    const schedule = getDatabase()
      .prepare(
        "SELECT owner_id,input_json FROM schedules WHERE id=? AND server_id=?",
      )
      .get(context.job.input.scheduleId, context.job.serverId) as
      | { owner_id: string; input_json: string }
      | undefined;
    const settings = schedule
      ? scheduleSchema.parse(JSON.parse(schedule.input_json))
      : null;
    if (
      !schedule ||
      schedule.owner_id !== id ||
      !settings?.enabled ||
      settings.action !== context.job.kind
    )
      throw new AppError(
        "SCHEDULE_REVOKED",
        403,
        "The schedule was deleted, disabled, or changed",
      );
    assertServerCapability(actor, context.job.serverId, "schedules.manage");
  }
  return actor;
}
async function lifecycle(
  context: JobContext,
  action: "start" | "stop" | "restart",
) {
  const actor = jobActor(context);
  const server = await resolveAuthorizedServer(
    actor,
    context.job.serverId,
    `server.${action}`,
    context.job.bindingRevision,
  );
  return withLocks(server.lockKeys, async () => {
    await resolveAuthorizedServer(
      actor,
      server.logical.id,
      `server.${action}`,
      context.job.bindingRevision,
    );
    jobActor(context);
    context.progress(action);
    suppressMonitoring(server.logical.id);
    try {
      await {
        start: startContainer,
        stop: stopContainer,
        restart: restartContainer,
      }[action](server.container.id);
      setIntentionalStop(server.logical.id, action === "stop");
      return { action };
    } finally {
      suppressMonitoring(server.logical.id);
    }
  });
}
export async function recoverUpdate(context: JobContext): Promise<void> {
  if (context.job.recovery.mutationStarted) return;
  // Pulling changes no server state. Backup recovery becomes applicable only
  // after its initial state and binding were durably recorded before stopping.
  if (typeof context.job.recovery.initialRunning !== "boolean") return;
  await recoverBackup(context);
}
export async function runUpdate(
  context: JobContext,
): Promise<Record<string, unknown>> {
  const request = updateRequestSchema.parse(context.job.input.request);
  const actor = jobActor(context);
  const capability: ServerCapability = request.forceRecreate
    ? "server.recreate"
    : "server.update";
  const server = await resolveAuthorizedServer(
    actor,
    context.job.serverId,
    capability,
    context.job.bindingRevision,
  );
  return withLocks(
    [...server.lockKeys, ...(request.createBackup ? ["backups:storage"] : [])],
    async () => {
      const selected = await validatedProject(server);
      try {
        if (
          context.job.input.sourceFingerprint !== selected.snapshot.fingerprint
        )
          throw new AppError(
            "SOURCE_CHANGED",
            409,
            "The Compose source changed after confirmation",
          );
        await assertSingleServiceContainer(server);
        const docker = getDockerInstance();
        const current = await docker
          .getContainer(server.container.id)
          .inspect();
        context.progress("pulling", {
          containerId: server.container.id,
          initiallyRunning: current.State.Running,
          previousImageId: current.Image,
        });
        await runCompose(
          [
            ...composeSnapshotArgs(selected.snapshot),
            "pull",
            "--policy",
            "always",
            selected.service,
          ],
          900_000,
        );
        const pulled = await docker.getImage(selected.image).inspect();
        const imageChanged = pulled.Id !== current.Image;
        if (!imageChanged && !request.forceRecreate) {
          notifyEvent(
            `operation:${context.job.id}`,
            `${server.container.displayName}: configured image is current.`,
          );
          return {
            alreadyCurrent: true,
            imageChanged: false,
            recreated: false,
          };
        }
        await resolveAuthorizedServer(
          actor,
          server.logical.id,
          capability,
          context.job.bindingRevision,
        );
        context.progress("validating", { selectedImageId: pulled.Id });
        suppressMonitoring(server.logical.id);
        if (request.createBackup) {
          context.progress("stopping", { stopRequested: true });
          await stopForDataOperation(server, context);
          context.progress("backing_up");
          const backup = await createStoppedBackup(
            { ...server, container: { ...server.container, state: "exited" } },
            context,
          );
          context.progress("validating", { backupId: backup.id });
        } else if (
          request.skipBackupConfirmation !== server.container.displayName
        ) {
          throw new AppError(
            "CONFIRMATION_REQUIRED",
            400,
            "Type the server name to confirm skipping the backup",
          );
        }
        // Check the live source and image again; execution uses the first vetted
        // snapshot so mutable source edits cannot redirect Compose after this.
        const latest = await validatedProject(server);
        try {
          if (latest.snapshot.fingerprint !== selected.snapshot.fingerprint)
            throw new AppError(
              "SOURCE_CHANGED",
              409,
              "Compose source changed during the update",
            );
        } finally {
          await latest.snapshot.cleanup();
        }
        await resolveAuthorizedServer(
          actor,
          server.logical.id,
          capability,
          context.job.bindingRevision,
        );
        await assertSingleServiceContainer(server);
        if ((await docker.getImage(selected.image).inspect()).Id !== pulled.Id)
          throw new AppError(
            "IMAGE_CHANGED",
            409,
            "Another manager changed the selected image during the update",
          );
        context.progress("recreating", { mutationStarted: true });
        const flags = current.State.Running
          ? ["up", "-d", "--wait", "--wait-timeout", "180"]
          : ["up", "--no-start"];
        flags.push("--no-deps", "--no-build", "--pull", "never");
        if (request.forceRecreate) flags.push("--force-recreate");
        await runCompose(
          [
            ...composeSnapshotArgs(selected.snapshot),
            ...flags,
            selected.service,
          ],
          240_000,
        );
        context.progress("verifying");
        await refreshServers();
        const replacement = await resolveAuthorizedServer(
          actor,
          server.logical.id,
          capability,
        );
        const actual = await docker
          .getContainer(replacement.container.id)
          .inspect();
        if (
          actual.Image !== pulled.Id ||
          actual.State.Running !== current.State.Running ||
          actual.State.Health?.Status === "unhealthy"
        )
          throw new AppError(
            "VERIFY_FAILED",
            409,
            "The resulting server state did not match the requested update; review it through Compose",
          );
        setIntentionalStop(server.logical.id, !current.State.Running);
        notifyEvent(
          `operation:${context.job.id}`,
          `${server.container.displayName}: ${request.forceRecreate ? "recreation" : "update"} completed.`,
        );
        return {
          imageChanged,
          recreated: true,
          forced: request.forceRecreate,
          running: actual.State.Running,
          verification: actual.State.Health ? "docker-health" : "docker-state",
        };
      } catch (error) {
        try {
          if (!context.job.recovery.mutationStarted)
            await restoreInitialRunningState(server, context);
        } catch {
          context.progress("recovery_required", { recoveryRequired: true });
        }
        notifyEvent(
          `operation:${context.job.id}`,
          `${server.container.displayName}: update failed. Review the operation and actual server state.`,
        );
        throw error;
      } finally {
        suppressMonitoring(server.logical.id);
        await selected.snapshot.cleanup();
      }
    },
  );
}
export function registerBackgroundJobs(): void {
  for (const action of ["start", "stop", "restart"] as const)
    registerJobHandler(action, {
      run: (context) => lifecycle(context, action),
    });
  registerJobHandler("backup", { run: runBackup, recover: recoverBackup });
  registerJobHandler("restore", { run: runRestore, recover: recoverRestore });
  registerJobHandler("update", { run: runUpdate, recover: recoverUpdate });
}
