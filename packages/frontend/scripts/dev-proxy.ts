import type { Server, ServerWebSocket, WebSocketHandler } from "bun";

type Message = string | Uint8Array<ArrayBuffer>;
interface Bridge {
  upstream: WebSocket;
  browser?: ServerWebSocket<Bridge>;
  pending: Message[];
  pendingBytes: number;
  closed?: { code: number; reason: string };
}

const MAX_BUFFERED_BYTES = 2 * 1024 * 1024;
const HOP_BY_HOP = [
  "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
  "te", "trailer", "transfer-encoding", "upgrade",
];

function endToEndHeaders(source: Headers): Headers {
  const headers = new Headers(source);
  for (const name of (source.get("connection") || "").split(",")) {
    if (name.trim()) headers.delete(name.trim());
  }
  for (const name of HOP_BY_HOP) headers.delete(name);
  return headers;
}

function size(message: Message): number {
  return typeof message === "string" ? Buffer.byteLength(message) : message.byteLength;
}

function closeUpstream(bridge: Bridge, code: number, reason: string) {
  if (bridge.upstream.readyState >= WebSocket.CLOSING) return;
  try {
    bridge.upstream.close(code, reason);
  } catch {
    // The client API may reject protocol-only close codes received from peers.
    bridge.upstream.close(1000, "Connection closed");
  }
}

function closeBridge(bridge: Bridge, code: number, reason: string) {
  bridge.closed = { code, reason };
  bridge.pending.length = 0;
  bridge.pendingBytes = 0;
  bridge.browser?.close(code, reason);
  closeUpstream(bridge, code, reason);
}

function sendToBrowser(bridge: Bridge, message: Message) {
  if (!bridge.browser) {
    if (bridge.pendingBytes + size(message) > MAX_BUFFERED_BYTES) {
      closeBridge(bridge, 1013, "Development proxy buffer exceeded");
      return;
    }
    bridge.pending.push(message);
    bridge.pendingBytes += size(message);
    return;
  }
  if (bridge.browser.getBufferedAmount() + size(message) > MAX_BUFFERED_BYTES) {
    closeBridge(bridge, 1013, "Development proxy buffer exceeded");
    return;
  }
  bridge.browser.send(message);
}

