import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import { afterEach, beforeEach, describe, it } from "bun:test";
import type { SocketChannel, SocketMessage } from "../src/socket-channel.js";
import {
  addEventClient,
  dispatchDockerEvent,
  dockerEventDecoder,
  stopEventStream,
} from "../src/events.js";
import { getDockerInstance } from "../src/docker.js";
import {
  closeDatabase,
  createUser,
  updateUserAccess,
  type SessionUser,
} from "../src/database.js";
import { setServerGrant } from "../src/authorization.js";
import { listLogicalServers } from "../src/identity.js";
import { refreshServers } from "../src/servers.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const docker = getDockerInstance();
const originals = {
  getEvents: docker.getEvents,
  getContainer: docker.getContainer,
  listContainers: docker.listContainers,
};
const admin: SessionUser = { id: "admin", username: "owner", role: "admin" };
const friend: SessionUser = {
  id: "friend",
  username: "friend",
  role: "operator",
};
const stranger: SessionUser = {
  id: "stranger",
  username: "stranger",
  role: "viewer",
};
const clients: FakeSocket[] = [];
let removed = false;
function fixture(id: string) {
  return {
    Id: id,
    Name: `/${id}`,
    Config: { Image: "itzg/minecraft-server", Labels: {} },
    State: { Status: "running" },
    Mounts: [],
    NetworkSettings: { Ports: {} },
    Created: "2026-01-01T00:00:00.000Z",
  };
}
class FakeSocket extends EventEmitter implements SocketChannel {
  get isOpen() { return this.readyState === 1; }
  onMessage(listener: (message: SocketMessage) => void): void { this.on("message", listener); }
  onClose(listener: (code: number) => void): void { this.once("close", listener); }
  OPEN = 1;
  readyState = 1;
  messages: string[] = [];
  closed = false;
  send(data: string) {
    this.messages.push(data);
  }
  close() {
    this.closed = true;
    this.readyState = 3;
    this.emit("close");
  }
}
function client(user: SessionUser): FakeSocket {
  const socket = new FakeSocket();
  clients.push(socket);
  addEventClient(socket, {
    user,
    validate: () => user,
  });
  return socket;
}
beforeEach(async () => {
  closeDatabase();
  removed = false;
  for (const user of [admin, friend, stranger])
    createUser({
      ...user,
      passwordHash: "test",
      disabled: false,
      createdAt: 0,
    });
  docker.getEvents = (async (options: unknown) => {
    assert.deepEqual(options, { filters: { type: ["container"] } });
    return new PassThrough();
  }) as unknown as typeof docker.getEvents;
  docker.listContainers = (async () =>
    (removed ? ["second"] : ["first", "second"]).map((id) => ({
      Id: id,
      Names: [`/${id}`],
      Image: "itzg/minecraft-server",
      Labels: {},
    }))) as unknown as typeof docker.listContainers;
  docker.getContainer = ((id: string) => ({
    inspect: async () => fixture(id),
  })) as unknown as typeof docker.getContainer;
  await refreshServers();
  const first = listLogicalServers().find(
    (server) => server.containerId === "first",
  )!;
  setServerGrant(
    friend.id,
    first.id,
    ["server.view", "server.start", "server.stop"],
    admin,
  );
});
afterEach(async () => {
  for (const socket of clients.splice(0)) socket.close();
  await stopEventStream();
  Object.assign(docker, originals);
  closeDatabase();
});

describe("Docker event framing", () => {
  it("buffers split lines and accepts several events in a chunk", () => {
    const events: unknown[] = [];
    const decode = dockerEventDecoder((event) => events.push(event));
    const content = Buffer.from(
      '{"Action":"start","Actor":{"ID":"first"},"name":"café"}\n{"Action":"stop"}\n',
    );
    const split = content.indexOf(Buffer.from("é")) + 1;
    decode(content.subarray(0, split));
    assert.deepEqual(events, []);
    decode(content.subarray(split));
    assert.equal(events.length, 2);
    assert.equal((events[0] as { name: string }).name, "café");
  });
  it("rejects oversized input and skips malformed lines", () => {
    const events: unknown[] = [];
    const decode = dockerEventDecoder((event) => events.push(event));
    decode(Buffer.from('malformed\nnull\n[]\n{"Action":"start"}\n'));
    assert.equal(events.length, 1);
    assert.throws(() => decode(Buffer.alloc(1_048_577, "a")), /exceeded/);
  });
});

describe("scoped Docker event delivery", () => {
  it("waits for an in-flight state refresh before event shutdown finishes", async () => {
    let started!: () => void;
    const pending = new Promise<void>((resolve) => { started = resolve; });
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const list = docker.listContainers;
    docker.listContainers = (async () => {
      started();
      await gate;
      return list.call(docker);
    }) as typeof docker.listContainers;
    const delivery = dispatchDockerEvent({ Action: "start", Actor: { ID: "first" } });
    await pending;
    let stopped = false;
    const stopping = stopEventStream().then(() => { stopped = true; });
    await new Promise<void>((resolve) => setImmediate(resolve));
    assert.equal(stopped, false);
    release();
    await delivery;
    await stopping;
    assert.equal(stopped, true);
  });

  it("sends only assigned logical server changes to non-administrators", async () => {
    const owner = client(admin),
      allowed = client(friend),
      denied = client(stranger);
    await dispatchDockerEvent({ Action: "start", Actor: { ID: "second" } });
    assert.equal(owner.messages.length, 1);
    assert.equal(allowed.messages.length, 0);
    assert.equal(denied.messages.length, 0);
    await dispatchDockerEvent({ Action: "stop", Actor: { ID: "first" } });
    assert.equal(allowed.messages.length, 1);
    const event = JSON.parse(allowed.messages[0]);
    assert.match(event.serverId, /^[0-9a-f-]{36}$/);
    assert.equal(event.containerId, undefined);
    assert.equal(event.name, undefined);
  });
  it("ignores exec commands and unknown container metadata", async () => {
    const owner = client(admin);
    await dispatchDockerEvent({
      Action: "exec_create: secret-password",
      Actor: { ID: "first" },
    });
    await dispatchDockerEvent({
      Action: "start",
      Actor: { ID: "unknown-container" },
    });
    assert.deepEqual(owner.messages, []);
  });
  it("rechecks grants and account status before each event", async () => {
    const socket = client(friend);
    const first = listLogicalServers().find(
      (server) => server.containerId === "first",
    )!;
    setServerGrant(friend.id, first.id, [], admin);
    await dispatchDockerEvent({ Action: "start", Actor: { ID: "first" } });
    assert.deepEqual(socket.messages, []);
    updateUserAccess(friend.id, "operator", true);
    await dispatchDockerEvent({ Action: "start", Actor: { ID: "first" } });
    assert.equal(socket.closed, true);
  });
  it("invalidates a previously visible removed server without revealing metadata", async () => {
    const socket = client(friend);
    removed = true;
    await dispatchDockerEvent({ Action: "destroy", Actor: { ID: "first" } });
    assert.equal(socket.messages.length, 1);
    assert.equal(JSON.parse(socket.messages[0]).action, "refresh");
    assert.equal(JSON.parse(socket.messages[0]).serverId, undefined);
  });
});
