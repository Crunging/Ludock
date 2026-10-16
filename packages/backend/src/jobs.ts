import { runLifecycle, recoverLifecycle, saveWorld } from "./lifecycle.js";
import {
  type ServerCapability,
  updateRequestSchema,
  scheduleSchema,
} from "@ludock/shared";
import { findUserById, getDatabase, type SessionUser } from "./database.js";
import { assertServerCapability } from "./authorization.js";
import { docker } from "./docker-client.js";
import { changeContainerState } from "./docker.js";
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
  keepStoppedAfterFailedBackup,
  assertBackupReady,
  backupCompleted,
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
import {
  isApiTokenOperationActor,
  isCurrentApiTokenOperationActor,
} from "./auth.js";
import { assertObservedServerBinding } from "./identity.js";

export function jobActor(context: JobContext): SessionUser {
  const id = context.job.actorId;
  const actor = isApiTokenOperationActor(id)
    ? isCurrentApiTokenOperationActor(id)
      ? { id: "api-token", username: "API token", role: "admin" as const }
      : null
    : findUserById(id);
  if (!actor || ("disabled" in actor && actor.disabled))
    throw new AppError(
      "ACCESS_REVOKED",
      403,
      "The operation owner no longer has access",
    );
  if (typeof context.job.input.scheduleId === "string") {
    const schedule = getDatabase()
      .query(
        "SELECT owner_id,input_json,revision FROM schedules WHERE id=? AND server_id=?",
      )
      .get(context.job.input.scheduleId, context.job.serverId) as
      | { owner_id: string; input_json: string; revision: number }
      | undefined;
    const settings = schedule
      ? scheduleSchema.parse(JSON.parse(schedule.input_json))
      : null;
    if (
      !schedule ||
      schedule.owner_id !== id ||
      context.job.input.scheduleRevision !== schedule.revision ||
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
/** Restarting with a backup returns a running server to running. A stopped
 * server can be backed up from Backups, or started. */
export function assertRunningForBackupRestart(state: string): void {
  if (state !== "running")
    throw new AppError("SERVER_NOT_RUNNING", 409, "Restart with a backup needs a running server. Start it, or back it up from Backups.");
}
/** Restart with a backup taken while the server is stopped, starting it once. */
async function runBackupRestart(context: JobContext): Promise<Record<string, unknown>> {
  const server = await resolveAuthorizedServer(
    jobActor(context), context.job.serverId, "server.restart", context.job.bindingRevision,
  );
  const assertAccess = () => {
    const actor = jobActor(context);
    assertServerCapability(actor, server.logical.id, "server.restart");
    assertServerCapability(actor, server.logical.id, "backups.create");
  };
  return withLocks([...server.lockKeys, "backups:storage"], async () => {
    const current = await resolveAuthorizedServer(
      jobActor(context), server.logical.id, "server.restart", context.job.bindingRevision,
    );
    assertAccess();
    assertRunningForBackupRestart(current.container.state);
    await assertBackupReady(current);
    suppressMonitoring(server.logical.id);
    try {
      await saveWorld(context, current, () => {
        assertAccess();
        assertObservedServerBinding(server.logical.id, current.observation, context.job.bindingRevision);
      });
      await stopForDataOperation(current, context);
      const backup = await createStoppedBackup(current, context);
      context.progress("backup_complete", { backupId: backup.id });
      const selected = await resolveAuthorizedServer(
        jobActor(context), server.logical.id, "server.restart", context.job.bindingRevision,
      );
      assertAccess();
      // Persist before dispatch. Recovery never repeats a start that reached Docker.
      context.progress("starting", { startRequested: true });
      await changeContainerState(selected.container.id, "start", (observation) => {
        assertAccess();
        assertObservedServerBinding(server.logical.id, observation, context.job.bindingRevision);
      });
      context.progress("started", { stateRestored: true });
      setIntentionalStop(server.logical.id, false);
      const { backupId, saveStatus, saveReason } = context.job.recovery;
      return { action: "restart", backupId: backupId ?? null, saveStatus, saveReason };
    } catch (error) {
      // Once a stop is recorded it may have reached Docker, even if its result
      // was lost. Reconcile it before reporting the server as left stopped.
      if (typeof context.job.recovery.initialRunning === "boolean" && !backupCompleted(context))
        await keepStoppedAfterFailedBackup(current, context);
      throw error;
    } finally {
      // Starting can update the game and change the world, so start only after
      // a completed backup, and only once the data helper has been removed.
      if (backupCompleted(context) && !context.job.recovery.dataHelperId)
        await restoreInitialRunningState(current, context);
      suppressMonitoring(server.logical.id);
    }
  });
}
async function recoverBackupRestart(context: JobContext): Promise<void> {
  // Starting is safe to reconcile, since a running server is left alone, but
  // only once the backup has completed.
  if (typeof context.job.recovery.initialRunning === "boolean")
    await recoverBackup(context, backupCompleted(context));
}
export async function recoverUpdate(context: JobContext): Promise<void> {
  if (context.job.recovery.mutationStarted) return;
  // Pulling changes no server state. Backup recovery becomes applicable only
  // after its initial state and binding were durably recorded before stopping.
  if (typeof context.job.recovery.initialRunning !== "boolean") return;
  await recoverBackup(context);
}
async function runUpdate(
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
        const current = await docker
          .getContainer(server.container.id)
          .inspect();
        assertServerCapability(
          jobActor(context), server.logical.id, capability,
        );
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
        assertServerCapability(
          jobActor(context), server.logical.id, capability,
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
  for (const action of ["start", "stop"] as const)
    registerJobHandler(action, {
      run: (context) => runLifecycle(context, action),
      recover: recoverLifecycle,
    });
  registerJobHandler("restart", {
    run: (context) => context.job.input.createBackup === true
      ? runBackupRestart(context) : runLifecycle(context, "restart"),
    recover: (context) => context.job.input.createBackup === true
      ? recoverBackupRestart(context) : recoverLifecycle(context),
  });
  registerJobHandler("backup", { run: runBackup, recover: recoverBackup });
  registerJobHandler("restore", { run: runRestore, recover: recoverRestore });
  registerJobHandler("update", { run: runUpdate, recover: recoverUpdate });
}
