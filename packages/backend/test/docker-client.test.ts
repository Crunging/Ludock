import { fixtureBytes } from "./fixtures/bytes.js";
import { byteView, concatBytes, decodeText, encodeText } from "../src/bytes.js";
import { afterEach, describe, expect, it, mock, spyOn } from "bun:test";
import { rm } from "node:fs/promises";
import { bytesStream } from "./fixtures/web-streams.js";
import { DockerClient } from "../src/docker-client.js";
import { DockerTransport } from "../src/docker-transport.js";
import { demuxDockerStream } from "../src/docker-stream.js";

const cleanup: Array<() => void | Promise<void>> = [];
afterEach(async () => {
  mock.restore();
  for (const close of cleanup.splice(0).reverse()) await close();
});

function socketPath(): string {
  const path = `/tmp/ludock-docker-${crypto.randomUUID()}.sock`;
  cleanup.push(() => rm(path, { force: true }));
  return path;
}

function httpFixture(handler: (request: Request) => Response | Promise<Response>) {
  const path = socketPath();
  const server = Bun.serve({ unix: path, fetch: handler });
  cleanup.push(() => server.stop(true));
  return new DockerClient({ socketPath: path });
}

const version = () => Response.json({ ApiVersion: "1.55", MinAPIVersion: "1.40" });
const isVersion = (request: Request) => new URL(request.url).pathname === "/version";
function frame(channel: number, value: string | Uint8Array): Uint8Array {
  const payload = fixtureBytes(value);
  const header = new Uint8Array(8);
  header[0] = channel;
  byteView(header).setUint32(4, payload.length);
  return concatBytes([header, payload]);
}

describe("Bun Docker HTTP client", () => {
  it("keeps daemon commands on the Unix socket when proxy environment variables are set", async () => {
    const keys = ["HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"];
    const previous = keys.map((key) => process.env[key]);
    let proxied = false;
    const proxy = Bun.serve({ hostname: "127.0.0.1", port: 0, fetch() { proxied = true; return new Response(null, { status: 502 }); } });
    const client = httpFixture((request) => isVersion(request) ? version() : new Response(null, { status: 204 }));
    try {
      for (const key of keys) process.env[key] = key.toLowerCase() === "no_proxy" ? "" : proxy.url.href;
      await client.getContainer("fixture").start();
      expect(proxied).toBe(false);
    } finally {
      keys.forEach((key, index) => { if (previous[index] === undefined) delete process.env[key]; else process.env[key] = previous[index]; });
      await proxy.stop(true);
    }
  });

  for (const status of [301, 500]) {
    it(`preserves HTTP ${status} without disclosing daemon text, following redirects, or retrying`, async () => {
      let count = 0;
      const client = httpFixture((request) => {
        if (isVersion(request)) return version();
        count++;
        return new Response('{"message":"fixture-secret"}', {
          status, headers: { Location: "http://127.0.0.1:1/private" },
        });
      });
      await expect(client.getContainer("fixture").start()).rejects.toMatchObject({
        statusCode: status, message: `Docker API request failed (HTTP ${status})`,
      });
      expect(count).toBe(1);
    });
  }

  it("retries failed negotiation on the next request and rejects identifiers before dispatch", async () => {
    let calls = 0;
    const client = httpFixture((request) => {
      calls++;
      if (calls === 1) return new Response(null, { status: 503 });
      return isVersion(request) ? version() : Response.json([]);
    });
    for (const id of ["../info", "a/b", "a?force=1", "a#fragment", "a\r\nHeader", "%2f", ""]) {
      expect(() => client.getContainer(id)).toThrow();
      expect(() => client.getVolume(id)).toThrow();
    }
    expect(calls).toBe(0);
    await expect(client.listContainers()).rejects.toMatchObject({ statusCode: 503 });
    expect(await client.listContainers()).toEqual([]);
    expect(calls).toBe(3);
    expect(() => client.getVolume("volume-" + "a".repeat(200))).not.toThrow();
  });

  it("bounds stalled read headers and bodies and accepts a later healthy read", async () => {
    const headers = Promise.withResolvers<Response>();
    let body: ReadableStreamDefaultController<Uint8Array> | undefined;
    const client = httpFixture(request => {
      const path = new URL(request.url).pathname;
      if (path === "/headers") return headers.promise;
      if (path === "/body") return new Response(new ReadableStream<Uint8Array>({
        start(controller) { body = controller; controller.enqueue(encodeText("[")); },
      }));
      return Response.json([]);
    });
    const transport = new DockerTransport(client.transport.socketPath, 25);
    try {
      await expect(transport.json("/headers")).rejects.toThrow("Docker read timed out");
      await expect(transport.json("/body")).rejects.toThrow("Docker read timed out");
      expect(await transport.json<unknown[]>("/healthy")).toEqual([]);
    } finally {
      headers.resolve(Response.json([]));
      try { body?.close(); } catch { /* The timed-out response was cancelled. */ }
    }
  });

  it("rejects malformed daemon JSON without including its contents", async () => {
    const client = httpFixture((request) => isVersion(request) ? version() : new Response("fixture-secret, invalid JSON"));
    await expect(client.listContainers()).rejects.toThrow("Docker returned invalid JSON");
  });

  it("checks fragmented pull progress through the final record and encodes the complete image reference", async () => {
    const image = "registry.example:5000/bun:1@sha256:" + "a".repeat(64);
    let requested = "";
    const client = httpFixture((request) => {
      if (isVersion(request)) return version();
      requested = new URL(request.url).searchParams.get("fromImage")!;
      return new Response(new ReadableStream({
        start(controller) {
          for (const part of ['{"status":"pull', 'ing 🌍"}\n{"status":"done"}']) controller.enqueue(fixtureBytes(part));
          controller.close();
        },
      }));
    });
    await client.pull(image);
    expect(requested).toBe(image);
  });

  for (const progress of ['', '{"error":"fixture-secret"}\n', '{"status":"pulling"}\n{"errorDetail":{"message":"fixture-secret"}}', '{"status":', '[]', 'a'.repeat(1_048_577)]) {
    it(`rejects failed, malformed, or oversized pull progress (${progress.length} bytes)`, async () => {
      const client = httpFixture((request) => isVersion(request) ? version() : new Response(progress));
      const error = await client.pull("fixture").catch((error: unknown) => error);
      expect(error).toBeInstanceOf(Error);
      expect((error as Error).message).not.toContain("fixture-secret");
    });
  }

  for (const kind of ["logs", "events"] as const) {
    it(`cancels the native ${kind} request when its reader is cancelled`, async () => {
      const cancelled = Promise.withResolvers<void>();
      const client = httpFixture((request) => {
        if (isVersion(request)) return version();
        return new Response(new ReadableStream({
          start(controller) { controller.enqueue(fixtureBytes("first chunk\n")); },
          cancel() { cancelled.resolve(); },
        }));
      });
      const stream = kind === "logs" ? await client.getContainer("fixture").logs({ follow: true, stdout: true }) : await client.getEvents();
      const reader = stream.getReader();
      expect(new TextDecoder().decode((await reader.read()).value)).toBe("first chunk\n");
      await reader.cancel();
      reader.releaseLock();
      await cancelled.promise;
    });
  }
});

