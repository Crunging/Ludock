import { webConnection } from "./fixtures/web-streams.js";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough, Readable } from "node:stream";
import { afterAll as after, afterEach, beforeEach, describe, it } from "bun:test";
import type { SocketChannel, SocketMessage } from "../src/socket-channel.js";
import { getDockerInstance } from "../src/docker.js";
import { handleConsoleConnection } from "../src/console.js";
import { handleContainerLogsConnection } from "../src/container-logs.js";
import {
  closeDatabase,
  createUser,
  updateUserAccess,
  type SessionUser,
} from "../src/database.js";
import { listLogicalServers } from "../src/identity.js";
import { setServerGrant, setUserServerGrants } from "../src/authorization.js";
import { refreshServers } from "../src/servers.js";
import { acquireLocks } from "../src/operation-locks.js";
import {
  ConsoleOutputRedactor,
  observationSecrets,
} from "../src/console-redaction.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const docker = getDockerInstance();
const originalGetContainer = docker.getContainer.bind(docker);
const originalListContainers = docker.listContainers.bind(docker);
const administrator: SessionUser = {
  id: "api-token",
  username: "api-token",
  role: "admin",
};
const operator: SessionUser = {
  id: "operator",
  username: "friend",
  role: "operator",
};
let logStream: PassThrough;
let logCalls = 0;
let attachCalls = 0;
let containerId = "physical-server";
let serverId: string;
const sockets: FakeWebSocket[] = [];

class FakeWebSocket extends EventEmitter implements SocketChannel {
  get isOpen() { return this.readyState === 1; }
  onMessage(listener: (message: SocketMessage) => void): void { this.on("message", listener); }
  onClose(listener: (code: number) => void): void { this.once("close", listener); }
  readonly OPEN = 1;
  readyState = this.OPEN;
  sent: Array<{ type: string; data: string }> = [];
  closeCode: number | null = null;
  constructor() {
    super();
    sockets.push(this);
  }
  send(raw: string): void {
    this.sent.push(JSON.parse(raw) as { type: string; data: string });
  }
  close(code: number): void {
    this.closeCode = code;
    this.readyState = 3;
    this.emit("close", code);
  }
}

beforeEach(async () => {
  closeDatabase();
  createUser({
    ...operator,
    passwordHash: "fixture",
    disabled: false,
    createdAt: 1,
  });
  logStream = new PassThrough();
  logCalls = 0;
  attachCalls = 0;
  containerId = "physical-server";
  docker.listContainers = (async () => [
    {
      Id: containerId,
      Image: "fixture:latest",
      Labels: { "ludock.enable": "true" },
    },
  ]) as unknown as typeof docker.listContainers;
  docker.getContainer = (() => ({
    inspect: async () => ({
      Id: containerId,
      Name: "/game",
      Config: {
        Image: "fixture:latest",
        Labels: { "ludock.enable": "true", "ludock.console": "stdin-console" },
        OpenStdin: true,
        StdinOnce: false,
        Env: [],
      },
      State: { Status: "running" },
      NetworkSettings: { Ports: {} },
      Created: "2026-09-01T00:00:00Z",
      Mounts: [],
    }),
    logs: async () => {
      logCalls += 1;
      return Readable.toWeb(logStream);
    },
    attach: async () => {
      attachCalls += 1;
      const stream = new PassThrough();
      stream.resume();
      return webConnection(stream);
    },
  })) as unknown as typeof docker.getContainer;
  await refreshServers();
  serverId = listLogicalServers()[0].id;
});
afterEach(() => {
  sockets.splice(0).forEach((socket) => socket.close(1000));
  docker.getContainer = originalGetContainer;
  docker.listContainers = originalListContainers;
});
after(() => closeDatabase());