/** Keep the checkout's API and console traffic on its own backend connection. */
export function createDevelopmentProxy(options: { target: string; instance?: string }) {
  const target = new URL(options.target);
  if (
    !["http:", "https:"].includes(target.protocol) || target.username ||
    target.password || target.pathname !== "/" || target.search || target.hash
  ) throw new Error("LUDOCK_DEV_API_ORIGIN must be an HTTP or HTTPS origin.");
  const instance = options.instance;
  if (instance && !/^[a-zA-Z0-9_-]+$/.test(instance)) {
    throw new Error("LUDOCK_DEV_INSTANCE must contain only letters, numbers, hyphens, and underscores.");
  }
  const bridges = new Set<Bridge>();
  const requests = new Set<AbortController>();

  function requestHeaders(request: Request): Headers {
    const headers = endToEndHeaders(request.headers);
    const cookieName = instance ? `ludock_session_${instance}` : "ludock_session";
    const cookie = request.headers.get("cookie")?.split(";").map(part => part.trim())
      .find(part => part.startsWith(`${cookieName}=`));
    if (cookie) headers.set("cookie", cookie);
    else headers.delete("cookie");
    if (instance) headers.set("x-ludock-dev-instance", instance);
    else headers.delete("x-ludock-dev-instance");
    const origin = new URL(request.url);
    headers.set("host", origin.host);
    headers.set("x-forwarded-host", origin.host);
    headers.set("x-forwarded-proto", origin.protocol.slice(0, -1));
    headers.set("x-forwarded-port", origin.port || (origin.protocol === "https:" ? "443" : "80"));
    return headers;
  }

  async function upgrade(request: Request, server: Server<Bridge>): Promise<Response | undefined> {
    const url = new URL(request.url);
    url.protocol = target.protocol === "https:" ? "wss:" : "ws:";
    url.host = target.host;
    const headers = requestHeaders(request);
    for (const name of ["sec-websocket-key", "sec-websocket-version", "sec-websocket-extensions", "sec-websocket-protocol"]) {
      headers.delete(name);
    }
    const protocols = request.headers.get("sec-websocket-protocol")?.split(",").map(value => value.trim()).filter(Boolean);
    const upstream = new WebSocket(url, { protocols: protocols || [], headers: Object.fromEntries(headers) });
    upstream.binaryType = "arraybuffer";
    const bridge: Bridge = { upstream, pending: [], pendingBytes: 0 };
    bridges.add(bridge);
    let settle: (connected: boolean) => void = () => {};
    let connected = false;
    const opened = new Promise<boolean>(resolve => { settle = resolve; });
    const cancel = () => {
      settle(false);
      closeBridge(bridge, 1001, "Client disconnected");
    };
    const timeout = setTimeout(() => {
      settle(false);
      closeBridge(bridge, 1011, "Development backend connection timed out");
    }, 10_000);
    request.signal.addEventListener("abort", cancel, { once: true });
    upstream.addEventListener("open", () => { connected = true; settle(true); });
    upstream.addEventListener("message", event => {
      const message: Message = typeof event.data === "string" ? event.data : new Uint8Array(event.data as ArrayBuffer);
      sendToBrowser(bridge, message);
    });
    upstream.addEventListener("close", event => {
      settle(false);
      const code = event.code === 1005 ? 1000 : event.code === 1006 ? 1011 : event.code;
      bridge.closed = { code, reason: event.code === 1006 ? "Development backend disconnected" : event.reason };
      bridge.browser?.close(bridge.closed.code, bridge.closed.reason);
      if (!bridge.browser) bridges.delete(bridge);
    });
    upstream.addEventListener("error", () => {
      settle(false);
      if (connected) closeBridge(bridge, 1011, "Development backend disconnected");
    });
    if (request.signal.aborted) cancel();
    const ready = await opened;
    clearTimeout(timeout);
    request.signal.removeEventListener("abort", cancel);
    if (!ready || request.signal.aborted) {
      bridges.delete(bridge);
      closeBridge(bridge, 1011, "Development backend unavailable");
      return new Response("Development backend unavailable", { status: 502 });
    }
    const upgraded = server.upgrade(request, {
      data: bridge,
      ...(upstream.protocol ? { headers: { "Sec-WebSocket-Protocol": upstream.protocol } } : {}),
    });
    if (!upgraded) {
      bridges.delete(bridge);
      closeBridge(bridge, 1001, "Client disconnected");
      return new Response("WebSocket upgrade required", { status: 400 });
    }
  }

  const websocket: WebSocketHandler<Bridge> = {
    data: {} as Bridge,
    idleTimeout: 0,
    maxPayloadLength: MAX_BUFFERED_BYTES,
    backpressureLimit: MAX_BUFFERED_BYTES,
    closeOnBackpressureLimit: true,
    open(browser) {
      const bridge = browser.data;
      bridge.browser = browser;
      const pending = bridge.pending.splice(0);
      bridge.pendingBytes = 0;
      for (const message of pending) sendToBrowser(bridge, message);
      if (bridge.closed) browser.close(bridge.closed.code, bridge.closed.reason);
    },
    message(browser, message) {
      const bridge = browser.data;
      if (bridge.upstream.readyState !== WebSocket.OPEN) {
        closeBridge(bridge, 1011, "Development backend disconnected");
      } else if (bridge.upstream.bufferedAmount + size(message) > MAX_BUFFERED_BYTES) {
        closeBridge(bridge, 1013, "Development proxy buffer exceeded");
      } else bridge.upstream.send(message);
    },
    close(browser, code, reason) {
      const bridge = browser.data;
      bridges.delete(bridge);
      bridge.pending.length = 0;
      bridge.pendingBytes = 0;
      closeUpstream(bridge, code, reason);
    },
  };

  return {
    websocket,
    async fetch(request: Request, server: Server<Bridge>): Promise<Response | undefined> {
      const url = new URL(request.url);
      // Reject DNS-rebinding hosts before forwarding any checkout credentials.
      if (!["127.0.0.1", "localhost", "[::1]"].includes(url.hostname)) {
        return new Response("Invalid development host", { status: 403 });
      }
      if (request.headers.get("upgrade")?.toLowerCase() === "websocket") return upgrade(request, server);
      url.protocol = target.protocol;
      url.host = target.host;
      const controller = new AbortController();
      requests.add(controller);
      const cancel = () => controller.abort();
      request.signal.addEventListener("abort", cancel, { once: true });
      if (request.signal.aborted) cancel();
      try {
        const response = await fetch(url, {
          method: request.method,
          headers: requestHeaders(request),
          body: request.body,
          redirect: "manual",
          decompress: false,
          signal: controller.signal,
        });
        if (!response.body) {
          requests.delete(controller);
          request.signal.removeEventListener("abort", cancel);
          return new Response(null, { status: response.status, headers: endToEndHeaders(response.headers) });
        }
        const reader = response.body.getReader();
        let finished = false;
        const finish = () => {
          if (finished) return;
          finished = true;
          requests.delete(controller);
          request.signal.removeEventListener("abort", cancel);
        };
        const body = new ReadableStream<Uint8Array>({
          async pull(stream) {
            try {
              const result = await reader.read();
              if (result.done) { finish(); stream.close(); }
              else stream.enqueue(result.value);
            } catch (error) { finish(); stream.error(error); }
          },
          async cancel() { controller.abort(); finish(); await reader.cancel().catch(() => {}); },
        });
        return new Response(body, { status: response.status, headers: endToEndHeaders(response.headers) });
      } catch {
        requests.delete(controller);
        request.signal.removeEventListener("abort", cancel);
        return new Response("Development backend unavailable", { status: 502 });
      } finally {
        // Streaming responses retain cancellation ownership until read or canceled.
        if (controller.signal.aborted) {
          requests.delete(controller);
          request.signal.removeEventListener("abort", cancel);
        }
      }
    },
    stop() {
      for (const request of requests) request.abort();
      requests.clear();
      for (const bridge of bridges) {
        closeBridge(bridge, 1001, "Development server stopping");
        bridge.upstream.terminate();
      }
      bridges.clear();
    },
  };
}
