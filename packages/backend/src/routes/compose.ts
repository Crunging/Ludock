import { composeProjectResponseSchema, composeProjectsResponseSchema, okResponseSchema, operationResponseSchema, updateCapabilityResponseSchema, updateRequestSchema, } from "@ludock/shared";
import { deleteComposeProject, listComposeProjects, registerComposeProject, updateCapability, validatedProject, type ComposeProject, } from "../compose.js";
import { getDatabase } from "../database.js";
import { AppError } from "../errors.js";
import { enqueueOperation } from "../operations.js";
import { resolveAuthorizedServer } from "../servers.js";
import { administrator, audit, id, requestKey, requestUser, respond, type ApiRoutes } from "./request.js";
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

export const composeRoutes: ApiRoutes = {
  "/api/v1/servers/:id/update-capability": {
    GET: administrator(async (ctx) => {
      const context = await resolveAuthorizedServer(requestUser(ctx), id(ctx.params.id), "server.update");
      return respond(updateCapabilityResponseSchema, {
        capability: await updateCapability(context),
      });
    })
  },
  "/api/v1/servers/:id/updates": {
    POST: administrator(async (ctx) => {
      const user = requestUser(ctx);
      const serverId = id(ctx.params.id);
      const request = updateRequestSchema.parse(ctx.body);
      const context = await resolveAuthorizedServer(user, serverId, request.forceRecreate ? "server.recreate" : "server.update");
      if (!request.createBackup &&
        request.skipBackupConfirmation !== context.container.displayName)
        throw new AppError("CONFIRMATION_REQUIRED", 400, "Type the server name to confirm skipping backup");
      const { snapshot } = await validatedProject(context);
      try {
        audit(user, "server.update.confirmed", serverId, {
          createBackup: request.createBackup,
          forceRecreate: request.forceRecreate,
          sourceFingerprint: snapshot.fingerprint,
        });
        return respond(operationResponseSchema, {
          operation: enqueueOperation({
            serverId,
            actorId: user.id,
            kind: "update",
            bindingRevision: context.logical.bindingRevision,
            input: { request, sourceFingerprint: snapshot.fingerprint },
            idempotencyKey: requestKey(ctx.request.headers.get("Idempotency-Key") ?? undefined),
          }),
        }, 202);
      }
      finally {
        await snapshot.cleanup();
      }
    })
  },
  "/api/v1/compose-projects": {
    GET: administrator(() => respond(composeProjectsResponseSchema, {
      projects: listComposeProjects().map(publicProject),
    })),
    POST: administrator(async (ctx) => {
      const project = publicProject(await registerComposeProject(ctx.body));
      audit(requestUser(ctx), "compose.project.registered", undefined, {
        projectId: project.id,
      });
      return respond(composeProjectResponseSchema, { project }, 201);
    })
  },
  "/api/v1/compose-projects/:id": {
    DELETE: administrator((ctx) => {
      const projectId = id(ctx.params.id);
      if (getDatabase()
        .prepare("SELECT id FROM operations WHERE kind='update' AND status IN ('queued','running')")
        .get())
        throw new AppError("OPERATION_CONFLICT", 409, "Wait for queued or running updates before unregistering a project");
      deleteComposeProject(projectId);
      audit(requestUser(ctx), "compose.project.deleted", undefined, { projectId });
      return respond(okResponseSchema, { ok: true });
    })
  }
};
