import {
  Router,
  type Request,
  type Response,
  type Router as RouterType,
} from "express";
import {
  serversResponseSchema,
  serverResponseSchema,
  serverStatsSchema,
  okResponseSchema,
} from "@ludock/shared";
import { writeAuditLog, type SessionUser } from "../database.js";
import {
  getContainerStats,
  restartContainer,
  startContainer,
  stopContainer,
} from "../docker.js";
import { createLogger, errorMessage } from "../logger.js";
import { listServers, getServer, resolveAuthorizedServer } from "../servers.js";
import { AppError } from "../errors.js";
import { AuthError } from "../auth.js";
import { AuthorizationError } from "../authorization.js";
import { ServerBindingError } from "../identity.js";
import { setIntentionalStop, suppressMonitoring } from "../monitoring.js";
import { respond } from "./request.js";
import { serverAction } from "./server-action.js";

export const serversRouter: RouterType = Router();
const logger = createLogger("api");
interface DockerRouteError extends Error {
  statusCode?: number;
  code?: string;
}

function sendDockerError(
  res: Response,
  caught: unknown,
  fallbackMessage: string,
): void {
  const error = caught as DockerRouteError;
  if (error instanceof AppError || error instanceof AuthError || error instanceof AuthorizationError || error instanceof ServerBindingError) {
    res
      .status(error.statusCode)
      .json({ error: error.message, code: error.code });
    return;
  }

  if (error.statusCode === 304) {
    respond(res, okResponseSchema, { ok: true });
    return;
  }

  if (error.code === "INVALID_CONTAINER_ID") {
    res.status(400).json({ error: "Invalid container identifier" });
    return;
  }

  if (error.statusCode === 403 || error.code === "FORBIDDEN") {
    res.status(403).json({ error: "Container is not managed by Ludock" });
    return;
  }

  if (error.statusCode === 404) {
    res.status(404).json({ error: "Container not found" });
    return;
  }

  logger.error(fallbackMessage, { error: errorMessage(caught) });
  res.status(500).json({ error: fallbackMessage });
}

function auditContainerAction(
  req: Request,
  res: Response,
  action: string,
  containerId: string,
): void {
  const user = res.locals.user as SessionUser;
  writeAuditLog({
    userId: user.id === "api-token" ? undefined : user.id,
    action: `server.${action}`,
    targetType: "server",
    targetId: containerId,
    ipAddress: req.ip,
  });
}

serversRouter.get("/api/v1/servers", async (_req: Request, res: Response) => {
  respond(res, serversResponseSchema, {
    servers: await listServers(res.locals.user as SessionUser),
  });
});

serversRouter.get(
  "/api/v1/servers/:id",
  async (req: Request, res: Response) => {
    const actor = res.locals.user as SessionUser;
    const id = req.params.id as string;
    const server = await getServer(actor, id);
    let stats = null;
    if (server.bindingStatus === "active") {
      try {
        const context = await resolveAuthorizedServer(actor, id, "server.view");
        stats = serverStatsSchema.parse(
          await getContainerStats(context.container.id),
        );
      } catch {
        /* Status remains useful without stats. */
      }
    }
    respond(res, serverResponseSchema, { server, stats });
  },
);

serversRouter.post(
  "/api/v1/servers/:id/start",
  serverAction("server.start", async (req, res, context) => {
    try {
      const id = context.logical.id;
      suppressMonitoring(id);
      await startContainer(context.container.id, context.assertAccess);
      setIntentionalStop(id, false);
      auditContainerAction(req, res, "start", id);
      respond(res, okResponseSchema, { ok: true });
    } catch (error: unknown) {
      sendDockerError(res, error, "Failed to start container");
    }
  }),
);

serversRouter.post(
  "/api/v1/servers/:id/stop",
  serverAction("server.stop", async (req, res, context) => {
    try {
      const id = context.logical.id;
      suppressMonitoring(id);
      await stopContainer(context.container.id, context.assertAccess);
      setIntentionalStop(id, true);
      auditContainerAction(req, res, "stop", id);
      respond(res, okResponseSchema, { ok: true });
    } catch (error: unknown) {
      sendDockerError(res, error, "Failed to stop container");
    }
  }),
);

serversRouter.post(
  "/api/v1/servers/:id/restart",
  serverAction("server.restart", async (req, res, context) => {
    try {
      const id = context.logical.id;
      suppressMonitoring(id);
      await restartContainer(context.container.id, context.assertAccess);
      setIntentionalStop(id, false);
      auditContainerAction(req, res, "restart", id);
      respond(res, okResponseSchema, { ok: true });
    } catch (error: unknown) {
      sendDockerError(res, error, "Failed to restart container");
    }
  }),
);