interface UpgradePeer {
  write(data: string | Uint8Array): void;
  end(data?: string | Uint8Array): void;
  pause(): void;
  resume(): void;
  onData?: (data: Uint8Array) => void;
  onEnd?: () => void;
}

/** A native Unix peer exercises the actual upgrade, half-close and backpressure. */
async function upgradeFixture(reply: (peer: UpgradePeer, request: Uint8Array) => void): Promise<DockerTransport> {
  const path = socketPath();
  type State = { request: Uint8Array; upgraded: boolean; peer: UpgradePeer; flush(): void };
  const listener = Bun.listen<State>({
    unix: path,
    allowHalfOpen: true,
    socket: {
      binaryType: "uint8array",
      open(socket) {
        const queued: Array<{ bytes: Uint8Array; offset: number }> = [];
        let ending = false;
        const flush = () => {
          while (queued.length) {
            const first = queued[0];
            const count = socket.write(first.bytes, first.offset, first.bytes.length - first.offset);
            if (count < 0) { socket.terminate(); return; }
            first.offset += count;
            if (first.offset < first.bytes.length) return;
            queued.shift();
          }
          if (ending) socket.shutdown();
        };
        const write = (value: string | Uint8Array) => {
          queued.push({ bytes: typeof value === "string" ? encodeText(value) : value, offset: 0 });
          flush();
        };
        socket.data = {
          request: new Uint8Array(), upgraded: false, flush,
          peer: {
            write,
            end(value) { if (value !== undefined) write(value); ending = true; flush(); },
            pause() { socket.pause(); },
            resume() { socket.resume(); },
          },
        };
      },
      data(socket, bytes) {
        const state = socket.data;
        if (state.upgraded) { state.peer.onData?.(bytes); return; }
        state.request = concatBytes([state.request, bytes]);
        const text = decodeText(state.request);
        const boundary = text.indexOf("\r\n\r\n");
        if (boundary === -1) return;
        const size = Number(/Content-Length: (\d+)/i.exec(text.slice(0, boundary))?.[1] || 0);
        const body = encodeText(text.slice(0, boundary + 4)).length;
        if (state.request.length < body + size) return;
        state.upgraded = true;
        reply(state.peer, state.request);
      },
      drain(socket) { socket.data.flush(); },
      end(socket) { if (socket.data.peer.onEnd) socket.data.peer.onEnd(); else socket.end(); },
      error(socket) { socket.terminate(); },
    },
  });
  cleanup.push(() => listener.stop(true));
  return new DockerTransport(path);
}

