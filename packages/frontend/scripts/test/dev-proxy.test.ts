import { afterEach, describe, expect, it } from "bun:test";
import { createDevelopmentProxy } from "../dev-proxy";

const cleanup: Array<() => void | Promise<void>> = [];
afterEach(async () => {
  for (const close of cleanup.splice(0).reverse()) await close();
});

function keepServer<T extends { stop(closeActiveConnections: boolean): Promise<void> }>(server: T): T {
  cleanup.push(() => server.stop(true));
  return server;
}

function startProxy(target: URL, instance = "checkout-a") {
  const proxy = createDevelopmentProxy({ target: target.origin, instance });
  const server = keepServer(Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch: proxy.fetch,
    websocket: proxy.websocket,
    idleTimeout: 0,
    maxRequestBodySize: Number.MAX_SAFE_INTEGER,
  }));
  cleanup.push(() => proxy.stop());
  return server;
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

async function received<T>(promise: Promise<T>, description: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      promise,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error(`Timed out waiting for ${description}`)), 2000);
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

function connect(url: URL, headers: Record<string, string> = {}) {
  const socket = new WebSocket(url.href.replace(/^http/, "ws"), { headers });
  socket.binaryType = "arraybuffer";
  cleanup.push(() => socket.close());
  const opened = deferred<void>();
  const closed = deferred<{ code: number; reason: string }>();
  const queued: Array<string | ArrayBuffer> = [];
  const waiting: Array<(message: string | ArrayBuffer) => void> = [];
  socket.addEventListener("open", () => opened.resolve());
  socket.addEventListener("error", () => opened.reject(new Error("WebSocket connection failed")));
  socket.addEventListener("close", (event) => closed.resolve({ code: event.code, reason: event.reason }));
  socket.addEventListener("message", (event) => {
    const next = waiting.shift();
    if (next) next(event.data);
    else queued.push(event.data);
  });
  return {
    socket,
    opened: opened.promise,
    closed: closed.promise,
    nextMessage: () => queued.length
      ? Promise.resolve(queued.shift()!)
      : new Promise<string | ArrayBuffer>((resolve) => waiting.push(resolve)),
  };
}

