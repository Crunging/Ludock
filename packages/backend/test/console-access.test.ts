import { fixtureBytes } from "./fixtures/bytes.js";
import { SocketFixture } from "./fixtures/socket-channel.js";
import { StreamFixture } from "./fixtures/web-streams.js";
import { expect, afterAll as after, afterEach, beforeEach, describe, it } from "bun:test";
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
let logStream: StreamFixture;
let logCalls = 0;
let attachCalls = 0;
let containerId = "physical-server";
let serverId: string;
const sockets: FakeWebSocket[] = [];

class FakeWebSocket extends SocketFixture {
  constructor() { super(); sockets.push(this); }
}

beforeEach(async () => {
  closeDatabase();
  createUser({
    ...operator,
    passwordHash: "fixture",
    disabled: false,
    createdAt: 1,
  });
  logStream = new StreamFixture();
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
      return logStream.readable;
    },
    attach: async () => {
      attachCalls += 1;
      const stream = new StreamFixture();

      return stream.connection;
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
    expect(console.closeCode).toBe(1008);
    expect(logs.closeCode).toBe(1008);
    expect(logCalls).toBe(0);
    expect(attachCalls).toBe(0);
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
    expect(ws.closeCode).toBe(null);
    expect(logCalls).toBe(0);
    ws.receive(fixtureBytes('{"type":"input","data":"help"}'));
    await settle();
    expect(attachCalls).toBe(1);
    expect(ws.sent.some((message) => message.data.includes("Command sent"))).toBeTruthy();
  });

  it("closes the console when log access is revoked during asynchronous attachment", async () => {
    setServerGrant(
      operator.id,
      serverId,
      ["server.view", "console.execute", "logs.read"],
      administrator,
    );
    let attachLogs!: (stream: StreamFixture) => void;
    let requestedLogs!: () => void;
    const pendingLogs = new Promise<StreamFixture>((resolve) => { attachLogs = resolve; });
    const didRequestLogs = new Promise<void>((resolve) => { requestedLogs = resolve; });
    const getFixtureContainer = docker.getContainer;
    docker.getContainer = ((id: string) => ({
      ...getFixtureContainer(id),
      logs: async () => {
        logCalls++;
        requestedLogs();
        return pendingLogs.then(stream => stream.readable);
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

    expect(ws.isOpen).toBe(false);
    expect(ws.closeCode).toBe(1008);
    expect(logStream.closed).toBe(true);
    ws.receive(fixtureBytes('{"type":"input","data":"help"}'));
    await settle();
    expect(attachCalls).toBe(0);

    // The remaining command grant works on a fresh, command-only connection.
    const reconnected = new FakeWebSocket();
    await handleConsoleConnection(reconnected, request("game-console"), auth(), "game");
    reconnected.receive(fixtureBytes('{"type":"input","data":"help"}'));
    await settle();
    expect(reconnected.isOpen).toBe(true);
    expect(logCalls).toBe(1);
    expect(attachCalls).toBe(1);
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
    logStream.enqueue("must not reach revoked user");
    await new Promise<void>(resolve => setImmediate(resolve));
    expect(ws.closeCode).toBe(1008);
    expect(logStream.closed).toBe(true);
    expect(ws.sent.some((message) => message.data.includes("must not reach"))).toBe(false);
  });

  for (const endpoint of ["logs", "game-console"] as const) {
    it(`does not release a credential fragment when ${endpoint} ends inside a UTF-8 frame`, async () => {
      const getFixtureContainer = docker.getContainer;
      docker.getContainer = ((id: string) => {
        const container = getFixtureContainer(id);
        return {
          ...container,
          inspect: async () => {
            const info = await container.inspect();
            return { ...info, Config: { ...info.Config, Env: ["API_TOKEN=secreté"] } };
          },
        };
      }) as unknown as typeof docker.getContainer;
      // Discover the fixture with its credential from the outset, so this is
      // ordinary stream output rather than a changed binding under review.
      closeDatabase();
      createUser({ ...operator, passwordHash: "fixture", disabled: false, createdAt: 1 });
      await refreshServers();
      serverId = listLogicalServers()[0].id;
      setServerGrant(operator.id, serverId, ["server.view", "logs.read", "console.execute"], administrator);
      const ws = new FakeWebSocket();
      if (endpoint === "logs") await handleContainerLogsConnection(ws, request(endpoint), auth());
      else await handleConsoleConnection(ws, request(endpoint), auth(), "game");
      const body = fixtureBytes("secreté");
      const wire = new Uint8Array(8 + body.length);
      wire[0] = 1;
      new DataView(wire.buffer).setUint32(4, body.length);
      wire.set(body, 8);
      logStream.close(wire.subarray(0, wire.length - 1));
      await settle();
      expect(ws.sent.filter(message => message.type === "stdout" || message.type === "stderr")).toStrictEqual([]);
      expect(ws.sent.some(message => message.type === "error" && message.data === "Docker log stream failed")).toBe(true);
    });
  }

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
    ws.receive(fixtureBytes('{"type":"input","data":"save"}'));
    await settle();
    expect(ws.closeCode).toBe(1008);
    expect(attachCalls).toBe(0);
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
      ws.receive(fixtureBytes('{"type":"input","data":"save"}'));
      await settle();
      expect(attachCalls).toBe(0);
      expect(ws.sent.some((message) =>
          message.data.includes("conflicting operation"),
        )).toBeTruthy();
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
    ws.receive(fixtureBytes('{"type":"input","data":"save"}'));
    await settle();
    expect(ws.closeCode).toBe(1008);
    expect(attachCalls).toBe(0);
  });

  it("keeps the server lock until a disconnected Docker exec has ended", async () => {
    closeDatabase();
    createUser({
      ...operator,
      passwordHash: "fixture",
      disabled: false,
      createdAt: 1,
    });
    const mainStream = new StreamFixture();
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
              return mainStream.connection;
            },
            inspect: async () => ({ Running: false, ExitCode: 125 }),
          };
        }
        return {
          start: async () => {
            cancellationStarted();
            const stream = new StreamFixture();
            setImmediate(() => stream.close());
            return stream.connection;
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
    ws.receive(fixtureBytes('{"type":"input","data":"save"}'));
    await didStart;
    expect(serverLockIsHeld()).toBe(true);

    ws.close(1000);
    await didCancel;
    expect(serverLockIsHeld()).toBe(true);
    mainStream.close();
    await settle();
    expect(serverLockIsHeld()).toBe(false);
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
    expect(ws.closeCode).toBe(1008);
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
    expect(values.join("")).toBe("prefix [redacted] suffix");
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
    expect(new Set(secrets)).toStrictEqual(new Set(["do-not-leak", "another-secret"]));
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
