import {
  backupSettingsResponseSchema,
  backupsResponseSchema,
  operationResponseSchema,
  okResponseSchema,
  backupSettingsSchema,
  restoreRequestSchema,
  type BackupSettings,
} from "@ludock/shared";
import { Router, type Router as RouterType } from "express";
import { requireRole, getRequestSession } from "../auth.js";
import { assertServerCapability } from "../authorization.js";
import { refreshServers, resolveAuthorizedServer } from "../servers.js";
import { getSetting, setSetting } from "../settings.js";
import { enqueueOperation } from "../operations.js";
import {
  validateBackupSettings,
  listBackups,
  getBackup,
  openBackupDownload,
  deleteBackup,
} from "../backups.js";
import { AppError } from "../errors.js";
import { respond, actor, id, audit, requestKey } from "./request.js";

export const backupsRouter: RouterType = Router();

backupsRouter.get(
  "/api/v1/settings/backups",
  requireRole("admin"),
  (_req, res) =>
    respond(res, backupSettingsResponseSchema, {
      settings: getSetting<BackupSettings>("backups"),
    }),
);
backupsRouter.put(
  "/api/v1/settings/backups",
  requireRole("admin"),
  async (req, res) => {
    const settings = backupSettingsSchema.parse(req.body);
    await validateBackupSettings(settings);
    setSetting("backups", settings);
    audit(actor(res), "settings.backups.updated");
    respond(res, backupSettingsResponseSchema, { settings });
  },
);
backupsRouter.get("/api/v1/servers/:id/backups", async (req, res) => {
  const serverId = id(req.params.id);
  await refreshServers();
  const user = actor(res);
  assertServerCapability(
    user,
    serverId,
    user.role === "admin" ? "backups.read" : "backups.create",
  );
  respond(res, backupsResponseSchema, { backups: listBackups(serverId) });
});
backupsRouter.post("/api/v1/servers/:id/backups", async (req, res) => {
  const serverId = id(req.params.id);
  const user = actor(res);
  const context = await resolveAuthorizedServer(
    user,
    serverId,
    "backups.create",
  );
  respond(res.status(202), operationResponseSchema, {
    operation: enqueueOperation({
      serverId,
      actorId: user.id,
      kind: "backup",
      bindingRevision: context.logical.bindingRevision,
      idempotencyKey: requestKey(req.get("Idempotency-Key")),
    }),
  });
});
backupsRouter.get(
  "/api/v1/servers/:id/backups/:backupId/download",
  requireRole("admin"),
  async (req, res) => {
    const serverId = id(req.params.id);
    assertServerCapability(actor(res), serverId, "backups.read");
    const backupId = id(req.params.backupId);
    getBackup(serverId, backupId);
    const stream = await openBackupDownload(serverId, backupId);
    res.setHeader("Content-Type", "application/x-tar");
    res.setHeader(
      "Content-Disposition",
      `attachment; filename="ludock-${backupId}.tar"`,
    );
    audit(actor(res), "backup.downloaded", serverId, { backupId });
    const user = actor(res);
    const revalidate = setInterval(() => {
      try {
        if (user.id !== "api-token" && !getRequestSession(req))
          throw new AppError("ACCESS_REVOKED", 401, "Session expired");
        assertServerCapability(user, serverId, "backups.read");
      } catch {
        stream.destroy();
        res.destroy();
      }
    }, 1000);
    revalidate.unref();
    const cleanup = () => {
      clearInterval(revalidate);
      stream.destroy();
    };
    stream.on("error", () => res.destroy());
    res.once("close", cleanup);
    res.once("finish", cleanup);
    stream.pipe(res);
  },
);
backupsRouter.delete(
  "/api/v1/servers/:id/backups/:backupId",
  requireRole("admin"),
  async (req, res) => {
    const serverId = id(req.params.id);
    assertServerCapability(actor(res), serverId, "backups.delete");
    await deleteBackup(serverId, id(req.params.backupId));
    audit(actor(res), "backup.deleted", serverId, {
      backupId: req.params.backupId,
    });
    respond(res, okResponseSchema, { ok: true });
  },
);
backupsRouter.post(
  "/api/v1/servers/:id/restores",
  requireRole("admin"),
  async (req, res) => {
    const user = actor(res);
    const serverId = id(req.params.id);
    const input = restoreRequestSchema.parse(req.body);
    const context = await resolveAuthorizedServer(
      user,
      serverId,
      "backups.restore",
    );
    if (input.confirmation !== context.container.displayName)
      throw new AppError(
        "CONFIRMATION_REQUIRED",
        400,
        "Type the server name to confirm the restore",
      );
    getBackup(serverId, input.backupId);
    respond(res.status(202), operationResponseSchema, {
      operation: enqueueOperation({
        serverId,
        actorId: user.id,
        kind: "restore",
        bindingRevision: context.logical.bindingRevision,
        input,
        idempotencyKey: requestKey(req.get("Idempotency-Key")),
      }),
    });
  },
);
