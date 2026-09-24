import { fixtureBytes, repeatedBytes } from "./fixtures/bytes.js";
import { SocketFixture } from "./fixtures/socket-channel.js";
import { expect, afterEach, beforeEach, describe, it } from "bun:test";
import {
  addEventClient,
  dispatchDockerEvent,
  dockerEventDecoder,
  stopEventStream,
} from "../src/events.js";
import { docker } from "../src/docker-client.js";
import {
  closeDatabase,
  createUser,
  updateUserAccess,
  type SessionUser,
} from "../src/database.js";
import { setServerGrant } from "./fixtures/grants.js";
import { listLogicalServers } from "../src/identity.js";
import { refreshServers } from "../src/servers.js";

process.env.LUDOCK_DB_PATH = ":memory:";
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
const FakeSocket = SocketFixture;
type FakeSocket = SocketFixture;
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
    expect(options).toStrictEqual({ filters: { type: ["container"] } });
    return new ReadableStream<Uint8Array>();
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
    const text = '{"Action":"start","Actor":{"ID":"first"},"name":"café"}\n{"Action":"stop"}\n';
    const content = fixtureBytes(text);
    // Split inside the two-byte "é".
    const split = fixtureBytes(text.slice(0, text.indexOf("é"))).length + 1;
    decode(content.subarray(0, split));
    expect(events).toStrictEqual([]);
    decode(content.subarray(split));
    expect(events.length).toBe(2);
    expect((events[0] as { name: string }).name).toBe("café");
  });
  it("rejects oversized input and skips malformed lines", () => {
    const events: unknown[] = [];
    const decode = dockerEventDecoder((event) => events.push(event));
    decode(fixtureBytes('malformed\nnull\n[]\n{"Action":"start"}\n'));
    expect(events.length).toBe(1);
    expect(() => decode(repeatedBytes(1_048_577, "a"))).toThrow(/exceeded/);
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
    expect(stopped).toBe(false);
    release();
    await delivery;
    await stopping;
    expect(stopped).toBe(true);
  });

  it("sends only assigned logical server changes to non-administrators", async () => {
    const owner = client(admin),
      allowed = client(friend),
      denied = client(stranger);
    await dispatchDockerEvent({ Action: "start", Actor: { ID: "second" } });
    expect(owner.messages.length).toBe(1);
    expect(allowed.messages.length).toBe(0);
    expect(denied.messages.length).toBe(0);
    await dispatchDockerEvent({ Action: "stop", Actor: { ID: "first" } });
    expect(allowed.messages.length).toBe(1);
    const event = JSON.parse(allowed.messages[0]);
    expect(event.serverId).toMatch(/^[0-9a-f-]{36}$/);
    expect(event.containerId).toBe(undefined);
    expect(event.name).toBe(undefined);
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
    expect(owner.messages).toStrictEqual([]);
  });
  it("rechecks grants and account status before each event", async () => {
    const socket = client(friend);
    const first = listLogicalServers().find(
      (server) => server.containerId === "first",
    )!;
    setServerGrant(friend.id, first.id, [], admin);
    await dispatchDockerEvent({ Action: "start", Actor: { ID: "first" } });
    expect(socket.messages).toStrictEqual([]);
    updateUserAccess(friend.id, "operator", true);
    await dispatchDockerEvent({ Action: "start", Actor: { ID: "first" } });
    expect(socket.closed).toBe(true);
  });
  it("invalidates a previously visible removed server without revealing metadata", async () => {
    const socket = client(friend);
    removed = true;
    await dispatchDockerEvent({ Action: "destroy", Actor: { ID: "first" } });
    expect(socket.messages.length).toBe(1);
    expect(JSON.parse(socket.messages[0]).action).toBe("refresh");
    expect(JSON.parse(socket.messages[0]).serverId).toBe(undefined);
  });
});
