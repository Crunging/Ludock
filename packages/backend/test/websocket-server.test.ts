import assert from "node:assert/strict";
import { serve } from "bun";
import { afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";
import { createSession } from "../src/auth.js";
import { closeDatabase, createUser, deleteUserSessions } from "../src/database.js";
import { getDockerInstance } from "../src/docker.js";
import { stopEventStream } from "../src/events.js";
import { NativeSocketChannel, MAX_SOCKET_BUFFER_BYTES } from "../src/socket-channel.js";
import {
  createWebSocketGateway,
  MAX_WEBSOCKET_CONNECTIONS,
  MAX_WEBSOCKET_CONNECTIONS_PER_SESSION,
  MAX_WEBSOCKET_CONNECTIONS_PER_USER,
  MAX_WEBSOCKET_PAYLOAD_BYTES,
  RESERVED_ADMIN_WEBSOCKET_CONNECTIONS,
} from "../src/websocket-server.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const apiToken = "native-websocket-fixture-token-0123456789";
process.env.LUDOCK_API_TOKEN = apiToken;
const docker = getDockerInstance();
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
    assert.equal(await upgradeStatus("/ws/v1/events"), 401);
    assert.equal(await upgradeStatus("/ws/v1/events", {
      Authorization: `Bearer ${apiToken}`, Origin: "https://untrusted.example",
    }), 401);
    assert.equal(await upgradeStatus("/ws/v1/unknown", { Authorization: `Bearer ${apiToken}` }), 404);
    const session = createSession({ id: "viewer", username: "viewer", role: "viewer" }, new Request(server.url));
    assert.equal(await upgradeStatus("/ws/v1/shell/server", {
      Cookie: `ludock_session=${session.token}`, Origin: server.url.origin,
    }), 403);
    assert.equal(gateway.connectionCount, 0);
  });

  it("closes a real browser session before handling input after revocation", async () => {
    const session = createSession({ id: "viewer", username: "viewer", role: "viewer" }, new Request(server.url));
    const client = await connect({ Cookie: `ludock_session=${session.token}`, Origin: server.url.origin });
    const closing = closed(client);
    deleteUserSessions("viewer");
    client.send("{}");
    assert.equal(await closing, 1008);
    assert.equal(gateway.connectionCount, 0);
  });

  it("rejects an oversized native WebSocket message", async () => {
    let accepted!: () => void;
    const delivered = new Promise<void>((resolve) => { accepted = resolve; });
    const receive = NativeSocketChannel.prototype.receive;
    const messages = spyOn(NativeSocketChannel.prototype, "receive").mockImplementation(function (message) {
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
    assert.ok([1006, 1009].includes(await closing));
    assert.equal(messages.mock.calls.length, 1);
    assert.equal(gateway.connectionCount, 0);
  });

  it("isolates users and sessions and releases their closed slots", async () => {
    const first = sessionHeaders("viewer");
    const connected = await Promise.all(Array.from(
      { length: MAX_WEBSOCKET_CONNECTIONS_PER_SESSION },
      () => connect(first),
    ));
    assert.equal(await upgradeStatus("/ws/v1/events", first), 429);

    const second = sessionHeaders("viewer");
    await Promise.all(Array.from(
      {
        length: MAX_WEBSOCKET_CONNECTIONS_PER_USER -
          MAX_WEBSOCKET_CONNECTIONS_PER_SESSION,
      },
      () => connect(second),
    ));
    assert.equal(await upgradeStatus("/ws/v1/events", second), 429);

    createUser({ id: "admin", username: "admin", role: "admin", disabled: false, passwordHash: "fixture", createdAt: 1 });
    await connect(sessionHeaders("admin", "admin"));
    assert.equal(gateway.connectionCount,
      MAX_WEBSOCKET_CONNECTIONS_PER_USER + 1);

    const closing = closed(connected[0]);
    connected[0].close();
    await closing;
    await connect(first);
    assert.equal(gateway.connectionCount,
      MAX_WEBSOCKET_CONNECTIONS_PER_USER + 1);
  });

  it("retains the global cap across many administrator principals", async () => {
    const headers: Record<string, string>[] = [];
    for (let index = 0; headers.length < MAX_WEBSOCKET_CONNECTIONS; index++) {
      const id = `administrator-${index}`;
      createUser({ id, username: id, role: "admin", disabled: false, passwordHash: "fixture", createdAt: 1 });
      const credentials = sessionHeaders(id, "admin");
      for (let count = 0;
        count < MAX_WEBSOCKET_CONNECTIONS_PER_SESSION &&
          headers.length < MAX_WEBSOCKET_CONNECTIONS;
        count++) headers.push(credentials);
    }
    await Promise.all(headers.map((credentials) => connect(credentials)));
    assert.equal(gateway.connectionCount, MAX_WEBSOCKET_CONNECTIONS);
    createUser({ id: "later-admin", username: "later-admin", role: "admin", disabled: false, passwordHash: "fixture", createdAt: 1 });
    assert.equal(await upgradeStatus(
      "/ws/v1/events",
      sessionHeaders("later-admin", "admin"),
    ), 503);
  });

  it("reserves global capacity for administrators", async () => {
    const nonAdminLimit = MAX_WEBSOCKET_CONNECTIONS -
      RESERVED_ADMIN_WEBSOCKET_CONNECTIONS;
    const headers: Record<string, string>[] = [];
    for (let index = 0; headers.length < nonAdminLimit; index++) {
      const id = `viewer-${index}`;
      createUser({ id, username: id, role: "viewer", disabled: false, passwordHash: "fixture", createdAt: 1 });
      const credentials = sessionHeaders(id);
      for (let count = 0;
        count < MAX_WEBSOCKET_CONNECTIONS_PER_SESSION &&
          headers.length < nonAdminLimit;
        count++) headers.push(credentials);
    }
    await Promise.all(headers.map((credentials) => connect(credentials)));
    createUser({ id: "later-viewer", username: "later-viewer", role: "viewer", disabled: false, passwordHash: "fixture", createdAt: 1 });
    assert.equal(await upgradeStatus(
      "/ws/v1/events",
      sessionHeaders("later-viewer"),
    ), 503);

    createUser({ id: "reserved-admin", username: "reserved-admin", role: "admin", disabled: false, passwordHash: "fixture", createdAt: 1 });
    await connect(sessionHeaders("reserved-admin", "admin"));
    assert.equal(gateway.connectionCount, nonAdminLimit + 1);
  });

  it("sends shutdown close frames and rejects further admission", async () => {
    const client = await connect();
    const closing = closed(client);
    await gateway.close();
    assert.equal(await closing, 1001);
    assert.equal(gateway.connectionCount, 0);
    assert.equal(await upgradeStatus("/ws/v1/events", { Authorization: `Bearer ${apiToken}` }), 503);
  });
});

function sessionHeaders(id: string, role: "viewer" | "admin" = "viewer") {
  const session = createSession(
    { id, username: id, role },
    new Request(server.url),
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
    assert.equal(channel.isOpen, false);
    assert.equal(cleaned, 1);
    assert.deepEqual(closes, [1013]);
    assert.deepEqual(sent, []);
  });

  it("accepts queued native sends without duplicating the frame", () => {
    const sent: string[] = [];
    const channel = new NativeSocketChannel({
      readyState: 1,
      getBufferedAmount: () => 0,
      sendText: (message) => { sent.push(message); return -1; },
      close() { assert.fail("A queued frame below the limit should stay connected"); },
    });
    channel.send("queued output");
    assert.equal(channel.isOpen, true);
    assert.deepEqual(sent, ["queued output"]);
  });
});
