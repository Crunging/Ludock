import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
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

  for (const exitCode of [0, 7]) it(`drains CLI shutdown once despite repeated SIGTERM and lingering handles, preserving exit code ${exitCode}`, async () => {
    const directory = await mkdtemp(path.join(os.tmpdir(), "ludock-cli-shutdown-"));
    const preload = path.join(directory, "fixture.ts");
    await Bun.write(preload, `
      import { getDatabase } from ${JSON.stringify(new URL("../src/database.ts", import.meta.url).href)};
      import { acquireLocks, isServerBusy } from ${JSON.stringify(new URL("../src/operation-locks.ts", import.meta.url).href)};
      const database = getDatabase();
      const key = "server:cli-shutdown-fixture";
      const release = acquireLocks([key]);
      let released = false;
      let signals = 0;
      process.exitCode = ${exitCode};
      // A persistent handle models verified native Bun.connect retention without
      // depending on operating-system backlog timing to leave a connect pending.
      setInterval(() => {}, 1_000);
      process.on("SIGTERM", () => {
        signals += 1;
        console.log("Fixture SIGTERM observed " + signals);
        if (signals !== 2) return;
        setTimeout(() => {
          try {
            if (database.prepare("SELECT 1 AS value").get()?.value !== 1)
              throw new Error("Fixture query failed");
            console.log("Fixture database usable before release");
            release();
            released = true;
            console.log("Fixture lock released");
          } catch {
            console.error("Fixture database closed before operation lock release");
            process.exitCode = 93;
            release();
          }
        }, 200);
      });
      process.on("exit", () => {
        let databaseClosed = false;
        try { database.prepare("SELECT 1").get(); }
        catch { databaseClosed = true; }
        const lockReleased = released && !isServerBusy(key.slice("server:".length));
        console.log("Fixture exit " + JSON.stringify({ databaseClosed, lockReleased, signals }));
        if (!databaseClosed || !lockReleased || signals !== 2) process.exitCode = 94;
      });
    `);
    const child = spawn([process.execPath, "--preload", preload, "src/index.ts"], {
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
    const waiting = new Map<string, () => void>();
    const waitForOutput = (message: string) => Promise.race([
      new Promise<void>((resolve) => {
        if (output.includes(message)) resolve();
        else waiting.set(message, resolve);
      }),
      child.exited.then(() => { throw new Error(`Native server exited before '${message}': ${output}`); }),
    ]);
    const readOutput = (async () => {
      const decoder = new TextDecoder();
      for await (const chunk of child.stdout) {
        output += decoder.decode(chunk, { stream: true });
        for (const [message, ready] of waiting) if (output.includes(message)) {
          waiting.delete(message);
          ready();
        }
      }
      output += decoder.decode();
    })();
    const errors = new Response(child.stderr).text();
    const deadline = setTimeout(() => child.kill("SIGKILL"), 10_000);
    try {
      await waitForOutput("Ludock listening");
      child.kill("SIGTERM");
      await waitForOutput("Fixture SIGTERM observed 1");
      child.kill("SIGTERM");
      await waitForOutput("Fixture SIGTERM observed 2");
      assert.equal(await child.exited, exitCode);
      await readOutput;
      assert.equal(output.match(/Shutting down/g)?.length, 1, output);
      assert.equal(output.match(/Shutdown complete/g)?.length, 1, output);
      const markers = [
        "Fixture SIGTERM observed 1",
        "Fixture SIGTERM observed 2",
        "Fixture database usable before release",
        "Fixture lock released",
        "Shutdown complete",
        'Fixture exit {"databaseClosed":true,"lockReleased":true,"signals":2}',
      ];
      let previous = -1;
      for (const marker of markers) {
        const index = output.indexOf(marker);
        assert.ok(index > previous, `Expected '${marker}' in shutdown order: ${output}`);
        previous = index;
      }
      assert.doesNotMatch(await errors, /error:|Unhandled|SyntaxError/);
    } finally {
      clearTimeout(deadline);
      child.kill("SIGKILL");
      await child.exited;
      await readOutput;
      await errors;
      await rm(directory, { recursive: true, force: true });
    }
  }, 15_000);
});
