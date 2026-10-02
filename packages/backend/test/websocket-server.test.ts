import { serve } from "bun";
import { expect, afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";
import { createSession } from "../src/auth.js";
import { closeDatabase, createUser, deleteUserSessions } from "../src/database.js";
import { docker } from "../src/docker-client.js";
import { stopEventStream } from "../src/events.js";
import { NativeSocketChannel, MAX_SOCKET_BUFFER_BYTES } from "../src/socket-channel.js";
import {
  createWebSocketGateway,
  MAX_WEBSOCKET_CONNECTIONS_PER_SESSION,
  MAX_WEBSOCKET_CONNECTIONS_PER_USER,
  MAX_WEBSOCKET_PAYLOAD_BYTES,
} from "../src/websocket-server.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const apiToken = "native-websocket-fixture-token-0123456789";
process.env.LUDOCK_API_TOKEN = apiToken;
const originalEvents = docker.getEvents;
const clients: WebSocket[] = [];
let gateway: ReturnType<typeof createWebSocketGateway>;
let server: ReturnType<typeof startFixture>;

function startFixture() {
  return serve({
    hostname: "127.0.0.1", port: 0,
    websocket: gateway.websocket,
    fetch(request, server) { return gateway.upgrade(request, server); },
  });
}

beforeEach(() => {
  closeDatabase();
  docker.getEvents = (async () => new ReadableStream<Uint8Array>()) as typeof docker.getEvents;
  createUser({ id: "viewer", username: "viewer", role: "viewer", disabled: false, passwordHash: "fixture", createdAt: 1 });
  gateway = createWebSocketGateway();
  server = startFixture();
});

afterEach(async () => {
  for (const client of clients.splice(0)) client.terminate();
  await gateway.close();
  await server.stop(true);
  await stopEventStream();
  mock.restore();
  docker.getEvents = originalEvents;
  closeDatabase();
});

async function connect(headers: Record<string, string> = { Authorization: `Bearer ${apiToken}` }) {
  const client = new WebSocket(new URL("/ws/v1/events", server.url).href.replace(/^http/, "ws"), { headers });
  clients.push(client);
  await new Promise<void>((resolve, reject) => {
    client.addEventListener("open", () => resolve(), { once: true });
    client.addEventListener("error", () => reject(new Error("Fixture upgrade failed")), { once: true });
  });
  return client;
}

function closed(client: WebSocket): Promise<number> {
  return new Promise((resolve) => client.addEventListener("close", (event) => resolve(event.code), { once: true }));
}

function upgradeStatus(path: string, headers: Record<string, string> = {}) {
  return fetch(new URL(path, server.url), {
    headers: { Upgrade: "websocket", Connection: "Upgrade", ...headers },
  }).then((response) => response.status);
}

describe("native WebSocket admission and lifetime", () => {
  it("requires authentication, same-origin browser sessions, and administrator shell access", async () => {
    expect(await upgradeStatus("/ws/v1/events")).toBe(401);
    expect(await upgradeStatus("/ws/v1/events", {
      Authorization: `Bearer ${apiToken}`, Origin: "https://untrusted.example",
    })).toBe(401);
    expect(await upgradeStatus("/ws/v1/unknown", { Authorization: `Bearer ${apiToken}` })).toBe(404);
    const session = createSession({ id: "viewer", username: "viewer", role: "viewer" }, new Request(String(server.url)));
    expect(await upgradeStatus("/ws/v1/shell/server", {
      Cookie: `ludock_session=${session.token}`, Origin: server.url.origin,
    })).toBe(403);
    expect(gateway.connectionCount).toBe(0);
  });

  it("closes a real browser session before handling input after revocation", async () => {
    const session = createSession({ id: "viewer", username: "viewer", role: "viewer" }, new Request(String(server.url)));
    const client = await connect({ Cookie: `ludock_session=${session.token}`, Origin: server.url.origin });
    const closing = closed(client);
    deleteUserSessions("viewer");
    client.send("{}");
    expect(await closing).toBe(1008);
    expect(gateway.connectionCount).toBe(0);
  });

  it("rejects an oversized native WebSocket message", async () => {
    let accepted!: () => void;
    const delivered = new Promise<void>((resolve) => { accepted = resolve; });
    const receive = NativeSocketChannel.prototype.receive;
    const messages = spyOn(NativeSocketChannel.prototype, "receive").mockImplementation(function (this: NativeSocketChannel, message) {
      receive.call(this, message);
      accepted();
    });
    const client = await connect();
    client.send(new Uint8Array(MAX_WEBSOCKET_PAYLOAD_BYTES));
    await delivered;
    const closing = closed(client);
    client.send(new Uint8Array(MAX_WEBSOCKET_PAYLOAD_BYTES + 1));
    // Bun may abort the transport before sending a 1009 close frame. In either
    // case the oversized payload must never reach a business handler.
    expect([1006, 1009].includes(await closing)).toBeTruthy();
    expect(messages.mock.calls.length).toBe(1);
    expect(gateway.connectionCount).toBe(0);
  });

  it("isolates users and sessions and releases their closed slots", async () => {
    const first = sessionHeaders("viewer");
    const connected = await Promise.all(Array.from(
      { length: MAX_WEBSOCKET_CONNECTIONS_PER_SESSION },
      () => connect(first),
    ));
    expect(await upgradeStatus("/ws/v1/events", first)).toBe(429);

    const second = sessionHeaders("viewer");
    await Promise.all(Array.from(
      {
        length: MAX_WEBSOCKET_CONNECTIONS_PER_USER -
          MAX_WEBSOCKET_CONNECTIONS_PER_SESSION,
      },
      () => connect(second),
    ));
    expect(await upgradeStatus("/ws/v1/events", second)).toBe(429);

    createUser({ id: "admin", username: "admin", role: "admin", disabled: false, passwordHash: "fixture", createdAt: 1 });
    await connect(sessionHeaders("admin", "admin"));
    expect(gateway.connectionCount).toBe(MAX_WEBSOCKET_CONNECTIONS_PER_USER + 1);

    const closing = closed(connected[0]);
    connected[0].close();
    await closing;
    await connect(first);
    expect(gateway.connectionCount).toBe(MAX_WEBSOCKET_CONNECTIONS_PER_USER + 1);
  });
});

function sessionHeaders(id: string, role: "viewer" | "admin" = "viewer") {
  const session = createSession(
    { id, username: id, role },
    new Request(String(server.url)),
  );
  return {
    Cookie: `ludock_session=${session.token}`,
    Origin: server.url.origin,
  };
}

describe("native socket output bounds", () => {
  it("disconnects a slow reader and releases its stream before another frame can be sent", () => {
    let cleaned = 0;
    const sent: string[] = [];
    const closes: number[] = [];
    const channel = new NativeSocketChannel({
      readyState: 1,
      getBufferedAmount: () => MAX_SOCKET_BUFFER_BYTES - 1,
      sendText: (message) => { sent.push(message); return 1; },
      close: (code) => { closes.push(code!); },
    });
    channel.onClose(() => { cleaned++; });
    channel.send("too much output");
    channel.send("later output");
    channel.finish(1006);
    expect(channel.isOpen).toBe(false);
    expect(cleaned).toBe(1);
    expect(closes).toStrictEqual([1013]);
    expect(sent).toStrictEqual([]);
  });
});
