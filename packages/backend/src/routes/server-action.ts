import type { Request, RequestHandler, Response } from "express";
import type { ServerCapability } from "@ludock/shared";
import { assertRequestUser } from "../auth.js";
import { assertServerCapability } from "../authorization.js";
import { type SessionUser } from "../database.js";
import { AppError } from "../errors.js";
import {
  assertObservedServerBinding,
  resolveServerBinding,
  type ServerObservation,
} from "../identity.js";
import { acquireLocks } from "../operation-locks.js";
import { resolveAuthorizedServer, type ServerContext } from "../servers.js";

interface ServerActionContext extends ServerContext {
  assertAccess: (observation?: ServerObservation) => void;
}

/** Every direct server action declares its capability alongside its handler.
 * The resolved context supplies the Docker binding; URL IDs never reach Docker.
 * Resource locks last through both the response and the handler's cleanup. */
export function serverAction(
  capability: ServerCapability,
  action: (
    req: Request,
    res: Response,
    context: ServerActionContext,
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
    const assertAccess = (observation?: ServerObservation) => {
      if (res.destroyed || req.aborted)
        throw new AppError("REQUEST_CLOSED", 409, "Request closed");
      const current = assertRequestUser(req, actor);
      assertServerCapability(current, context.logical.id, capability);
      if (observation)
        assertObservedServerBinding(
          context.logical.id, observation, context.logical.bindingRevision,
        );
      else
        resolveServerBinding(context.logical.id, context.logical.bindingRevision);
    };
    assertAccess();
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
        assertAccess();
      } catch {
        res.destroy();
      }
    }, 1000);
    revalidate.unref();
    res.once("finish", responseDone);
    res.once("close", responseDone);
    try {
      await action(req, res, { ...context, assertAccess });
    } finally {
      handlerSettled = true;
      releaseWhenSettled();
    }
  };
}
