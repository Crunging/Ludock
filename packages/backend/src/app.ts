import path from "node:path";
import { z } from "zod";
import {
  authenticateRequest, defaultSetupWindow, type SetupWindow,
} from "./auth.js";
import { checkDatabase } from "./database.js";
import { developmentInstance, matchesDevelopmentInstance } from "./development-instance.js";
import { checkDockerConnection } from "./docker.js";
import { DockerApiError } from "./docker-transport.js";
import { AppError, errorResponse } from "./errors.js";
import { createLogger, errorMessage } from "./logger.js";
import { isExternalHttpsRequest, isSameOriginRequest } from "./request-security.js";
import { accessRoutes } from "./routes/access.js";
import { accountRoutes } from "./routes/accounts.js";
import { backupsRoutes } from "./routes/backups.js";
import { composeRoutes } from "./routes/compose.js";
import { filesRoutes } from "./routes/files.js";
import {
  trackResponse, type ApiHandler, type ApiRoutes, type HttpMethod,
  type NativeHandler, type NativeRoutes, type RequestContext,
} from "./routes/request.js";
import { schedulesRoutes } from "./routes/schedules.js";
import { serversRoutes } from "./routes/servers.js";
import { settingsRoutes } from "./routes/settings.js";
import { statusRoutes } from "./routes/status.js";
import { attentionRoutes } from "./routes/attention.js";
import { getMaxUploadBytes } from "./upload-limit.js";
import { staticFiles } from "./static-files.js";

const logger = createLogger("api");
const JSON_LIMIT = 64 * 1024;
const publicEndpoints = new Set([
  "GET /api/v1/auth/status", "POST /api/v1/auth/setup",
  "POST /api/v1/auth/login", "POST /api/v1/auth/logout", "GET /api/v1/health",
]);
interface CreateAppOptions {
  frontendDist?: string | false;
  setupWindow?: SetupWindow;
  /** Additional routes receive the same request and authentication policy. */
  routes?: ApiRoutes;
}

function responseHeaders(request: Request, url: URL, requestId: string): Headers {
  const headers = new Headers({
    "X-Request-ID": requestId,
    "X-Content-Type-Options": "nosniff",
    "Referrer-Policy": "no-referrer",
    "X-Frame-Options": "DENY",
    "Cross-Origin-Opener-Policy": "same-origin",
    "Cross-Origin-Resource-Policy": "same-origin",
    "Origin-Agent-Cluster": "?1",
    "Permissions-Policy": "camera=(), microphone=(), geolocation=(), payment=()",
    "Content-Security-Policy": "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'",
  });
  if (developmentInstance) headers.set("X-Ludock-Dev-Instance", developmentInstance);
  if (url.pathname.toLowerCase().startsWith("/api/")) headers.set("Cache-Control", "no-store");
  if (isExternalHttpsRequest(request))
    headers.set("Strict-Transport-Security", "max-age=31536000; includeSubDomains");
  return headers;
}

async function jsonBody(request: Request): Promise<unknown> {
  if (request.headers.get("content-type")?.split(";", 1)[0].trim().toLowerCase() !== "application/json")
    return undefined;
  if (Number(request.headers.get("content-length")) > JSON_LIMIT)
    throw new AppError("BODY_TOO_LARGE", 413, "Request body exceeds 64 KiB");
  if (!request.body) return undefined;
  const reader = (request.body as ReadableStream<Uint8Array>).getReader();
  const chunks: Uint8Array[] = [];
  let length = 0;
  let timedOut = false;
  const deadline = setTimeout(() => {
    timedOut = true;
    void reader.cancel().catch(() => {});
  }, 30_000);
  deadline.unref();
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (timedOut) throw new AppError("BODY_TIMEOUT", 408, "Request body timed out");
      if (done) break;
      length += value.byteLength;
      if (length > JSON_LIMIT) {
        await reader.cancel();
        throw new AppError("BODY_TOO_LARGE", 413, "Request body exceeds 64 KiB");
      }
      chunks.push(value);
    }
  } finally { clearTimeout(deadline); reader.releaseLock(); }
  if (length === 0) return undefined;
  const bytes = new Uint8Array(length);
  let offset = 0;
  for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.byteLength; }
  try {
    const body: unknown = JSON.parse(new TextDecoder().decode(bytes));
    if (body === null || typeof body !== "object") throw new Error("JSON object or array required");
    return body;
  } catch {
    throw new AppError("INVALID_JSON", 400, "Invalid JSON request body");
  }
}

