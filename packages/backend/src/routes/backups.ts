import { backupPreflightResponseSchema, backupSettingsResponseSchema, backupSettingsSchema, backupStorageResponseSchema, backupsResponseSchema, okResponseSchema, operationResponseSchema, restoreRequestSchema, type BackupSettings, } from "@ludock/shared";
import { managedReadable } from "../managed-readable.js";
import { assertRequestUser, operationActorId } from "../auth.js";
import { assertAdministrator, assertServerCapability } from "../authorization.js";
import { deleteBackup, getBackup, getBackupPreflight, getBackupStorageStatus, listBackups, openBackupDownload } from "../backups.js";
import { validateBackupSettings } from "../backup-storage.js";
import { AppError } from "../errors.js";
import { enqueueOperation } from "../operations.js";
import { refreshServers, resolveAuthorizedServer } from "../servers.js";
import { getSetting, setSetting } from "../settings.js";
import { administrator, audit, id, requestKey, requestUser, respond, type ApiRoutes } from "./request.js";

export const backupsRoutes: ApiRoutes = {
  "/api/v1/settings/backups/status": {
    GET: administrator(async (ctx) => {
      const storage = await getBackupStorageStatus();
      assertAdministrator(assertRequestUser(ctx.request, requestUser(ctx)));
      return respond(backupStorageResponseSchema, { storage });
    }),
  },
  "/api/v1/settings/backups": {
    GET: administrator(() => respond(backupSettingsResponseSchema, {
      settings: getSetting<BackupSettings>("backups"),
    })),
    PUT: administrator(async (ctx) => {
      const settings = backupSettingsSchema.parse(ctx.body);
      await validateBackupSettings(settings);
      assertAdministrator(assertRequestUser(ctx.request, requestUser(ctx)));
      setSetting("backups", settings);
      audit(ctx, "settings.backups.updated");
      return respond(backupSettingsResponseSchema, { settings });
    })
  },
  "/api/v1/servers/:id/backups": {
    GET: administrator(async (ctx) => {
      const serverId = id(ctx.params.id);
      // Backup metadata is persisted locally and remains useful during a daemon
      // outage. A successful refresh still applies binding changes before access.
      await refreshServers().catch(() => undefined);
      const user = assertRequestUser(ctx.request, requestUser(ctx));
      assertServerCapability(user, serverId, "backups.read");
      return respond(backupsResponseSchema, { backups: listBackups(serverId) });
    }),
    POST: async (ctx) => {
      const serverId = id(ctx.params.id);
      const user = requestUser(ctx);
      const context = await resolveAuthorizedServer(user, serverId, "backups.create");
      assertServerCapability(assertRequestUser(ctx.request, user), serverId, "backups.create");
      return respond(operationResponseSchema, {
        operation: enqueueOperation({
          serverId,
          actorId: operationActorId(ctx.request, user),
          kind: "backup",
          bindingRevision: context.logical.bindingRevision,
          idempotencyKey: requestKey(ctx.request.headers.get("Idempotency-Key") ?? undefined),
        }),
      }, 202);
    }
  },
  "/api/v1/servers/:id/backups/preflight": {
    GET: async (ctx) => {
      const serverId = id(ctx.params.id);
      const user = requestUser(ctx);
      const context = await resolveAuthorizedServer(user, serverId, "backups.create");
      assertServerCapability(assertRequestUser(ctx.request, user), serverId, "backups.create");
      const preflight = await getBackupPreflight(context);
      assertServerCapability(assertRequestUser(ctx.request, user), serverId, "backups.create");
      return respond(backupPreflightResponseSchema, { preflight });
    },
  },
  "/api/v1/servers/:id/backups/:backupId/download": {
    GET: administrator(async (ctx) => {
      const serverId = id(ctx.params.id);
      assertServerCapability(requestUser(ctx), serverId, "backups.read");
      const backupId = id(ctx.params.backupId);
      getBackup(serverId, backupId);
      const stream = await openBackupDownload(serverId, backupId);
      try {
        assertServerCapability(assertRequestUser(ctx.request, requestUser(ctx)), serverId, "backups.read");
      } catch (error) {
        await stream.cancel().catch(() => {});
        throw error;
      }
      ctx.headers.set("Content-Type", "application/x-tar");
      ctx.headers.set("Content-Disposition", `attachment; filename="ludock-${backupId}.tar"`);
      audit(ctx, "backup.downloaded", serverId, { backupId });
      const user = requestUser(ctx);
      const revoked = new AbortController();
      const signal = AbortSignal.any([ctx.request.signal, revoked.signal]);
      const revalidate = setInterval(() => {
        try {
          const current = assertRequestUser(ctx.request, user);
          assertServerCapability(current, serverId, "backups.read");
        }
        catch (error) { revoked.abort(error); }
      }, 1000);
      revalidate.unref();
      const download = managedReadable(stream, { signal, cleanup() { clearInterval(revalidate); } });
      return new Response(download.stream);
    })
  },
  "/api/v1/servers/:id/backups/:backupId": {
    DELETE: administrator(async (ctx) => {
      const serverId = id(ctx.params.id);
      assertServerCapability(requestUser(ctx), serverId, "backups.delete");
      await deleteBackup(serverId, id(ctx.params.backupId), () => {
        assertServerCapability(assertRequestUser(ctx.request, requestUser(ctx)), serverId, "backups.delete");
      });
      audit(ctx, "backup.deleted", serverId, {
        backupId: ctx.params.backupId,
      });
      return respond(okResponseSchema, { ok: true });
    })
  },
  "/api/v1/servers/:id/restores": {
    POST: administrator(async (ctx) => {
      const user = requestUser(ctx);
      const serverId = id(ctx.params.id);
      const input = restoreRequestSchema.parse(ctx.body);
      const context = await resolveAuthorizedServer(user, serverId, "backups.restore");
      assertServerCapability(assertRequestUser(ctx.request, user), serverId, "backups.restore");
      if (input.confirmation !== context.container.displayName)
        throw new AppError("CONFIRMATION_REQUIRED", 400, "Type the server name to confirm the restore");
      getBackup(serverId, input.backupId);
      return respond(operationResponseSchema, {
        operation: enqueueOperation({
          serverId,
          actorId: operationActorId(ctx.request, user),
          kind: "restore",
          bindingRevision: context.logical.bindingRevision,
          input,
          idempotencyKey: requestKey(ctx.request.headers.get("Idempotency-Key") ?? undefined),
        }),
      }, 202);
    })
  }
};
