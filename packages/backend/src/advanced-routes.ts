import { Router, type Router as RouterType } from "express";
import { z } from "zod";
import {
  backupSettingsSchema,
  serverGrantsSchema,
  updateRequestSchema,
} from "@ludock/shared";
import { requireRole, getRequestSession } from "./auth.js";
import {
  findUserById,
  getDatabase,
  writeAuditLog,
  type SessionUser,
} from "./database.js";
import {
  assertServerCapability,
  listUserServerGrants,
  setUserServerGrants,
} from "./authorization.js";
import { getLogicalServer, reviewServerBinding } from "./identity.js";
import {
  getServer,
  refreshServers,
  resolveAuthorizedServer,
} from "./servers.js";
import { getDiscoveryDiagnostics } from "./docker.js";
import { getGameCapabilityMatrix } from "./server-presets.js";
import { getSetting, setSetting } from "./settings.js";
import {
  configureNotifications,
  notificationConfiguration,
} from "./notifications.js";
import { configureAvailability, getAvailability } from "./monitoring.js";
import { createSchedule, deleteSchedule, listSchedules } from "./schedules.js";
import {
  enqueueOperation,
  getOperation,
  listOperations,
  publicOperation,
} from "./operations.js";
import {
  deleteComposeProject,
  isComposeAvailable,
  listComposeProjects,
  registerComposeProject,
  updateCapability,
  validatedProject,
  type ComposeProject,
} from "./compose.js";
import {
  validateBackupSettings,
  listBackups,
  getBackup,
  openBackupDownload,
  deleteBackup,
} from "./backups.js";
import { withLocks } from "./operation-locks.js";
import { AppError } from "./errors.js";

function publicProject(project: ComposeProject) {
  return {
    id: project.id,
    projectName: project.projectName,
    projectDirectory: project.projectDirectory,
    composeFiles: project.composeFiles,
    envFiles: project.envFiles,
    disabled: project.disabled,
  };
}

export const advancedRouter: RouterType = Router();
const actor = (res: { locals: Record<string, unknown> }) =>
  res.locals.user as SessionUser;
const identifier = z.string().uuid();
const id = (value: unknown) => identifier.parse(value);
function audit(
  user: SessionUser,
  action: string,
  targetId?: string,
  details?: Record<string, unknown>,
) {
  writeAuditLog({
    userId: user.id === "api-token" ? undefined : user.id,
    action,
    targetType: targetId ? "server" : "settings",
    targetId,
    details,
  });
}
function requestKey(value: string | undefined): string | undefined {
  if (value && !/^[\w.-]{1,128}$/.test(value))
    throw new AppError("INVALID_REQUEST_KEY", 400, "Invalid idempotency key");
  return value;
}