function requestError(error: unknown, context: RequestContext, requestId: string): Response {
  if (error instanceof z.ZodError)
    return Response.json({ error: "Invalid request", code: "INVALID_REQUEST" }, { status: 400 });
  if (error instanceof AppError) return errorResponse(error);
  // A container can disappear between authorization and the Docker request.
  if (error instanceof DockerApiError && error.statusCode === 404)
    return Response.json({ error: "Container not found", code: "NOT_FOUND" }, { status: 404 });
  logger.error("Unhandled request error", {
    requestId, method: context.request.method, path: context.url.pathname,
    error: errorMessage(error),
  });
  return context.url.pathname.toLowerCase().startsWith("/api/")
    ? Response.json({ error: "Internal server error", requestId }, { status: 500 })
    : new Response("Internal server error", { status: 500 });
}

type RoutedHandler = (
  request: Parameters<NativeHandler>[0],
  server: Parameters<NativeHandler>[1],
  encodedParams?: Record<string, string>,
) => ReturnType<NativeHandler>;

function requestHandler(handler: ApiHandler, publicEndpoint = false): RoutedHandler {
  return async (request, server, encodedParams) => {
    // Bound incoming JSON before allowing long operations or file transfers.
    server.timeout(request, 30);
    const url = new URL(request.url);
    const requestId = crypto.randomUUID();
    const startedAt = performance.now();
    const context: RequestContext = {
      request, url, params: request.params || {}, body: undefined,
      headers: responseHeaders(request, url, requestId),
      ipAddress: server.requestIP(request)?.address,
      user: null,
    };
    let response: Response;
    try {
      if (encodedParams) {
        try {
          context.params = Object.fromEntries(Object.entries(encodedParams)
            .map(([name, value]) => [name, decodeURIComponent(value)]));
        } catch {
          throw new AppError("INVALID_PATH", 400, "Invalid request path");
        }
      }
      if (!matchesDevelopmentInstance(request.headers.get("x-ludock-dev-instance") || undefined)) {
        response = Response.json({ error: "This request belongs to a different development checkout." }, { status: 409 });
      } else if (!["GET", "HEAD", "OPTIONS"].includes(request.method) && (
        request.headers.get("sec-fetch-site") === "cross-site" ||
        (request.headers.has("origin") && !isSameOriginRequest(request))
      )) {
        response = Response.json({ error: "Cross-origin request rejected" }, { status: 403 });
      } else {
        const authentication = authenticateRequest(request);
        if (authentication) Object.assign(context, authentication);
        if (!publicEndpoint && !authentication) {
          response = Response.json({ error: "Authentication required" }, { status: 401 });
        } else {
          context.body = await jsonBody(request);
          // Transfers may be quiet while helpers validate paths or stop a
          // game. Their cancellation and operation limits own the lifetime.
          server.timeout(request, 0);
          // The body can arrive slowly. Session revocation and role changes
          // during that wait must take effect before a route can mutate state.
          const current = authenticateRequest(request);
          context.user = current?.user ?? null;
          context.sessionTokenHash = current?.sessionTokenHash;
          response = !publicEndpoint && !current
            ? Response.json({ error: "Authentication required" }, { status: 401 })
            : await handler(context);
        }
      }
    } catch (error) {
      response = requestError(error, context, requestId);
    }
    for (const [name, value] of context.headers) {
      if (name !== "set-cookie") response.headers.set(name, value);
    }
    for (const cookie of context.headers.getSetCookie()) response.headers.append("Set-Cookie", cookie);
    response = trackResponse(response, request.signal, () => {
      if (url.pathname === "/api/v1/application-logs") return;
      logger.debug("HTTP request completed", {
        requestId, method: request.method, path: url.pathname, status: response.status,
        durationMs: Math.round(performance.now() - startedAt), remoteAddress: context.ipAddress,
      });
    });
    if (request.method === "HEAD") {
      await response.body?.cancel();
      return new Response(null, { status: response.status, headers: response.headers });
    }
    return response;
  };
}

