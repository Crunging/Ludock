import { fixtureBytes } from "./fixtures/bytes.js";
import { byteView, concatBytes } from "../src/bytes.js";
import { SocketFixture } from "./fixtures/socket-channel.js";
import { StreamFixture } from "./fixtures/web-streams.js";
import { expect, afterAll as after, afterEach, beforeEach, describe, it } from "bun:test";
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

const FakeWebSocket = SocketFixture;
type FakeWebSocket = SocketFixture;

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
  it("streams partial payloads while reassembling split multiplexed headers", () => {
    const output: Array<{ type: string; data: string }> = [];
    const decoder = new DockerLogDecoder((type, data) =>
      output.push({ type, data }),
    );
    const stdout = frame(1, "hello ");
    const stderr = frame(2, "world");
    const combined = concatBytes([stdout, stderr]);

    decoder.push(combined.subarray(0, 2));
    decoder.push(combined.subarray(2, 11));
    expect(output).toStrictEqual([{ type: "stdout", data: "hel" }]);
    decoder.push(combined.subarray(11, 17));
    decoder.push(combined.subarray(17));

    expect(decoder.end()).toBe(true);
    expect(output).toStrictEqual([
      { type: "stdout", data: "hel" },
      { type: "stdout", data: "lo " },
      { type: "stderr", data: "world" },
    ]);
  });

  it("passes TTY log output through without Docker framing", () => {
    const output: string[] = [];
    const decoder = new DockerLogDecoder((_type, data) => output.push(data));
    decoder.push(fixtureBytes("plain "));
    decoder.push(fixtureBytes("output"));
    expect(decoder.end()).toBe(true);
    expect(output).toStrictEqual(["plain ", "output"]);
  });

  it("streams a large frame before its final chunk and tolerates reused input buffers", () => {
    let received = 0;
    const decoder = new DockerLogDecoder((type, data) => {
      expect(type).toBe("stdout");
      expect(data).toBe("x".repeat(data.length));
      received += data.length;
    });
    const header = new Uint8Array(8);
    header[0] = 1;
    byteView(header).setUint32(4, 1024 * 1024);
    decoder.push(header);
    const payload = new Uint8Array(4096);
    for (let n = 0; n < 256; n++) {
      payload.fill(120);
      decoder.push(payload);
      payload.fill(0);
      expect(received).toBe((n + 1) * 4096);
    }
    expect(decoder.end()).toBe(true);
  });

  it("keeps UTF-8 decoding separate for interleaved channels and empty frames", () => {
    const output = { stdout: "", stderr: "" };
    const decoder = new DockerLogDecoder((type, value) => { output[type] += value; });
    const stdout = fixtureBytes("🔑"), stderr = fixtureBytes("é");
    const bytes = concatBytes([
      frame(1, stdout.subarray(0, 2)), frame(2, stderr.subarray(0, 1)),
      frame(1, ""), frame(1, stdout.subarray(2)), frame(2, stderr.subarray(1)),
    ]);
    for (const byte of bytes) decoder.push(fixtureBytes([byte]));
    expect(decoder.end()).toBe(true);
    expect(output).toStrictEqual({ stdout: "🔑", stderr: "é" });
  });

  it("reports truncated headers and payloads without emitting framing bytes", () => {
    for (const length of [4, 7, 8, 10]) {
      let output = "";
      const decoder = new DockerLogDecoder((_type, data) => { output += data; });
      decoder.push(frame(1, "hello").subarray(0, length));
      expect(decoder.end()).toBe(false);
      expect(output).toBe(length > 8 ? "hello".slice(0, length - 8) : "");
    }
  });

  it("preserves raw fallback for invalid or oversized headers, including after a frame", () => {
    const oversized = frame(1, "raw text");
    byteView(oversized).setUint32(4, 16 * 1024 * 1024 + 1);
    for (const bytes of [fixtureBytes([1, 0, 0]), fixtureBytes("\x01\0\0raw text"), oversized]) {
      for (const framedPrefix of [false, true]) {
        // A short ambiguous prefix at EOF is raw only before framed mode starts.
        if (framedPrefix && bytes.length < 8) continue;
        let output = "";
        const decoder = new DockerLogDecoder((_type, data) => { output += data; });
        if (framedPrefix) decoder.push(frame(1, "prefix "));
        for (const byte of bytes) decoder.push(fixtureBytes([byte]));
        expect(decoder.end()).toBe(true);
        expect(output).toBe((framedPrefix ? "prefix " : "") + new TextDecoder().decode(bytes));
      }
    }
  });

  for (const framed of [false, true]) {
    it(`preserves and redacts Unicode credentials split across ${framed ? "Docker frames" : "TTY chunks"}`, () => {
      const output: string[] = [];
      const secret = "päss🔑word";
      const redactor = new ConsoleOutputRedactor([secret], (value) => output.push(value));
      const decoder = new DockerLogDecoder((_type, value) => redactor.push(value));
      for (const byte of fixtureBytes(`before ${secret} after`)) {
        const payload = fixtureBytes([byte]);
        decoder.push(framed ? frame(1, payload) : payload);
      }
      expect(decoder.end()).toBe(true);
      redactor.end();
      expect(output.join("")).toBe("before [redacted] after");
    });
  }
});

describe("Docker log WebSocket", () => {
  it("follows assigned logs for viewers and rejects input", async () => {
    const logStream = new StreamFixture();
    let logOptions: Record<string, unknown> | null = null;
    docker.getContainer = (() => ({
      inspect: async () => managedInspect("managed-id", true),
      logs: async (options: Record<string, unknown>) => {
        logOptions = options;
        return logStream.readable;
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

    expect(logOptions).toStrictEqual({
      follow: true,
      stdout: true,
      stderr: true,
      tail: 500,
      timestamps: true,
    });
    logStream.enqueue("2026-08-06T12:00:00Z game started\n");
    await new Promise<void>(resolve => setImmediate(resolve));
    expect(ws.sent.some(
        (message) =>
          message.type === "stdout" && message.data.includes("game started"),
      )).toBeTruthy();

    ws.receive(fixtureBytes('{"type":"input","data":"stop"}'));
    expect(ws.sent.some(
        (message) =>
          message.type === "error" &&
          message.data === "Docker logs are read-only",
      )).toBeTruthy();
    ws.close(1000);
    expect(logStream.closed).toBe(true);
  });

  it("closes before reading logs from an unmanaged container", async () => {
    let logsCalled = false;
    docker.getContainer = (() => ({
      inspect: async () => managedInspect("unmanaged-id", false),
      logs: async () => {
        logsCalled = true;
        return new ReadableStream<Uint8Array>();
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

    expect(ws.closeCode).toBe(1008);
    expect(logsCalled).toBe(false);
  });
});

function frame(type: 1 | 2, value: string | Uint8Array): Uint8Array {
  const payload = fixtureBytes(value);
  const result = new Uint8Array(8 + payload.length);
  result[0] = type;
  byteView(result).setUint32(4, payload.length);
  result.set(payload, 8);
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
