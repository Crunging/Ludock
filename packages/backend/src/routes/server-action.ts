import type { Request, RequestHandler, Response } from "express";
import type { ServerCapability } from "@ludock/shared";
import { getRequestSession } from "../auth.js";
import { assertServerCapability } from "../authorization.js";
import { type SessionUser } from "../database.js";
import { AppError } from "../errors.js";
import { acquireLocks } from "../operation-locks.js";
import { resolveAuthorizedServer, type ServerContext } from "../servers.js";

/** Every direct server action declares its capability alongside its handler.
 * The resolved context supplies the Docker binding; URL IDs never reach Docker.
 * Resource locks last through both the response and the handler's cleanup. */
export function serverAction(
  capability: ServerCapability,
  action: (
    req: Request,
    res: Response,
    context: ServerContext,
  ) => Promise<void>,
): RequestHandler {
  return async (req, res) => {
    const actor = res.locals.user as SessionUser;
    const context = await resolveAuthorizedServer(
      actor,
      req.params.id as string,
      capability,
    );
    if (res.destroyed || req.aborted) return;
    const release = acquireLocks(context.lockKeys);
    let handlerSettled = false;
    let responseClosed = false;
    const releaseWhenSettled = () => {
      if (handlerSettled && responseClosed) release();
    };
    const responseDone = () => {
      responseClosed = true;
      clearInterval(revalidate);
      releaseWhenSettled();
    };
    const revalidate = setInterval(() => {
      try {
        if (actor.id !== "api-token" && !getRequestSession(req))
          throw new AppError("ACCESS_REVOKED", 401, "Session expired");
        assertServerCapability(actor, context.logical.id, capability);
      } catch {
        res.destroy();
      }
    }, 1000);
    revalidate.unref();
    res.once("finish", responseDone);
    res.once("close", responseDone);
    try {
      await action(req, res, context);
    } finally {
      handlerSettled = true;
      releaseWhenSettled();
    }
  };
}