describe("WebSocket server capability boundaries", () => {
  it("rejects console and log endpoints for a lifecycle-only friend before Docker attachment", async () => {
    setServerGrant(
      operator.id,
      serverId,
      ["server.view", "server.start", "server.stop"],
      administrator,
    );
    const console = new FakeWebSocket();
    await handleConsoleConnection(
      console,
      request("game-console"),
      auth(),
      "game",
    );
    const logs = new FakeWebSocket();
    await handleContainerLogsConnection(
      logs,
      request("logs"),
      auth(),
    );
    assert.equal(console.closeCode, 1008);
    assert.equal(logs.closeCode, 1008);
    assert.equal(logCalls, 0);
    assert.equal(attachCalls, 0);
  });

  it("lets a command-only grant execute without attaching historical or live logs", async () => {
    setServerGrant(
      operator.id,
      serverId,
      ["server.view", "console.execute"],
      administrator,
    );
    const ws = new FakeWebSocket();
    await handleConsoleConnection(
      ws,
      request("game-console"),
      auth(),
      "game",
    );
    assert.equal(ws.closeCode, null);
    assert.equal(logCalls, 0);
    ws.emit("message", Buffer.from('{"type":"input","data":"help"}'));
    await settle();
    assert.equal(attachCalls, 1);
    assert.ok(ws.sent.some((message) => message.data.includes("Command sent")));
  });

  it("closes the console when log access is revoked during asynchronous attachment", async () => {
    setServerGrant(
      operator.id,
      serverId,
      ["server.view", "console.execute", "logs.read"],
      administrator,
    );
    let attachLogs!: (stream: PassThrough) => void;
    let requestedLogs!: () => void;
    const pendingLogs = new Promise<PassThrough>((resolve) => { attachLogs = resolve; });
    const didRequestLogs = new Promise<void>((resolve) => { requestedLogs = resolve; });
    const getFixtureContainer = docker.getContainer;
    docker.getContainer = ((id: string) => ({
      ...getFixtureContainer(id),
      logs: async () => {
        logCalls++;
        requestedLogs();
        return pendingLogs.then(stream => Readable.toWeb(stream));
      },
    })) as unknown as typeof docker.getContainer;

    const ws = new FakeWebSocket();
    const connection = handleConsoleConnection(ws, request("game-console"), auth(), "game");
    await didRequestLogs;
    setServerGrant(
      operator.id,
      serverId,
      ["server.view", "console.execute"],
      administrator,
    );
    attachLogs(logStream);
    await connection;

    assert.equal(ws.isOpen, false);
    assert.equal(ws.closeCode, 1008);
    assert.equal(logStream.destroyed, true);
    ws.emit("message", Buffer.from('{"type":"input","data":"help"}'));
    await settle();
    assert.equal(attachCalls, 0);

    // The remaining command grant works on a fresh, command-only connection.
    const reconnected = new FakeWebSocket();
    await handleConsoleConnection(reconnected, request("game-console"), auth(), "game");
    reconnected.emit("message", Buffer.from('{"type":"input","data":"help"}'));
    await settle();
    assert.equal(reconnected.isOpen, true);
    assert.equal(logCalls, 1);
    assert.equal(attachCalls, 1);
  });

  it("closes and destroys the log stream before sending any frame after grants are revoked", async () => {
    setServerGrant(
      operator.id,
      serverId,
      ["server.view", "logs.read"],
      administrator,
    );
    const ws = new FakeWebSocket();
    await handleContainerLogsConnection(
      ws,
      request("logs"),
      auth(),
    );
    setUserServerGrants(operator.id, [], administrator);
    logStream.write("must not reach revoked user");
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(ws.closeCode, 1008);
    assert.equal(logStream.destroyed, true);
    assert.equal(
      ws.sent.some((message) => message.data.includes("must not reach")),
      false,
    );
  });

  it("blocks the next console command immediately after account disablement", async () => {
    setServerGrant(
      operator.id,
      serverId,
      ["server.view", "console.execute"],
      administrator,
    );
    const ws = new FakeWebSocket();
    await handleConsoleConnection(
      ws,
      request("game-console"),
      auth(),
      "game",
    );
    updateUserAccess(operator.id, "operator", true);
    ws.emit("message", Buffer.from('{"type":"input","data":"save"}'));
    await settle();
    assert.equal(ws.closeCode, 1008);
    assert.equal(attachCalls, 0);
  });

  it("rejects commands while a conflicting operation owns the server lock", async () => {
    setServerGrant(
      operator.id,
      serverId,
      ["server.view", "console.execute"],
      administrator,
    );
    const ws = new FakeWebSocket();
    await handleConsoleConnection(
      ws,
      request("game-console"),
      auth(),
      "game",
    );
    const release = acquireLocks([`server:${serverId}`]);
    try {
      ws.emit("message", Buffer.from('{"type":"input","data":"save"}'));
      await settle();
      assert.equal(attachCalls, 0);
      assert.ok(
        ws.sent.some((message) =>
          message.data.includes("conflicting operation"),
        ),
      );
    } finally {
      release();
    }
  });

  it("closes a stream when Docker recreation replaces its original binding", async () => {
    setServerGrant(
      operator.id,
      serverId,
      ["server.view", "console.execute"],
      administrator,
    );
    const ws = new FakeWebSocket();
    await handleConsoleConnection(
      ws,
      request("game-console"),
      auth(),
      "game",
    );
    containerId = "new-container";
    ws.emit("message", Buffer.from('{"type":"input","data":"save"}'));
    await settle();
    assert.equal(ws.closeCode, 1008);
    assert.equal(attachCalls, 0);
  });

  it("keeps the server lock until a disconnected Docker exec has ended", async () => {
    closeDatabase();
    createUser({
      ...operator,
      passwordHash: "fixture",
      disabled: false,
      createdAt: 1,
    });
    const mainStream = new PassThrough();
    let executionCount = 0;
    let started!: () => void;
    let cancellationStarted!: () => void;
    const didStart = new Promise<void>((resolve) => { started = resolve; });
    const didCancel = new Promise<void>((resolve) => {
      cancellationStarted = resolve;
    });
    docker.getContainer = (() => ({
      inspect: async () => ({
        Id: containerId,
        Name: "/game",
        Config: {
          Image: "fixture:latest",
          Labels: {
            "ludock.enable": "true",
            "ludock.console": "minecraft-rcon",
          },
          OpenStdin: false,
          StdinOnce: false,
          Env: [],
        },
        State: { Status: "running" },
        NetworkSettings: { Ports: {} },
        Created: "2026-09-01T00:00:00Z",
        Mounts: [],
      }),
      exec: async () => {
        executionCount++;
        if (executionCount === 1) {
          return {
            start: async () => {
              started();
              return webConnection(mainStream);
            },
            inspect: async () => ({ Running: false, ExitCode: 125 }),
          };
        }
        return {
          start: async () => {
            cancellationStarted();
            const stream = new PassThrough();
            setImmediate(() => stream.end());
            return webConnection(stream);
          },
        };
      },
    })) as unknown as typeof docker.getContainer;
    await refreshServers();
    serverId = listLogicalServers()[0].id;
    setServerGrant(
      operator.id,
      serverId,
      ["server.view", "console.execute"],
      administrator,
    );
    const ws = new FakeWebSocket();
    await handleConsoleConnection(
      ws,
      request("game-console"),
      auth(),
      "game",
    );
    ws.emit("message", Buffer.from('{"type":"input","data":"save"}'));
    await didStart;
    assert.equal(serverLockIsHeld(), true);

    ws.close(1000);
    await didCancel;
    assert.equal(serverLockIsHeld(), true);
    mainStream.end();
    await settle();
    assert.equal(serverLockIsHeld(), false);
  });

  it("does not grant administrator shell access through an operator's console grant", async () => {
    setServerGrant(
      operator.id,
      serverId,
      ["server.view", "console.execute"],
      administrator,
    );
    const ws = new FakeWebSocket();
    await handleConsoleConnection(
      ws,
      request("shell"),
      auth(),
      "shell",
    );
    assert.equal(ws.closeCode, 1008);
  });
});

