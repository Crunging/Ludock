// Development server: serves the app with hot reload and forwards /api and /ws
// to the backend so the browser sees a single origin.
import homepage from "../index.html";
import { resolve } from "node:path";
import type { Server, ServerWebSocket } from "bun";

const port = Number(process.env.LUDOCK_DEV_PORT || 3000);
const api = new URL(process.env.LUDOCK_DEV_API_ORIGIN || "http://127.0.0.1:3001");
const HOP_BY_HOP = [
  "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
  "te", "trailer", "transfer-encoding", "upgrade",
];

interface Bridge {
  upstream: WebSocket;
  pending: (string | ArrayBuffer)[];
  browser?: ServerWebSocket<Bridge>;
}

function forwardedHeaders(request: Request): Headers {
  const url = new URL(request.url);
  const headers = new Headers(request.headers);
  for (const name of HOP_BY_HOP) headers.delete(name);
  headers.set("x-forwarded-host", url.host);
  headers.set("x-forwarded-proto", url.protocol.slice(0, -1));
  return headers;
}

function closeUpstream(upstream: WebSocket, code: number, reason: string) {
  // Clients may only send 1000 or 3000-4999; fall back for other codes.
  try { upstream.close(code, reason); } catch { upstream.close(); }
}

async function proxy(request: Request, server: Server<Bridge>): Promise<Response | undefined> {
  const url = new URL(request.url);
  // A rebinding DNS name must not receive this browser's session cookie.
  if (!["127.0.0.1", "localhost", "[::1]"].includes(url.hostname))
    return new Response("Invalid development host", { status: 403 });
  const target = new URL(url.pathname + url.search, api);
  if (request.headers.get("upgrade")?.toLowerCase() !== "websocket") {
    try {
      return await fetch(target, {
        method: request.method,
        headers: forwardedHeaders(request),
        body: request.body,
        redirect: "manual",
        decompress: false,
        signal: request.signal,
      });
    } catch {
      return new Response("Development backend unavailable", { status: 502 });
    }
  }
  target.protocol = api.protocol === "https:" ? "wss:" : "ws:";
  const headers = forwardedHeaders(request);
  for (const name of [...headers.keys()]) if (name.startsWith("sec-websocket-")) headers.delete(name);
  const upstream = new WebSocket(target, { headers: Object.fromEntries(headers) });
  upstream.binaryType = "arraybuffer";
  const opened = await new Promise<boolean>((resolve) => {
    upstream.addEventListener("open", () => resolve(true), { once: true });
    upstream.addEventListener("close", () => resolve(false), { once: true });
  });
  if (!opened) return new Response("Development backend unavailable", { status: 502 });
  const bridge: Bridge = { upstream, pending: [] };
  upstream.addEventListener("message", (event) => {
    if (bridge.browser) bridge.browser.send(event.data);
    else bridge.pending.push(event.data);
  });
  upstream.addEventListener("close", (event) => {
    bridge.browser?.close(event.code === 1005 ? 1000 : event.code === 1006 ? 1011 : event.code, event.reason);
  });
  if (server.upgrade(request, { data: bridge })) return undefined;
  closeUpstream(upstream, 1000, "Upgrade failed");
  return new Response("WebSocket upgrade failed", { status: 400 });
}

const publicDirectory = resolve(import.meta.dir, "../public");
const publicFiles = Object.fromEntries(
  Array.from(new Bun.Glob("**/*").scanSync({ cwd: publicDirectory, onlyFiles: true }))
    .map((file) => [`/${file}`, Bun.file(resolve(publicDirectory, file))]),
);
const server = Bun.serve({
  hostname: "127.0.0.1",
  port,
  idleTimeout: 0,
  // The backend owns upload limits.
  maxRequestBodySize: Number.MAX_SAFE_INTEGER,
  development: { hmr: true, console: false },
  routes: {
    ...publicFiles,
    "/api": proxy,
    "/api/*": proxy,
    "/ws": proxy,
    "/ws/*": proxy,
    "/*": homepage,
  },
  websocket: {
    data: {} as Bridge,
    idleTimeout: 0,
    open(browser) {
      const bridge = browser.data;
      bridge.browser = browser;
      for (const message of bridge.pending.splice(0)) browser.send(message);
      if (bridge.upstream.readyState >= WebSocket.CLOSING) browser.close(1011, "Development backend disconnected");
    },
    message(browser, message) {
      if (browser.data.upstream.readyState === WebSocket.OPEN) browser.data.upstream.send(message);
      else browser.close(1011, "Development backend disconnected");
    },
    close(browser, code, reason) {
      closeUpstream(browser.data.upstream, code, reason);
    },
  },
});
console.log(`Ludock development: ${server.url}`);
for (const signal of ["SIGINT", "SIGTERM"] as const) process.once(signal, () => void server.stop(true));
