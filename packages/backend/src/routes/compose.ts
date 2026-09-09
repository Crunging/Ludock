import {
  updateCapabilityResponseSchema,
  operationResponseSchema,
  composeProjectsResponseSchema,
  composeProjectResponseSchema,
  okResponseSchema,
  updateRequestSchema,
} from "@ludock/shared";
import { Router, type Router as RouterType } from "express";
import { requireRole } from "../auth.js";
import { getDatabase } from "../database.js";
import { resolveAuthorizedServer } from "../servers.js";
import { enqueueOperation } from "../operations.js";
import {
  deleteComposeProject,
  listComposeProjects,
  registerComposeProject,
  updateCapability,
  validatedProject,
  type ComposeProject,
} from "../compose.js";
import { AppError } from "../errors.js";
import { respond, actor, id, audit, requestKey } from "./request.js";

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

export const composeRouter: RouterType = Router();

composeRouter.get(
  "/api/v1/servers/:id/update-capability",
  requireRole("admin"),
  async (req, res) => {
    const context = await resolveAuthorizedServer(
      actor(res),
      id(req.params.id),
      "server.update",
    );
    respond(res, updateCapabilityResponseSchema, {
      capability: await updateCapability(context),
    });
  },
);
composeRouter.post(
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
      respond(res.status(202), operationResponseSchema, {
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
composeRouter.get(
  "/api/v1/compose-projects",
  requireRole("admin"),
  (_req, res) =>
    respond(res, composeProjectsResponseSchema, {
      projects: listComposeProjects().map(publicProject),
    }),
);
composeRouter.post(
  "/api/v1/compose-projects",
  requireRole("admin"),
  async (req, res) => {
    const project = publicProject(await registerComposeProject(req.body));
    audit(actor(res), "compose.project.registered", undefined, {
      projectId: project.id,
    });
    respond(res.status(201), composeProjectResponseSchema, { project });
  },
);
composeRouter.delete(
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
    respond(res, okResponseSchema, { ok: true });
  },
);