function matchingRoute(pathname: string, patterns: string[]): {
  pattern: string;
  encodedParams: Record<string, string>;
} | null {
  const parts = pathname.replace(/\/$/, "").split("/");
  for (const pattern of patterns) {
    const expected = pattern.split("/");
    if (expected.length !== parts.length) continue;
    if (!expected.every((part, index) => part.startsWith(":")
      ? parts[index].length > 0 : part.toLowerCase() === parts[index].toLowerCase())) continue;
    return {
      pattern,
      encodedParams: Object.fromEntries(expected.flatMap((part, index) =>
        part.startsWith(":") ? [[part.slice(1), parts[index]]] : [])),
    };
  }
  return null;
}

export function createApp(options: CreateAppOptions = {}): {
  routes: NativeRoutes;
  fetch: NativeHandler;
  maxRequestBodySize: number;
} {
  let healthCache: { checkedAt: number; healthy: boolean; } | null = null;
  let healthProbe: Promise<boolean> | null = null;
  const health: ApiHandler = async () => {
    if (!healthCache || Date.now() - healthCache.checkedAt >= 5000) {
      healthProbe ||= (async () => {
        try { checkDatabase(); await checkDockerConnection(); return true; }
        catch { return false; }
      })().then((healthy) => {
        healthCache = { checkedAt: Date.now(), healthy };
        healthProbe = null;
        return healthy;
      });
      await healthProbe;
    }
    return healthCache?.healthy
      ? Response.json({ status: "ok", docker: "connected", database: "connected" })
      : Response.json({ status: "degraded" }, { status: 503 });
  };
  const handlers: ApiRoutes = {
    ...accountRoutes(options.setupWindow || defaultSetupWindow),
    "/api/v1/health": { GET: health },
    ...accessRoutes, ...backupsRoutes, ...composeRoutes, ...filesRoutes,
    ...schedulesRoutes, ...serversRoutes, ...settingsRoutes, ...statusRoutes, ...attentionRoutes,
    ...options.routes,
  };
  const routes: Record<string, Partial<Record<HttpMethod, RoutedHandler>>> = {};
  for (const [pathname, methods] of Object.entries(handlers)) {
    const native: Partial<Record<HttpMethod, RoutedHandler>> = {};
    for (const [method, handler] of Object.entries(methods)) {
      native[method as HttpMethod] = requestHandler(handler, publicEndpoints.has(`${method} ${pathname}`));
    }
    // Bun suppresses a HEAD body, but explicit cancellation also releases file
    // helper and authorization lifetimes created by the GET handler.
    if (native.GET && !native.HEAD) native.HEAD = native.GET;
    native.OPTIONS ||= requestHandler(() => new Response(null, {
      status: 204, headers: { Allow: Object.keys(native).join(", ") },
    }));
    routes[pathname] = native;
  }
  const frontendDist = options.frontendDist === undefined
    ? path.resolve(import.meta.dir, "../../frontend/dist") : options.frontendDist;
  const serveStatic = frontendDist === false
    ? () => new Response("Not found", { status: 404 }) : staticFiles(frontendDist);
  const fallback: ApiHandler = async (context) => {
    if (/^\/(api|ws)(\/|$)/i.test(context.url.pathname))
      return Response.json({ error: "API endpoint not found" }, { status: 404 });
    return serveStatic(context.request);
  };
  const publicFallback = requestHandler(fallback, true);
  const apiFallback = requestHandler(fallback);
  const patterns = Object.keys(routes);
  return {
    routes,
    maxRequestBodySize: Math.max(JSON_LIMIT, getMaxUploadBytes()),
    fetch: (request, server) => {
      const pathname = new URL(request.url).pathname;
      const match = matchingRoute(pathname, patterns);
      const handler = match && routes[match.pattern][request.method as HttpMethod];
      // Dispatch variants before policy or body consumption, using the same
      // method-specific handler as native routes. A redirect would require a
      // client to replay uploads and could hide public routes behind auth.
      if (handler && match) return handler(request, server, match.encodedParams);
      return /^\/api\/v1(?:\/|$)/i.test(pathname)
        ? apiFallback(request, server) : publicFallback(request, server);
    },
  };
}
