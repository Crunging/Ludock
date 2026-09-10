import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import { afterAll as after, afterEach, beforeEach, describe, it } from "bun:test";
import type { SocketChannel, SocketMessage } from "../src/socket-channel.js";
import { ConsoleOutputRedactor } from "../src/console-redaction.js";

process.env.LUDOCK_DB_PATH = ":memory:";

const [
  { DockerLogDecoder, handleContainerLogsConnection },
  { getDockerInstance },
  { closeDatabase, createUser },
  { listLogicalServers },
  { refreshServers },
  { setServerGrant },
] = await Promise.all([
  import("../src/container-logs.js"),
  import("../src/docker.js"),
  import("../src/database.js"),
  import("../src/identity.js"),
  import("../src/servers.js"),
  import("../src/authorization.js"),
]);

const docker = getDockerInstance();
const originalGetContainer = docker.getContainer.bind(docker);
const originalListContainers = docker.listContainers.bind(docker);
const administrator = {
  id: "api-token",
  username: "api-token",
  role: "admin" as const,
};

class FakeWebSocket extends EventEmitter implements SocketChannel {
  get isOpen() { return this.readyState === 1; }
  onMessage(listener: (message: SocketMessage) => void): void { this.on("message", listener); }
  onClose(listener: (code: number) => void): void { this.once("close", listener); }
  readonly OPEN = 1;
  readyState = this.OPEN;
  sent: Array<{ type: string; data: string }> = [];
  closeCode: number | null = null;

  send(raw: string): void {
    this.sent.push(JSON.parse(raw) as { type: string; data: string });
  }

  close(code: number): void {
    this.closeCode = code;
    this.readyState = 3;
    this.emit("close", code);
  }
}

beforeEach(() => {
  closeDatabase();
  createUser({
    id: "viewer",
    username: "viewer",
    role: "viewer",
    passwordHash: "fixture",
    disabled: false,
    createdAt: 1,
  });
});
afterEach(() => {
  docker.getContainer = originalGetContainer;
  docker.listContainers = originalListContainers;
});

after(() => closeDatabase());

describe("Docker log decoder", () => {
  it("reassembles multiplexed headers and payloads split across chunks", () => {
    const output: Array<{ type: string; data: string }> = [];
    const decoder = new DockerLogDecoder((type, data) =>
      output.push({ type, data }),
    );
    const stdout = frame(1, "hello ");
    const stderr = frame(2, "world");
    const combined = Buffer.concat([stdout, stderr]);

    decoder.push(combined.subarray(0, 2));
    decoder.push(combined.subarray(2, 11));
    decoder.push(combined.subarray(11, 17));
    decoder.push(combined.subarray(17));

    assert.equal(decoder.end(), true);
    assert.deepEqual(output, [
      { type: "stdout", data: "hello " },
      { type: "stderr", data: "world" },
    ]);
  });

  it("passes TTY log output through without Docker framing", () => {
    const output: string[] = [];
    const decoder = new DockerLogDecoder((_type, data) => output.push(data));
    decoder.push(Buffer.from("plain "));
    decoder.push(Buffer.from("output"));
    assert.equal(decoder.end(), true);
    assert.deepEqual(output, ["plain ", "output"]);
  });

  for (const framed of [false, true]) {
    it(`preserves and redacts Unicode credentials split across ${framed ? "Docker frames" : "TTY chunks"}`, () => {
      const output: string[] = [];
      const secret = "päss🔑word";
      const redactor = new ConsoleOutputRedactor([secret], (value) => output.push(value));
      const decoder = new DockerLogDecoder((_type, value) => redactor.push(value));
      for (const byte of Buffer.from(`before ${secret} after`)) {
        const payload = Buffer.from([byte]);
        decoder.push(framed ? frame(1, payload) : payload);
      }
      assert.equal(decoder.end(), true);
      redactor.end();
      assert.equal(output.join(""), "before [redacted] after");
    });
  }
});

describe("Docker log WebSocket", () => {
  it("follows assigned logs for viewers and rejects input", async () => {
    const logStream = new PassThrough();
    let logOptions: Record<string, unknown> | null = null;
    docker.getContainer = (() => ({
      inspect: async () => managedInspect("managed-id", true),
      logs: async (options: Record<string, unknown>) => {
        logOptions = options;
        return logStream;
      },
    })) as unknown as typeof docker.getContainer;

    docker.listContainers = (async () => [
      {
        Id: "managed-id",
        Image: "fixture:latest",
        Labels: { "ludock.enable": "true" },
      },
    ]) as unknown as typeof docker.listContainers;
    await refreshServers();
    const logicalId = listLogicalServers()[0].id;
    setServerGrant(
      "viewer",
      logicalId,
      ["server.view", "logs.read"],
      administrator,
    );
    const ws = new FakeWebSocket();
    await handleContainerLogsConnection(
      ws,
      request(`/ws/v1/logs/${logicalId}`),
      viewerAuth(),
    );

    assert.deepEqual(logOptions, {
      follow: true,
      stdout: true,
      stderr: true,
      tail: 500,
      timestamps: true,
    });
    logStream.write("2026-08-06T12:00:00Z game started\n");
    assert.ok(
      ws.sent.some(
        (message) =>
          message.type === "stdout" && message.data.includes("game started"),
      ),
    );

    ws.emit("message", Buffer.from('{"type":"input","data":"stop"}'));
    assert.ok(
      ws.sent.some(
        (message) =>
          message.type === "error" &&
          message.data === "Docker logs are read-only",
      ),
    );
    ws.close(1000);
    assert.equal(logStream.destroyed, true);
  });

  it("closes before reading logs from an unmanaged container", async () => {
    let logsCalled = false;
    docker.getContainer = (() => ({
      inspect: async () => managedInspect("unmanaged-id", false),
      logs: async () => {
        logsCalled = true;
        return new PassThrough();
      },
    })) as unknown as typeof docker.getContainer;

    docker.listContainers =
      (async () => []) as unknown as typeof docker.listContainers;
    const ws = new FakeWebSocket();
    await handleContainerLogsConnection(
      ws,
      request("/ws/v1/logs/unmanaged-id"),
      viewerAuth(),
    );

    assert.equal(ws.closeCode, 1008);
    assert.equal(logsCalled, false);
  });
});

function frame(type: 1 | 2, value: string | Buffer): Buffer {
  const payload = Buffer.from(value);
  const result = Buffer.alloc(8 + payload.length);
  result[0] = type;
  result.writeUInt32BE(payload.length, 4);
  payload.copy(result, 8);
  return result;
}

function viewerAuth() {
  const user = { id: "viewer", username: "viewer", role: "viewer" as const };
  return { user, validate: () => user };
}

function request(path: string): Request {
  return new Request(`http://localhost${path}`);
}

function managedInspect(id: string, managed: boolean) {
  return {
    Id: id,
    Config: {
      Image: "fixture:latest",
      Labels: managed ? { "ludock.enable": "true" } : {},
    },
    Name: `/${id}`,
    State: { Status: "running" },
    NetworkSettings: { Ports: {} },
    Created: "2026-08-06T00:00:00.000Z",
    Mounts: [],
  };
}