const upgrade = "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n";

describe("Bun Docker duplex transport", () => {
  for (const status of [101, 200]) {
    it(`preserves fragmented ${status} headers and output sharing the final header chunk`, async () => {
      let request = "";
      const transport = await upgradeFixture((socket, received) => {
        request = decodeText(received);
        const header = status === 101 ? upgrade : "HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n";
        socket.write(header.slice(0, -1));
        setTimeout(() => socket.end(concatBytes([fixtureBytes(header.slice(-1)), frame(1, "hello 🌍"), frame(2, "diagnostic")])), 5);
      });
      const stream = await transport.hijack("/v1.55/exec/fixture/start", { Detach: false, Tty: false });
      let out = "", err = "";
      await demuxDockerStream(stream.readable, data => { out += decodeText(fixtureBytes(data)); }, data => { err += decodeText(fixtureBytes(data)); });
      expect(out).toBe("hello 🌍");
      expect(err).toBe("diagnostic");
      expect(request).toStartWith("POST /v1.55/exec/fixture/start HTTP/1.1\r\n");
      expect(request.split("\r\n\r\n")[1]).toBe('{"Detach":false,"Tty":false}');
    });
  }

  it("drains large stdin writes and receives a helper result after half-closing stdin", async () => {
    const payload = new Uint8Array(8 * 1024 * 1024).fill(97);
    let received = 0;
    const transport = await upgradeFixture((socket) => {
      socket.write(upgrade);
      socket.pause();
      setTimeout(() => socket.resume(), 25);
      socket.onData = (data: Uint8Array) => { received += data.length; };
      socket.onEnd = () => { socket.end(frame(1, String(received))); };
    });
    const stream = await transport.hijack("/exec/fixture/start", {});
    let output = "";
    const completed = demuxDockerStream(stream.readable, data => { output += decodeText(fixtureBytes(data)); });
    const writer = stream.writable.getWriter();
    await writer.write(payload);
    await writer.close();
    writer.releaseLock();
    await completed;
    expect(received).toBe(payload.length);
    expect(output).toBe(String(payload.length));
  });

  it("terminates the native connection when an attached stream is cancelled", async () => {
    const closed = Promise.withResolvers<void>();
    const transport = await upgradeFixture((socket) => {
      socket.write(upgrade);
      socket.onEnd = () => { socket.end(); closed.resolve(); };
    });
    const stream = await transport.hijack("/containers/fixture/attach?stdin=true");
    stream.abort();
    await closed.promise;
  });

  for (const direction of ["read", "connection"] as const) it(`rejects a blocked stdin write when ${direction} is cancelled`, async () => {
    const transport = await upgradeFixture(socket => {
      socket.write(upgrade);
      socket.pause();
    });
    const stream = await transport.hijack("/exec/fixture/start", {});
    const writer = stream.writable.getWriter();
    void writer.closed.catch(() => {});
    const writing = writer.write(new Uint8Array(8 * 1024 * 1024).fill(97));
    void writing.catch(() => {});
    await Bun.sleep(10);
    if (direction === "read") await stream.readable.cancel(new Error("Client disconnected"));
    else stream.abort(new Error("Client disconnected"));
    await expect(writing).rejects.toThrow("Client disconnected");
    writer.releaseLock();
  });

  it("finishes the request before accepting stdin after an early upgrade response", async () => {
    let handlers: Bun.SocketHandler<undefined, "uint8array">;
    let written = "";
    let release!: () => void;
    let first = true;
    const socket = {
      write(data: Uint8Array, offset = 0, length = data.length) {
        const count = first ? 2 : length;
        first = false;
        written += decodeText(fixtureBytes(data.subarray(offset, offset + count)));
        return count;
      },
      terminate() {}, resume() {}, pause() {}, shutdown() {},
    } as unknown as Bun.Socket<undefined>;
    spyOn(Bun, "connect").mockImplementation(options => {
      // Bun types model only the default Buffer binary type.
      handlers = options.socket as unknown as Bun.SocketHandler<undefined, "uint8array">;
      queueMicrotask(() => {
        handlers.open!(socket);
        handlers.data!(socket, fixtureBytes(upgrade));
        release = () => handlers.drain!(socket);
      });
      return Promise.resolve(socket);
    });
    let delivered = false;
    const opening = new DockerTransport("/fixture.sock").hijack("/exec/id/start", { Detach: false });
    void opening.then(() => { delivered = true; });
    await new Promise<void>(resolve => setImmediate(resolve));
    expect(delivered).toBe(false);
    release();
    const stream = await opening;
    const writer = stream.writable.getWriter();
    await writer.write(fixtureBytes("stdin"));
    expect(written).toEndWith('{"Detach":false}stdin');
    writer.releaseLock();
    stream.abort();
  });

  for (const reply of ["HTTP/1.1 403 Forbidden\r\n\r\nfixture-secret", "HTTP/1.1 101 UPGRADED\r\nUpgrade: websocket\r\n\r\n", "HTTP/1.1 200 OK\r\nContent-Type: text/html\r\n\r\n", "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nTransfer-Encoding: chunked\r\n\r\n", "HTTP/1.1", "x".repeat(16_385)]) {
    it(`rejects invalid, refused, or incomplete upgrades (${reply.length} bytes)`, async () => {
      const transport = await upgradeFixture((socket) => socket.end(reply));
      const error = await transport.hijack("/exec/fixture/start", {}).catch((error: unknown) => error);
      expect(error).toBeInstanceOf(Error);
      expect((error as Error).message).not.toContain("fixture-secret");
      if (reply.includes("403")) expect(error).toHaveProperty("statusCode", 403);
      else expect(error).toHaveProperty("requestStarted", true);
    });
  }

  it("resumes a partial native write at the exact byte offset before reporting completion", async () => {
    let handlers: Bun.SocketHandler<undefined, "uint8array">;
    let written = new Uint8Array(0);
    let short = false;
    const socket = {
      write(data: Uint8Array, offset = 0, length = data.length) {
        const count = short ? Math.min(2, length) : length;
        written = concatBytes([written, data.subarray(offset, offset + count)]);
        if (count < length) queueMicrotask(() => handlers.drain!(socket));
        return count;
      },
      terminate() {}, resume() {}, pause() {}, shutdown() {},
    } as unknown as Bun.Socket<undefined>;
    spyOn(Bun, "connect").mockImplementation((options) => {
      // Bun types model only the default Buffer binary type.
      handlers = options.socket as unknown as Bun.SocketHandler<undefined, "uint8array">;
      queueMicrotask(() => { handlers.open!(socket); handlers.data!(socket, fixtureBytes(upgrade)); });
      return Promise.resolve(socket);
    });
    const stream = await new DockerTransport("/fixture.sock").hijack("/exec/id/start");
    short = true;
    written = new Uint8Array(0);
    const writer = stream.writable.getWriter();
    await writer.write(fixtureBytes("abcdefghij"));
    writer.releaseLock();
    expect(decodeText(written)).toBe("abcdefghij");
    stream.abort();
  });
});

