import assert from "node:assert/strict";
import {
  mkdtemp,
  mkdir,
  copyFile,
  readFile,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import { createConnection, createServer } from "node:net";
import os from "node:os";
import path from "node:path";
import { afterAll as after, beforeAll as before, it } from "bun:test";
import {
  developmentConfig,
  lockDevelopmentState,
  reserveDevelopmentPorts,
  verifyDevelopmentBackend,
} from "../dev.mjs";

let directory;
before(async () => {
  directory = await mkdtemp(path.join(os.tmpdir(), "ludock-dev-tests-"));
  await mkdir(path.join(directory, "checkout"));
  await mkdir(path.join(directory, "worktree"));
});
after(async () => {
  await rm(directory, { recursive: true, force: true });
});

it("isolates checkout state and cookies while canonical aliases share one identity", async () => {
  const env = { LUDOCK_DEV_HOME: path.join(directory, "state") };
  const checkout = path.join(directory, "checkout");
  await symlink(checkout, path.join(directory, "alias"), "dir");
  const first = await developmentConfig(checkout, env);
  const alias = await developmentConfig(path.join(directory, "alias"), env);
  const second = await developmentConfig(path.join(directory, "worktree"), env);
  assert.deepEqual(alias, first);
  assert.notEqual(first.instance, second.instance);
  assert.notEqual(first.database, second.database);
  assert.match(first.instance, /^[a-f0-9]{12}$/);
  assert.equal(first.backendPort, first.frontendPort + 1);
  assert.equal(first.dockerConfigured, false);
  assert.ok(first.dockerSocket.startsWith(first.stateDirectory + path.sep));
  assert.ok(!first.database.startsWith(checkout + path.sep));
});

it("honors explicit configuration and rejects conflicting or invalid ports", async () => {
  const checkout = path.join(directory, "checkout");
  const config = await developmentConfig(checkout, {
    LUDOCK_DEV_HOME: path.join(directory, "custom-state"),
    LUDOCK_DEV_PORT: "4100",
    LUDOCK_DEV_API_PORT: "4101",
    LUDOCK_DB_PATH: ":memory:",
    DOCKER_SOCKET: "/tmp/development-docker.sock",
  });
  assert.equal(config.database, ":memory:");
  assert.equal(config.dockerSocket, "/tmp/development-docker.sock");
  assert.equal(config.fixedPorts, true);
  assert.equal(config.dockerConfigured, true);
  for (const value of ["0", "65536", "3000oops", "3.5"]) {
    await assert.rejects(
      developmentConfig(checkout, { LUDOCK_DEV_PORT: value }),
      /port|between/,
    );
  }
  await assert.rejects(
    developmentConfig(checkout, {
      LUDOCK_DEV_PORT: "4100",
      LUDOCK_DEV_API_PORT: "4100",
    }),
    /must differ/,
  );
});

it("finds an available pair but never silently moves explicitly requested ports", async () => {
  const blocker = createServer();
  await new Promise((resolve) => blocker.listen(0, "127.0.0.1", resolve));
  const blockedPort = blocker.address().port;
  try {
    await assert.rejects(
      reserveDevelopmentPorts({
        frontendPort: blockedPort,
        backendPort: blockedPort + 1,
        fixedPorts: true,
      }),
      /already in use/,
    );
    const selected = await reserveDevelopmentPorts({
      frontendPort: blockedPort,
      backendPort: blockedPort + 1,
      fixedPorts: false,
    });
    try {
      assert.notEqual(selected.frontendPort, blockedPort);
      await assert.rejects(
        reserveDevelopmentPorts({ ...selected, fixedPorts: true }),
        /already in use/,
      );
    } finally {
      await selected.release();
      await selected.release();
    }
  } finally {
    await new Promise((resolve) => blocker.close(resolve));
  }
});

it("hands off reserved ports even when a browser reconnects during startup", async () => {
  const config = await developmentConfig(path.join(directory, "checkout"), {});
  const selected = await reserveDevelopmentPorts(config);
  const sockets = [selected.frontendPort, selected.backendPort].map((port) =>
    createConnection({ host: "127.0.0.1", port }),
  );
  const closed = sockets.map(
    (socket) =>
      new Promise((resolve) => {
        // Resetting a reservation connection is intentional.
        socket.on("error", () => {});
        socket.once("close", resolve);
      }),
  );
  let timer;
  try {
    await Promise.all(
      sockets.map(
        (socket) =>
          new Promise((resolve) => {
            socket.once("connect", () => {
              socket.write(
                "GET /api/v1/health HTTP/1.1\r\nHost: localhost\r\n\r\n",
              );
              resolve();
            });
          }),
      ),
    );
    await Promise.race([
      Promise.all([...closed, selected.release()]),
      new Promise((_, reject) => {
        timer = setTimeout(
          () =>
            reject(new Error("Browser connections prevented port handoff.")),
          1000,
        );
      }),
    ]);
  } finally {
    clearTimeout(timer);
    sockets.forEach((socket) => socket.destroy());
    await selected.release();
  }
});

it("prevents overlapping checkout runs and removes only its own locks", async () => {
  const config = await developmentConfig(path.join(directory, "checkout"), {
    LUDOCK_DEV_HOME: path.join(directory, "locks"),
  });
  const release = await lockDevelopmentState(config);
  await assert.rejects(
    lockDevelopmentState(config),
    /already has a development lock/,
  );
  const lockFile = path.join(config.stateDirectory, "dev.lock");
  const owner = JSON.parse(await readFile(lockFile, "utf8"));
  assert.equal(owner.pid, process.pid);
  await release();
  const releaseAgain = await lockDevelopmentState(config);
  await writeFile(lockFile, JSON.stringify({ nonce: "a different owner" }));
  await releaseAgain();
  assert.equal(
    JSON.parse(await readFile(lockFile, "utf8")).nonce,
    "a different owner",
  );
});

it("locks a shared database override across checkouts and directory aliases", async () => {
  const databaseDirectory = path.join(directory, "shared-database");
  await mkdir(databaseDirectory);
  await symlink(
    databaseDirectory,
    path.join(directory, "database-alias"),
    "dir",
  );
  const first = await developmentConfig(path.join(directory, "checkout"), {
    LUDOCK_DEV_HOME: path.join(directory, "db-locks"),
    LUDOCK_DB_PATH: path.join(databaseDirectory, "ludock.db"),
  });
  const second = await developmentConfig(path.join(directory, "worktree"), {
    LUDOCK_DEV_HOME: path.join(directory, "db-locks"),
    LUDOCK_DB_PATH: path.join(directory, "database-alias/ludock.db"),
  });
  const release = await lockDevelopmentState(first);
  try {
    await assert.rejects(
      lockDevelopmentState(second),
      /already has a development lock/,
    );
  } finally {
    await release();
  }
  // The failed attempt must release its own checkout lock as well.
  const releaseSecond = await lockDevelopmentState(second);
  await releaseSecond();
});

it("checks backend instance identity before accepting an occupied API port", async () => {
  let identity = "012345abcdef";
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch: () => Response.json({ status: "degraded" }, {
      status: 503,
      headers: { "X-Ludock-Dev-Instance": identity },
    }),
  });
  const port = server.port;
  try {
    assert.equal(await verifyDevelopmentBackend(identity, port), true);
    identity = "abcdef123456";
    await assert.rejects(
      verifyDevelopmentBackend("012345abcdef", port),
      /different backend/,
    );
  } finally {
    await server.stop(true);
  }
  assert.equal(await verifyDevelopmentBackend(identity, port), false);
});