advancedRouter.get(
  "/api/v1/users/:userId/server-grants",
  requireRole("admin"),
  (req, res) => {
    if (!findUserById(id(req.params.userId)))
      throw new AppError("NOT_FOUND", 404, "User not found");
    res.json({ grants: listUserServerGrants(req.params.userId as string) });
  },
);
advancedRouter.put(
  "/api/v1/users/:userId/server-grants",
  requireRole("admin"),
  async (req, res) => {
    const input = serverGrantsSchema.parse(req.body);
    await refreshServers();
    res.json({
      grants: setUserServerGrants(
        id(req.params.userId),
        input.grants,
        actor(res),
      ),
    });
  },
);
advancedRouter.post(
  "/api/v1/servers/:id/binding-review",
  requireRole("admin"),
  async (req, res) => {
    const serverId = id(req.params.id);
    await refreshServers();
    const server = getLogicalServer(serverId);
    const input = z
      .object({ confirmation: z.string().max(200) })
      .strict()
      .parse(req.body);
    if (!server || !server.pendingFingerprint)
      throw new AppError(
        "NO_REVIEW",
        409,
        "This server has no pending binding review",
      );
    if (input.confirmation !== server.displayName)
      throw new AppError(
        "CONFIRMATION_REQUIRED",
        400,
        "Type the server name to accept the changed binding",
      );
    await withLocks([`server:${serverId}`], () => {
      reviewServerBinding(serverId, server.pendingFingerprint!);
    });
    audit(actor(res), "server.binding.reviewed", serverId);
    res.json({ server: await getServer(actor(res), serverId) });
  },
);
advancedRouter.get(
  "/api/v1/settings/backups",
  requireRole("admin"),
  (_req, res) => res.json({ settings: getSetting("backups") }),
);
advancedRouter.put(
  "/api/v1/settings/backups",
  requireRole("admin"),
  async (req, res) => {
    const settings = backupSettingsSchema.parse(req.body);
    await validateBackupSettings(settings);
    setSetting("backups", settings);
    audit(actor(res), "settings.backups.updated");
    res.json({ settings });
  },
);
advancedRouter.get("/api/v1/servers/:id/backups", async (req, res) => {
  const serverId = id(req.params.id);
  await refreshServers();
  const user = actor(res);
  assertServerCapability(
    user,
    serverId,
    user.role === "admin" ? "backups.read" : "backups.create",
  );
  res.json({ backups: listBackups(serverId) });
});
advancedRouter.post("/api/v1/servers/:id/backups", async (req, res) => {
  const serverId = id(req.params.id);
  const user = actor(res);
  const context = await resolveAuthorizedServer(
    user,
    serverId,
    "backups.create",
  );
  res
    .status(202)
    .json({
      operation: enqueueOperation({
        serverId,
        actorId: user.id,
        kind: "backup",
        bindingRevision: context.logical.bindingRevision,
        idempotencyKey: requestKey(req.get("Idempotency-Key")),
      }),
    });
});
advancedRouter.get(
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
advancedRouter.delete(
  "/api/v1/servers/:id/backups/:backupId",
  requireRole("admin"),
  async (req, res) => {
    const serverId = id(req.params.id);
    assertServerCapability(actor(res), serverId, "backups.delete");
    await deleteBackup(serverId, id(req.params.backupId));
    audit(actor(res), "backup.deleted", serverId, {
      backupId: req.params.backupId,
    });
    res.json({ ok: true });
  },
);
advancedRouter.post(
  "/api/v1/servers/:id/restores",
  requireRole("admin"),
  async (req, res) => {
    const user = actor(res);
    const serverId = id(req.params.id);
    const input = z
      .object({ backupId: identifier, confirmation: z.string().max(200) })
      .strict()
      .parse(req.body);
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
    res
      .status(202)
      .json({
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
advancedRouter.get("/api/v1/servers/:id/operations", (req, res) => {
  const serverId = id(req.params.id);
  assertServerCapability(actor(res), serverId, "server.view");
  res.json({ operations: listOperations(serverId) });
});
advancedRouter.get("/api/v1/operations/:id", (req, res) => {
  const operation = getOperation(id(req.params.id));
  if (!operation) throw new AppError("NOT_FOUND", 404, "Operation not found");
  assertServerCapability(actor(res), operation.serverId, "server.view");
  res.json({ operation: publicOperation(operation) });
});
advancedRouter.get("/api/v1/servers/:id/schedules", (req, res) =>
  res.json({ schedules: listSchedules(actor(res), id(req.params.id)) }),
);
advancedRouter.post("/api/v1/servers/:id/schedules", (req, res) =>
  res
    .status(201)
    .json({
      schedule: createSchedule(actor(res), id(req.params.id), req.body),
    }),
);
advancedRouter.delete(
  "/api/v1/servers/:id/schedules/:scheduleId",
  (req, res) => {
    deleteSchedule(actor(res), id(req.params.id), id(req.params.scheduleId));
    res.json({ ok: true });
  },
);
advancedRouter.get("/api/v1/servers/:id/availability", (req, res) => {
  const serverId = id(req.params.id);
  assertServerCapability(actor(res), serverId, "server.view");
  res.json(getAvailability(serverId));
});
advancedRouter.put(
  "/api/v1/servers/:id/availability",
  requireRole("admin"),
  (req, res) => {
    const serverId = id(req.params.id);
    assertServerCapability(actor(res), serverId, "server.view");
    res.json(configureAvailability(serverId, req.body));
    audit(actor(res), "availability.configured", serverId);
  },
);
advancedRouter.get(
  "/api/v1/servers/:id/update-capability",
  requireRole("admin"),
  async (req, res) => {
    const context = await resolveAuthorizedServer(
      actor(res),
      id(req.params.id),
      "server.update",
    );
    res.json({ capability: await updateCapability(context) });
  },
);
advancedRouter.post(
  "/api/v1/servers/:id/updates",
  requireRole("admin"),
  async (req, res) => {
    const user = actor(res);
    const serverId = id(req.params.id);
    const request = updateRequestSchema.parse(req.body);
    const context = await resolveAuthorizedServer(
      user,
      serverId,
      request.forceRecreate ? "server.recreate" : "server.update",
    );
    if (
      !request.createBackup &&
      request.skipBackupConfirmation !== context.container.displayName
    )
      throw new AppError(
        "CONFIRMATION_REQUIRED",
        400,
        "Type the server name to confirm skipping backup",
      );
    const { snapshot } = await validatedProject(context);
    try {
      audit(user, "server.update.confirmed", serverId, {
        createBackup: request.createBackup,
        forceRecreate: request.forceRecreate,
        sourceFingerprint: snapshot.fingerprint,
      });
      res
        .status(202)
        .json({
          operation: enqueueOperation({
            serverId,
            actorId: user.id,
            kind: "update",
            bindingRevision: context.logical.bindingRevision,
            input: { request, sourceFingerprint: snapshot.fingerprint },
            idempotencyKey: requestKey(req.get("Idempotency-Key")),
          }),
        });
    } finally {
      await snapshot.cleanup();
    }
  },
);
advancedRouter.get(
  "/api/v1/compose-projects",
  requireRole("admin"),
  (_req, res) =>
    res.json({ projects: listComposeProjects().map(publicProject) }),
);
advancedRouter.post(
  "/api/v1/compose-projects",
  requireRole("admin"),
  async (req, res) => {
    const project = publicProject(await registerComposeProject(req.body));
    audit(actor(res), "compose.project.registered", undefined, {
      projectId: project.id,
    });
    res.status(201).json({ project });
  },
);
advancedRouter.delete(
  "/api/v1/compose-projects/:id",
  requireRole("admin"),
  (req, res) => {
    const projectId = id(req.params.id);
    if (
      getDatabase()
        .prepare(
          "SELECT id FROM operations WHERE kind='update' AND status IN ('queued','running')",
        )
        .get()
    )
      throw new AppError(
        "OPERATION_CONFLICT",
        409,
        "Wait for queued or running updates before unregistering a project",
      );
    deleteComposeProject(projectId);
    audit(actor(res), "compose.project.deleted", undefined, { projectId });
    res.json({ ok: true });
  },
);
advancedRouter.get("/api/v1/notifications", requireRole("admin"), (_req, res) =>
  res.json(notificationConfiguration()),
);
advancedRouter.put(
  "/api/v1/notifications",
  requireRole("admin"),
  (req, res) => {
    const input = z
      .object({
        enabled: z.boolean(),
        webhookUrl: z.string().max(1024).optional(),
      })
      .strict()
      .parse(req.body);
    configureNotifications(input.enabled, input.webhookUrl);
    audit(actor(res), "notifications.configured");
    res.json(notificationConfiguration());
  },
);
advancedRouter.get(
  "/api/v1/diagnostics",
  requireRole("admin"),
  async (_req, res) => {
    let diagnostics: unknown[] = [];
    let dockerConnected = true;
    try {
      diagnostics = await getDiscoveryDiagnostics();
    } catch {
      dockerConnected = false;
    }
    res.json({
      diagnostics,
      dockerConnected,
      composeAvailable: await isComposeAvailable(),
    });
  },
);
advancedRouter.get("/api/v1/integrations", requireRole("admin"), (_req, res) =>
  res.json({ integrations: getGameCapabilityMatrix() }),
);