describe("Docker output framing", () => {

  it("decodes bytewise headers and bodies, drops stdin, and rejects incomplete output", async () => {
    const bytes = concatBytes([frame(0, "ignored"), frame(1, "stdout"), frame(2, "stderr"), frame(1, "")]);
    let stdout = "", stderr = "";
    await demuxDockerStream(bytesStream([...bytes].map(value => fixtureBytes([value]))),
      data => { stdout += decodeText(fixtureBytes(data)); },
      data => { stderr += decodeText(fixtureBytes(data)); });
    expect({ stdout, stderr }).toEqual({ stdout: "stdout", stderr: "stderr" });
    for (const data of [bytes.subarray(0, 3), frame(1, "lost").subarray(0, 10), frame(3, "invalid")])
      await expect(demuxDockerStream(bytesStream([data]))).rejects.toThrow();
  });

  it("retains backpressure until the consumer accepts output and cancels on consumer failure", async () => {
    const gate = Promise.withResolvers<void>();
    const blocked = Promise.withResolvers<void>();
    let cancelled = false;
    const source = new ReadableStream<Uint8Array>({
      start(controller) { controller.enqueue(concatBytes([frame(1, "first"), frame(1, "second")])); },
      cancel() { cancelled = true; },
    });
    let writes = 0;
    const completed = demuxDockerStream(source, async () => { writes++; blocked.resolve(); await gate.promise; });
    await blocked.promise;
    expect(writes).toBe(1);
    gate.reject(new Error("Consumer closed"));
    await expect(completed).rejects.toThrow("Consumer closed");
    expect(cancelled).toBe(true);
  });
});