it("retains development locks until children drain after repeated shutdown signals", async () => {
  const checkout = path.join(directory, "shutdown-checkout");
  for (const folder of [
    "scripts",
    "packages/shared/node_modules/typescript/bin",
    "packages/shared/dist",
    "packages/backend/src",
    "packages/frontend/scripts",
  ]) await mkdir(path.join(checkout, folder), { recursive: true });
  for (const filename of ["dev.mjs", "watch-backend.mjs"]) {
    await copyFile(
      new URL(`../${filename}`, import.meta.url),
      path.join(checkout, "scripts", filename),
    );
  }
  await writeFile(path.join(checkout, "packages/shared/package.json"), "{}");
  await writeFile(
    path.join(checkout, "packages/shared/node_modules/typescript/bin/tsc"),
    `if (process.argv.includes("--watch")) {
      const timer = setInterval(() => {}, 1000);
      process.on("SIGTERM", () => clearInterval(timer));
    }`,
  );
  const events = path.join(checkout, "events");
  await writeFile(path.join(checkout, "packages/backend/src/index.ts"), `
    import { appendFileSync } from "node:fs";
    const events = ${JSON.stringify(events)};
    const server = Bun.serve({
      hostname: "127.0.0.1", port: Number(process.env.PORT),
      fetch: () => Response.json({ status: "ok" }, {
        headers: { "x-ludock-dev-instance": process.env.LUDOCK_DEV_INSTANCE },
      }),
    });
    let stopping = false;
    process.on("SIGTERM", () => {
      if (stopping) return;
      stopping = true;
      appendFileSync(events, "backend-stopping\\n");
      setTimeout(async () => {
        await server.stop(true);
        appendFileSync(events, "backend-drained\\n");
      }, 500);
    });
  `);
  await writeFile(path.join(checkout, "packages/frontend/scripts/dev.ts"), `
    import { appendFileSync } from "node:fs";
    appendFileSync(${JSON.stringify(events)}, "frontend-started\\n");
    const timer = setInterval(() => {}, 1000);
    process.on("SIGTERM", () => clearInterval(timer));
  `);
  const env = {
    ...process.env,
    LUDOCK_DEV_HOME: path.join(directory, "shutdown-state"),
    DOCKER_SOCKET: "",
  };
  const config = await developmentConfig(checkout, env);
  const child = Bun.spawn([process.execPath, "scripts/dev.mjs"], {
    cwd: checkout, env, stdin: "ignore", stdout: "ignore", stderr: "pipe",
  });
  const waitFor = async (event) => {
    for (let attempt = 0; attempt < 300; attempt++) {
      const actual = await readFile(events, "utf8").catch(() => "");
      if (actual.includes(`${event}\n`)) return;
      if (child.exitCode !== null) {
        throw new Error(`Development run exited: ${await new Response(child.stderr).text()}`);
      }
      await Bun.sleep(10);
    }
    assert.fail(`Development fixture did not report ${event}`);
  };
  try {
    await waitFor("frontend-started");
    child.kill("SIGTERM");
    await waitFor("backend-stopping");
    child.kill("SIGTERM");
    child.kill("SIGINT");
    await Bun.sleep(20);
    assert.equal(child.exitCode, null, "Launcher must remain alive while the backend drains");
    await assert.rejects(lockDevelopmentState(config), /already has a development lock/);
    assert.equal(await child.exited, 0);
    assert.match(await readFile(events, "utf8"), /backend-drained\n/);
    const release = await lockDevelopmentState(config);
    await release();
  } finally {
    if (child.exitCode === null) {
      child.kill("SIGTERM");
      await child.exited;
    }
  }
}, 10_000);
