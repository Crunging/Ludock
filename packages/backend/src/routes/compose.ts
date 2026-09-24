import { operationResponseSchema, updateCapabilityResponseSchema, updateRequestSchema, } from "@ludock/shared";
import { assertRequestUser, operationActorId } from "../auth.js";
import { assertAdministrator, assertServerCapability } from "../authorization.js";
import { updateCapability, validatedProject, } from "../compose.js";
import { AppError } from "../errors.js";
import { enqueueOperation } from "../operations.js";
import { resolveAuthorizedServer } from "../servers.js";
import { administrator, audit, id, requestKey, requestUser, respond, type ApiRoutes } from "./request.js";
export const composeRoutes: ApiRoutes = {
  "/api/v1/servers/:id/update-capability": {
    GET: administrator(async (ctx) => {
      const context = await resolveAuthorizedServer(requestUser(ctx), id(ctx.params.id), "server.update");
      const capability = await updateCapability(context);
      assertAdministrator(assertRequestUser(ctx.request, requestUser(ctx)));
      return respond(updateCapabilityResponseSchema, {
        capability,
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
        assertServerCapability(assertRequestUser(ctx.request, user), serverId, request.forceRecreate ? "server.recreate" : "server.update");
        audit(ctx, "server.update.confirmed", serverId, {
          createBackup: request.createBackup,
          forceRecreate: request.forceRecreate,
          sourceFingerprint: snapshot.fingerprint,
        });
        return respond(operationResponseSchema, {
          operation: enqueueOperation({
            serverId,
            actorId: operationActorId(ctx.request, user),
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
};