describe("development HTTP proxy", () => {
  it("preserves HTTP semantics and forwards only the selected checkout's session", async () => {
    let incoming: { method: string; path: string; host: string | null; origin: string | null; cookie: string | null; instance: string | null; type: string | null; body: string } | undefined;
    const backend = keepServer(Bun.serve({
      hostname: "127.0.0.1", port: 0,
      async fetch(request) {
        const url = new URL(request.url);
        incoming = {
          method: request.method, path: `${url.pathname}${url.search}`,
          host: request.headers.get("host"), origin: request.headers.get("origin"),
          cookie: request.headers.get("cookie"), instance: request.headers.get("x-ludock-dev-instance"),
          type: request.headers.get("content-type"), body: await request.text(),
        };
        const headers = new Headers({ "Content-Type": "application/problem+json" });
        headers.append("Set-Cookie", "ludock_session_checkout-a=replaced; HttpOnly; Path=/");
        headers.append("Set-Cookie", "csrf=fixture; SameSite=Strict; Path=/");
        return new Response('{"error":"fixture conflict"}', { status: 409, headers });
      },
    }));
    const frontend = startProxy(backend.url);
    const response = await fetch(new URL("/api/v1/files?path=a%2Fb&overwrite=false", frontend.url), {
      method: "PUT",
      headers: {
        "Content-Type": "application/json", Origin: frontend.url.origin,
        Cookie: "ludock_session=production; ludock_session_checkout-b=peer; ludock_session_checkout-a=selected; unrelated=value",
        "X-Ludock-Dev-Instance": "wrong-checkout",
      },
      body: '{"name":"world"}',
    });
    expect(incoming).toEqual({
      method: "PUT", path: "/api/v1/files?path=a%2Fb&overwrite=false",
      host: frontend.url.host, origin: frontend.url.origin,
      cookie: "ludock_session_checkout-a=selected", instance: "checkout-a",
      type: "application/json", body: '{"name":"world"}',
    });
    expect(response.status).toBe(409);
    expect(response.headers.get("content-type")).toBe("application/problem+json");
    expect(response.headers.getSetCookie()).toEqual([
      "ludock_session_checkout-a=replaced; HttpOnly; Path=/",
      "csrf=fixture; SameSite=Strict; Path=/",
    ]);
    expect(await response.text()).toBe('{"error":"fixture conflict"}');
  });

  it("omits peer and production cookies when the current checkout has no session", async () => {
    const backend = keepServer(Bun.serve({
      hostname: "127.0.0.1", port: 0,
      fetch: (request) => Response.json({ cookie: request.headers.get("cookie") }),
    }));
    const frontend = startProxy(backend.url);
    const response = await fetch(new URL("/api/v1/auth/status", frontend.url), {
      headers: { Cookie: "ludock_session=production; ludock_session_checkout-b=peer" },
    });
    expect(await response.json()).toEqual({ cookie: null });
  });

  it("rejects a non-loopback Host before sending credentials to the backend", async () => {
    let forwarded = false;
    const backend = keepServer(Bun.serve({
      hostname: "127.0.0.1", port: 0,
      fetch() { forwarded = true; return new Response("unexpected request"); },
    }));
    const frontend = startProxy(backend.url);
    const response = await fetch(new URL("/api/v1/auth/status", frontend.url), {
      headers: { Host: "untrusted.example", Cookie: "ludock_session_checkout-a=selected" },
    });
    expect(response.status).toBe(403);
    expect(forwarded).toBe(false);
  });

  it("returns redirects without following them on behalf of the browser", async () => {
    const backend = keepServer(Bun.serve({
      hostname: "127.0.0.1", port: 0,
      fetch: () => new Response(null, { status: 307, headers: { Location: "/api/v1/next" } }),
    }));
    const frontend = startProxy(backend.url);
    const response = await fetch(new URL("/api/v1/redirect", frontend.url), { redirect: "manual" });
    expect(response.status).toBe(307);
    expect(response.headers.get("location")).toBe("/api/v1/next");
  });

  it("preserves compressed download bytes and their representation headers", async () => {
    const archive = Bun.gzipSync(new TextEncoder().encode("world backup fixture\n".repeat(100)));
    const backend = keepServer(Bun.serve({
      hostname: "127.0.0.1", port: 0,
      fetch: () => new Response(archive, { headers: {
        "Content-Type": "application/octet-stream", "Content-Encoding": "gzip",
        "Content-Length": String(archive.byteLength), "Content-Disposition": 'attachment; filename="world.bin"',
      } }),
    }));
    const frontend = startProxy(backend.url);
    const response = await fetch(new URL("/api/v1/backups/download", frontend.url), { decompress: false });
    expect(response.headers.get("content-encoding")).toBe("gzip");
    expect(response.headers.get("content-length")).toBe(String(archive.byteLength));
    expect(response.headers.get("content-disposition")).toBe('attachment; filename="world.bin"');
    expect(new Uint8Array(await response.arrayBuffer())).toEqual(archive);
  });

  it("streams upload chunks before completion and cancels the backend read when the browser aborts", async () => {
    const firstChunk = deferred<string>();
    const backendCancelled = deferred<void>();
    const backend = keepServer(Bun.serve({
      hostname: "127.0.0.1", port: 0, idleTimeout: 0,
      async fetch(request) {
        request.signal.addEventListener("abort", () => backendCancelled.resolve(), { once: true });
        const reader = request.body!.getReader();
        try {
          const first = await reader.read();
          firstChunk.resolve(new TextDecoder().decode(first.value));
          while (!(await reader.read()).done) { /* consume subsequent chunks */ }
        } catch {
          backendCancelled.resolve();
        } finally {
          reader.releaseLock();
        }
        return new Response("upload ended");
      },
    }));
    const frontend = startProxy(backend.url);
    const controller = new AbortController();
    cleanup.push(() => controller.abort());
    const upload = fetch(new URL("/api/v1/files/upload", frontend.url), {
      method: "POST", signal: controller.signal,
      body: new ReadableStream({
        start(stream) { stream.enqueue(new TextEncoder().encode("first upload chunk")); },
      }),
    }).then(() => "completed", () => "aborted");
    expect(await received(firstChunk.promise, "a streamed upload chunk")).toBe("first upload chunk");
    controller.abort();
    expect(await received(upload, "the upload cancellation")).toBe("aborted");
    await received(backendCancelled.promise, "the backend upload cancellation");
  });

  it("cancels an unfinished backend download when the browser disconnects", async () => {
    const backendCancelled = deferred<void>();
    const backend = keepServer(Bun.serve({
      hostname: "127.0.0.1", port: 0, idleTimeout: 0,
      fetch: () => new Response(new ReadableStream({
        start(stream) { stream.enqueue(new TextEncoder().encode("first download chunk")); },
        cancel() { backendCancelled.resolve(); },
      })),
    }));
    const frontend = startProxy(backend.url);
    const controller = new AbortController();
    cleanup.push(() => controller.abort());
    const response = await fetch(new URL("/api/v1/files/download", frontend.url), { signal: controller.signal });
    const reader = response.body!.getReader();
    expect(new TextDecoder().decode((await reader.read()).value)).toBe("first download chunk");
    controller.abort();
    await reader.cancel().catch(() => {});
    await received(backendCancelled.promise, "the backend download cancellation");
  });
});

