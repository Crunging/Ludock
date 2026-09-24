import { okResponseSchema, serverResponseSchema, serversResponseSchema } from "@ludock/shared";
import { changeContainerState, type ContainerAction } from "../docker.js";
import { setIntentionalStop, suppressMonitoring } from "../monitoring.js";
import { getServerSnapshot, listServers } from "../servers.js";
import { audit, requestUser, respond, type ApiRoutes } from "./request.js";
import { serverAction } from "./server-action.js";

function containerAction(action: ContainerAction) {
  return serverAction(`server.${action}`, async (ctx, context) => {
    const id = context.logical.id;
    suppressMonitoring(id);
    await changeContainerState(context.container.id, action, context.assertAccess);
    setIntentionalStop(id, action === "stop");
    audit(ctx, `server.${action}`, id);
    return respond(okResponseSchema, { ok: true });
  });
}

export const serversRoutes: ApiRoutes = {
  "/api/v1/servers": {
    GET: async (ctx) => respond(serversResponseSchema, {
      servers: await listServers(requestUser(ctx)),
    }),
  },
  "/api/v1/servers/:id": {
    GET: async (ctx) => respond(serverResponseSchema,
      await getServerSnapshot(requestUser(ctx), ctx.params.id)),
  },
  "/api/v1/servers/:id/start": { POST: containerAction("start") },
  "/api/v1/servers/:id/stop": { POST: containerAction("stop") },
  "/api/v1/servers/:id/restart": { POST: containerAction("restart") },
};
