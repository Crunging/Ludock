import type { ServerCapability } from "@ludock/shared";
import { assertRequestUser } from "../auth.js";
import { assertServerCapability } from "../authorization.js";
import { AppError } from "../errors.js";
import {
  assertObservedServerBinding,
  resolveServerBinding,
  type ServerObservation,
} from "../identity.js";
import { acquireLocks } from "../operation-locks.js";
import { resolveAuthorizedServer, type ServerContext } from "../servers.js";
import { requestUser, trackResponse, type ApiHandler, type RequestContext } from "./request.js";

interface ServerActionContext extends ServerContext {
  assertAccess: (observation?: ServerObservation) => void;
  signal: AbortSignal;
  waitForCleanup: (cleanup: Promise<unknown>) => void;
}

/** Every direct server action declares its capability alongside its handler.
 * The resolved context supplies the Docker binding; URL IDs never reach Docker.
 * Resource locks last through both the response and the helper's cleanup. */
export function serverAction(
  capability: ServerCapability,
  action: (request: RequestContext, context: ServerActionContext) => Promise<Response>,
): ApiHandler {
  return async (request) => {
    const actor = requestUser(request);
    const context = await resolveAuthorizedServer(actor, request.params.id, capability);
    const revoked = new AbortController();
    const signal = AbortSignal.any([request.request.signal, revoked.signal]);
    const assertAccess = (observation?: ServerObservation) => {
      if (signal.aborted) throw new AppError("REQUEST_CLOSED", 409, "Request closed");
      const current = assertRequestUser(request.request, actor);
      assertServerCapability(current, context.logical.id, capability);
      if (observation)
        assertObservedServerBinding(context.logical.id, observation, context.logical.bindingRevision);
      else resolveServerBinding(context.logical.id, context.logical.bindingRevision);
    };
    assertAccess();
    const release = acquireLocks(context.lockKeys);
    const cleanup: Promise<unknown>[] = [];
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
      try { assertAccess(); }
      catch (error) { revoked.abort(error); }
    }, 1000);
    revalidate.unref();
    const settle = async () => {
      await Promise.allSettled(cleanup);
      handlerSettled = true;
      releaseWhenSettled();
    };
    try {
      const response = await action(request, {
        ...context, assertAccess, signal,
        waitForCleanup(promise) {
          // Observe immediately, including while a producer is still opening.
          cleanup.push(promise.catch(() => { }));
        },
      });
      void settle();
      return trackResponse(response, signal, responseDone);
    } catch (error) {
      responseDone();
      await settle();
      throw error;
    }
  };
}