describe("development WebSocket proxy", () => {
  it("does not open a browser connection when the upstream rejects its handshake", async () => {
    const backend = keepServer(Bun.serve({
      hostname: "127.0.0.1", port: 0,
      fetch: () => new Response("Private backend rejection details", { status: 401 }),
    }));
    const frontend = startProxy(backend.url);
    const client = connect(new URL("/ws/v1/console", frontend.url));
    const opened = client.opened.then(() => true, () => false);
    expect(await received(opened, "the rejected handshake")).toBe(false);
    expect((await received(client.closed, "the browser connection close")).reason).not.toContain("Private backend");
  });

  it("waits for the upstream handshake, preserves its initial message and forwards text and binary frames", async () => {
    const handshake = deferred<void>();
    const allowHandshake = deferred<void>();
    let incoming: Record<string, string | null> | undefined;
    const backend = keepServer(Bun.serve({
      hostname: "127.0.0.1", port: 0,
      async fetch(request, server) {
        const url = new URL(request.url);
        incoming = {
          path: `${url.pathname}${url.search}`, host: request.headers.get("host"),
          origin: request.headers.get("origin"), cookie: request.headers.get("cookie"),
          instance: request.headers.get("x-ludock-dev-instance"),
        };
        handshake.resolve();
        await allowHandshake.promise;
        if (!server.upgrade(request)) return new Response("Upgrade failed", { status: 400 });
      },
      websocket: {
        open(socket) { socket.send("initial console output"); },
        message(socket, data) { socket.send(data); },
      },
    }));
    const frontend = startProxy(backend.url);
    const client = connect(new URL("/ws/v1/console?server=one&mode=logs", frontend.url), {
      Origin: frontend.url.origin,
      Cookie: "ludock_session=production; ludock_session_checkout-b=peer; ludock_session_checkout-a=selected",
      "X-Ludock-Dev-Instance": "wrong-checkout",
    });
    await received(handshake.promise, "the upstream handshake");
    expect(client.socket.readyState).toBe(WebSocket.CONNECTING);
    allowHandshake.resolve();
    await received(client.opened, "the browser WebSocket open");
    expect(incoming).toEqual({
      path: "/ws/v1/console?server=one&mode=logs", host: frontend.url.host, origin: frontend.url.origin,
      cookie: "ludock_session_checkout-a=selected", instance: "checkout-a",
    });
    expect(await received(client.nextMessage(), "initial console output")).toBe("initial console output");
    client.socket.send("say hello");
    expect(await received(client.nextMessage(), "a text echo")).toBe("say hello");
    const binary = new Uint8Array([0, 255, 32, 127, 128]);
    client.socket.send(binary);
    const echo = await received(client.nextMessage(), "a binary echo");
    expect(echo).toBeInstanceOf(ArrayBuffer);
    expect(new Uint8Array(echo as ArrayBuffer)).toEqual(binary);
  });

  it("propagates backend policy denial as close code 1008", async () => {
    const backend = keepServer(Bun.serve({
      hostname: "127.0.0.1", port: 0,
      fetch(request, server) {
        if (!server.upgrade(request)) return new Response("Upgrade failed", { status: 400 });
      },
      websocket: { message(socket) { socket.close(1008, "Access revoked"); } },
    }));
    const frontend = startProxy(backend.url);
    const client = connect(new URL("/ws/v1/console", frontend.url));
    await received(client.opened, "the browser WebSocket open");
    client.socket.send("check access");
    expect(await received(client.closed, "the policy close")).toEqual({ code: 1008, reason: "Access revoked" });
  });

  it("closes the upstream connection when the browser leaves", async () => {
    const upstreamClosed = deferred<number>();
    const backend = keepServer(Bun.serve({
      hostname: "127.0.0.1", port: 0,
      fetch(request, server) {
        if (!server.upgrade(request)) return new Response("Upgrade failed", { status: 400 });
      },
      websocket: {
        message() {},
        close(_socket, code) { upstreamClosed.resolve(code); },
      },
    }));
    const frontend = startProxy(backend.url);
    const client = connect(new URL("/ws/v1/console", frontend.url));
    await received(client.opened, "the browser WebSocket open");
    client.socket.close(1000, "Leaving console");
    expect(await received(upstreamClosed.promise, "the upstream close")).toBe(1000);
  });
});
