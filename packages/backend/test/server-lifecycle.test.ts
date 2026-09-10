import assert from "node:assert/strict";
import { PassThrough } from "node:stream";
import { spawn } from "bun";
import { afterEach, describe, it } from "bun:test";
import { startServer } from "../src/index.js";
import { closeDatabase, getDatabase } from "../src/database.js";
import { getDockerInstance } from "../src/docker.js";
import { acquireLocks } from "../src/operation-locks.js";

process.env.LUDOCK_DB_PATH = ":memory:";
process.env.LUDOCK_API_TOKEN = "lifecycle-fixture-api-token-0123456789";
const docker = getDockerInstance();
const originals = { listContainers: docker.listContainers, getEvents: docker.getEvents };
let runtime: ReturnType<typeof startServer> | undefined;
let releaseLock: (() => void) | undefined;

afterEach(async () => {
  releaseLock?.();
  releaseLock = undefined;
  await runtime?.shutdown("test cleanup");
  runtime = undefined;
  Object.assign(docker, originals);
  closeDatabase();
});

describe("native server shutdown", () => {
  it("stops admission and WebSockets while retaining SQLite until active locks drain", async () => {
    docker.listContainers = (async () => []) as typeof docker.listContainers;
    docker.getEvents = (async () => new PassThrough()) as typeof docker.getEvents;
    runtime = startServer({ port: 0, hostname: "127.0.0.1", frontendDist: false });
    const database = getDatabase();
    releaseLock = acquireLocks(["server:shutdown-fixture"]);
    const client = new WebSocket(new URL("/ws/v1/events", runtime.server.url).href.replace(/^http/, "ws"), {
      headers: { Authorization: `Bearer ${process.env.LUDOCK_API_TOKEN}` },
    });
    try {
      await new Promise<void>((resolve, reject) => {
        client.addEventListener("open", () => resolve(), { once: true });
        client.addEventListener("error", () => reject(new Error("Fixture upgrade failed")), { once: true });
      });
      const closing = new Promise<number>((resolve) => {
        client.addEventListener("close", (event) => resolve(event.code), { once: true });
      });
      let stopped = false;
      const shutdown = runtime.shutdown("test").then(() => { stopped = true; });
      assert.equal(await closing, 1001);
      assert.equal(stopped, false);
      assert.equal(database.prepare("SELECT 1 AS value").get()?.value, 1);
      await assert.rejects(fetch(new URL("/api/v1/health", runtime.server.url), { signal: AbortSignal.timeout(1000) }));
      releaseLock();
      releaseLock = undefined;
      await shutdown;
      assert.equal(stopped, true);
      assert.throws(() => database.prepare("SELECT 1").get(), /closed/i);
    } finally {
      client.terminate();
    }
  });

  it("handles SIGTERM through the executable entry point and exits after cleanup", async () => {
    const child = spawn([process.execPath, "src/index.ts"], {
      cwd: import.meta.dir + "/..",
      env: {
        ...process.env,
        NODE_ENV: "test",
        HOST: "127.0.0.1",
        PORT: "0",
        LOG_LEVEL: "info",
        LUDOCK_DB_PATH: ":memory:",
        DOCKER_SOCKET: "/tmp/ludock-no-daemon-lifecycle.sock",
      },
      stdout: "pipe", stderr: "pipe",
    });
    let output = "";
    let ready!: () => void;
    const listening = new Promise<void>((resolve) => { ready = resolve; });
    const readOutput = (async () => {
      const decoder = new TextDecoder();
      for await (const chunk of child.stdout) {
        output += decoder.decode(chunk, { stream: true });
        if (output.includes("Ludock listening")) ready();
      }
      output += decoder.decode();
    })();
    const errors = new Response(child.stderr).text();
    const deadline = setTimeout(() => child.kill("SIGKILL"), 10_000);
    try {
      await Promise.race([
        listening,
        child.exited.then(() => { throw new Error("Native server exited before listening"); }),
      ]);
      child.kill("SIGTERM");
      assert.equal(await child.exited, 0);
      await readOutput;
      assert.match(output, /Shutdown complete/);
      assert.doesNotMatch(await errors, /error:|Unhandled|SyntaxError/);
    } finally {
      clearTimeout(deadline);
      child.kill("SIGKILL");
      await child.exited;
      await readOutput;
      await errors;
    }
  }, 15_000);
});
