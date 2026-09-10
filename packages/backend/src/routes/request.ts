import type { ResponseSchema } from "@ludock/shared";
import type { Server } from "bun";
import { z } from "zod";
import { AuthError } from "../auth.js";
import { writeAuditLog, type SessionUser } from "../database.js";
import { AppError } from "../errors.js";

export interface RequestContext {
  request: Request;
  url: URL;
  params: Record<string, string>;
  body: unknown;
  headers: Headers;
  ipAddress?: string;
  user: SessionUser | null;
  sessionTokenHash?: string;
}
export type HttpMethod = "GET" | "HEAD" | "POST" | "PUT" | "PATCH" | "DELETE" | "OPTIONS";
export type ApiHandler = (context: RequestContext) => Response | Promise<Response>;
export type ApiRoutes = Record<string, Partial<Record<HttpMethod, ApiHandler>>>;
export type HttpServer = Pick<Server<unknown>, "requestIP" | "timeout">;
export type NativeHandler = (
  request: Request & { params?: Record<string, string>; },
  server: HttpServer,
) => Promise<Response>;
export type NativeRoutes = Record<string, Partial<Record<HttpMethod, NativeHandler>>>;

export function requestUser(context: RequestContext): SessionUser {
  if (!context.user) throw new AuthError("AUTHENTICATION_REQUIRED", 401, "Authentication required");
  return context.user;
}
export function administrator(handler: ApiHandler): ApiHandler {
  return (context) => {
    if (requestUser(context).role !== "admin")
      return Response.json({ error: "Insufficient permissions" }, { status: 403 });
    return handler(context);
  };
}
const identifier = z.string().uuid();
export const id = (value: unknown): string => identifier.parse(value);
export function audit(
  user: SessionUser,
  action: string,
  targetId?: string,
  details?: Record<string, unknown>,
): void {
  writeAuditLog({
    userId: user.id === "api-token" ? undefined : user.id,
    action,
    targetType: targetId ? "server" : "settings",
    targetId,
    details,
  });
}
export function requestKey(value: string | undefined): string | undefined {
  if (value && !/^[\w.-]{1,128}$/.test(value))
    throw new AppError("INVALID_REQUEST_KEY", 400, "Invalid idempotency key");
  return value;
}

/** Invalid output is a server defect, not an invalid client request. Parsing
 * also keeps internal fields outside the public response contract. */
export function respond<T>(schema: ResponseSchema<T>, value: NoInfer<T>, status = 200): Response {
  let data: T;
  try {
    data = schema.parse(value);
  } catch {
    throw new AppError("INVALID_RESPONSE", 500, "The server could not produce a valid response");
  }
  return Response.json(data, { status });
}

/** Keep transport completion explicit for permissions, resource locks and logs.
 * Finishing the body does not wait for unrelated asynchronous handler cleanup. */
export function trackResponse(
  response: Response,
  signal: AbortSignal,
  finished: () => void,
): Response {
  if (!response.body) {
    finished();
    return response;
  }
  const reader = (response.body as ReadableStream<Uint8Array>).getReader();
  let ended = false;
  let abort: () => void;
  const done = () => {
    if (ended) return;
    ended = true;
    signal.removeEventListener("abort", abort);
    finished();
  };
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      abort = () => {
        if (ended) return;
        void reader.cancel(signal.reason).catch(() => { });
        controller.error(new DOMException("Request aborted", "AbortError"));
        done();
      };
      if (signal.aborted) abort();
      else signal.addEventListener("abort", abort, { once: true });
    },
    async pull(controller) {
      if (ended) return;
      try {
        const next = await reader.read();
        if (ended) return;
        if (next.done) {
          done();
          controller.close();
        } else controller.enqueue(next.value);
      } catch (error) {
        if (!ended) {
          done();
          controller.error(error);
        }
      }
    },
    async cancel(reason) {
      try { await reader.cancel(reason); }
      finally { done(); }
    },
  });
  return new Response(body, { status: response.status, statusText: response.statusText, headers: response.headers });
}
