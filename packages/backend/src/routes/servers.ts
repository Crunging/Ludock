import { okResponseSchema, operationResponseSchema, serverResponseSchema, serversResponseSchema } from "@ludock/shared";
import { assertRequestUser, operationActorId } from "../auth.js";
import { assertServerCapability } from "../authorization.js";
import { changeContainerState } from "../docker.js";
import { setIntentionalStop, suppressMonitoring } from "../monitoring.js";
import { enqueueOperation } from "../operations.js";
import { getServerSnapshot, listServers, resolveAuthorizedServer } from "../servers.js";
import { audit, requestKey, requestUser, respond, type ApiHandler, type ApiRoutes } from "./request.js";
import { serverAction } from "./server-action.js";

function containerAction(action: "stop" | "restart"): ApiHandler {
  return async (ctx) => {
    const actor = requestUser(ctx);
    const context = await resolveAuthorizedServer(actor, ctx.params.id, `server.${action}`);
    assertServerCapability(assertRequestUser(ctx.request, actor), context.logical.id, `server.${action}`);
    return respond(operationResponseSchema, { operation: enqueueOperation({
      serverId: context.logical.id,
      actorId: operationActorId(ctx.request, actor),
      kind: action,
      bindingRevision: context.logical.bindingRevision,
      idempotencyKey: requestKey(ctx.request.headers.get("Idempotency-Key") ?? undefined),
    }) }, 202);
  };
}

export const serversRoutes: ApiRoutes = {
  "/api/v1/servers": {
    GET: async (ctx) => respond(serversResponseSchema, { servers: await listServers(requestUser(ctx)) }),
  },
  "/api/v1/servers/:id": {
    GET: async (ctx) => respond(serverResponseSchema, await getServerSnapshot(requestUser(ctx), ctx.params.id)),
  },
  "/api/v1/servers/:id/start": {
    POST: serverAction("server.start", async (ctx, context) => {
      suppressMonitoring(context.logical.id);
      await changeContainerState(context.container.id, "start", context.assertAccess);
      setIntentionalStop(context.logical.id, false);
      audit(ctx, "server.start", context.logical.id);
      return respond(okResponseSchema, { ok: true });
    }),
  },
  "/api/v1/servers/:id/stop": { POST: containerAction("stop") },
  "/api/v1/servers/:id/restart": { POST: containerAction("restart") },
};
