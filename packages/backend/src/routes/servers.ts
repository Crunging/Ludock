import { okResponseSchema, serverResponseSchema, serversResponseSchema, } from "@ludock/shared";
import { writeAuditLog } from "../database.js";
import { restartContainer, startContainer, stopContainer, } from "../docker.js";
import { AppError, errorResponse } from "../errors.js";
import { createLogger, errorMessage } from "../logger.js";
import { setIntentionalStop, suppressMonitoring } from "../monitoring.js";
import { getServerSnapshot, listServers } from "../servers.js";
import { requestUser, respond, type ApiRoutes, type RequestContext } from "./request.js";
import { serverAction } from "./server-action.js";
const logger = createLogger("api");
interface DockerRouteError extends Error {
  statusCode?: number;
  code?: string;
}
function sendDockerError(caught: unknown, fallbackMessage: string): Response {
  const error = caught as DockerRouteError;
  if (error instanceof AppError) return errorResponse(error);
  if (error.statusCode === 304) {
    return respond(okResponseSchema, { ok: true });
  }
  if (error.code === "INVALID_CONTAINER_ID") {
    return Response.json({ error: "Invalid container identifier" }, { status: 400 });
  }
  if (error.statusCode === 403 || error.code === "FORBIDDEN") {
    return Response.json({ error: "Container is not managed by Ludock" }, { status: 403 });
  }
  if (error.statusCode === 404) {
    return Response.json({ error: "Container not found" }, { status: 404 });
  }
  logger.error(fallbackMessage, { error: errorMessage(caught) });
  return Response.json({ error: fallbackMessage }, { status: 500 });
}
function auditContainerAction(ctx: RequestContext, action: string, containerId: string): void {
  const user = requestUser(ctx);
  writeAuditLog({
    userId: user.id === "api-token" ? undefined : user.id,
    action: `server.${action}`,
    targetType: "server",
    targetId: containerId,
    ipAddress: ctx.ipAddress,
  });
}

export const serversRoutes: ApiRoutes = {
  "/api/v1/servers": {
    GET: async (ctx) => {
      return respond(serversResponseSchema, {
        servers: await listServers(requestUser(ctx)),
      });
    }
  },
  "/api/v1/servers/:id": {
    GET: async (ctx) => respond(serverResponseSchema,
      await getServerSnapshot(requestUser(ctx), ctx.params.id)),
  },
  "/api/v1/servers/:id/start": {
    POST: serverAction("server.start", async (ctx, context) => {
      try {
        const id = context.logical.id;
        suppressMonitoring(id);
        await startContainer(context.container.id, context.assertAccess);
        setIntentionalStop(id, false);
        auditContainerAction(ctx, "start", id);
        return respond(okResponseSchema, { ok: true });
      }
      catch (error: unknown) {
        return sendDockerError(error, "Failed to start container");
      }
    })
  },
  "/api/v1/servers/:id/stop": {
    POST: serverAction("server.stop", async (ctx, context) => {
      try {
        const id = context.logical.id;
        suppressMonitoring(id);
        await stopContainer(context.container.id, context.assertAccess);
        setIntentionalStop(id, true);
        auditContainerAction(ctx, "stop", id);
        return respond(okResponseSchema, { ok: true });
      }
      catch (error: unknown) {
        return sendDockerError(error, "Failed to stop container");
      }
    })
  },
  "/api/v1/servers/:id/restart": {
    POST: serverAction("server.restart", async (ctx, context) => {
      try {
        const id = context.logical.id;
        suppressMonitoring(id);
        await restartContainer(context.container.id, context.assertAccess);
        setIntentionalStop(id, false);
        auditContainerAction(ctx, "restart", id);
        return respond(okResponseSchema, { ok: true });
      }
      catch (error: unknown) {
        return sendDockerError(error, "Failed to restart container");
      }
    })
  }
};