describe("console credential redaction", () => {
  it("redacts known secrets split across frame boundaries and custom password variables", () => {
    const values: string[] = [];
    const redactor = new ConsoleOutputRedactor(["secret-value"], (value) =>
      values.push(value),
    );
    redactor.push("prefix sec");
    redactor.push("ret-va");
    redactor.push("lue suffix");
    redactor.end();
    assert.equal(values.join(""), "prefix [redacted] suffix");
    const secrets = observationSecrets({
      containerId: "id",
      name: "game",
      displayName: "game",
      gameType: "unknown",
      mounts: [],
      gameConfiguration: {
        "ludock.console.password-env": "CUSTOM_KEY",
        "env:CUSTOM_KEY": "do-not-leak",
        "env:API_TOKEN": "another-secret",
        "env:RCON_PORT": "25575",
      },
    });
    assert.deepEqual(
      new Set(secrets),
      new Set(["do-not-leak", "another-secret"]),
    );
  });
});

function auth() {
  return { user: operator, validate: () => operator };
}
function request(endpoint: string): Request {
  return new Request(`http://localhost/ws/v1/${endpoint}/${serverId}`);
}

async function settle(): Promise<void> {
  await new Promise<void>((resolve) => setImmediate(resolve));
}

function serverLockIsHeld(): boolean {
  let release: (() => void) | undefined;
  try {
    release = acquireLocks([`server:${serverId}`]);
    return false;
  } catch {
    return true;
  } finally {
    release?.();
  }
}
