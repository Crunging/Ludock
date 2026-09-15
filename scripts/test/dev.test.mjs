import {
  mkdtemp,
  mkdir,
  copyFile,
  readFile,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { expect, afterAll as after, beforeAll as before, it } from "bun:test";
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
  expect(alias).toStrictEqual(first);
  expect(first.instance).not.toBe(second.instance);
  expect(first.database).not.toBe(second.database);
  expect(first.instance).toMatch(/^[a-f0-9]{12}$/);
  expect(first.backendPort).toBe(first.frontendPort + 1);
  expect(first.dockerConfigured).toBe(false);
  expect(first.dockerSocket.startsWith(first.stateDirectory + path.sep)).toBeTruthy();
  expect(!first.database.startsWith(checkout + path.sep)).toBeTruthy();
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
  expect(config.database).toBe(":memory:");
  expect(config.dockerSocket).toBe("/tmp/development-docker.sock");
  expect(config.fixedPorts).toBe(true);
  expect(config.dockerConfigured).toBe(true);
  for (const value of ["0", "65536", "3000oops", "3.5"]) {
    await expect(developmentConfig(checkout, { LUDOCK_DEV_PORT: value })).rejects.toThrow(/port|between/);
  }
  await expect(developmentConfig(checkout, {
      LUDOCK_DEV_PORT: "4100",
      LUDOCK_DEV_API_PORT: "4100",
    })).rejects.toThrow(/must differ/);
});

it("finds an available pair but never silently moves explicitly requested ports", async () => {
  const blocker = Bun.listen({
    hostname: "127.0.0.1", port: 0, exclusive: true,
    socket: { data(socket) { socket.terminate(); } },
  });
  const blockedPort = blocker.port;
  try {
    await expect(reserveDevelopmentPorts({
        frontendPort: blockedPort,
        backendPort: blockedPort + 1,
        fixedPorts: true,
      })).rejects.toThrow(/already in use/);
    const selected = await reserveDevelopmentPorts({
      frontendPort: blockedPort,
      backendPort: blockedPort + 1,
      fixedPorts: false,
    });
    try {
      expect(selected.frontendPort).not.toBe(blockedPort);
      await expect(reserveDevelopmentPorts({ ...selected, fixedPorts: true })).rejects.toThrow(/already in use/);
    } finally {
      await selected.release();
      await selected.release();
    }
  } finally {
    blocker.stop(true);
  }
});

it("releases the first reservation when the requested API port is occupied", async () => {
  const config = await developmentConfig(path.join(directory, "checkout"), {});
  const selected = await reserveDevelopmentPorts(config);
  await selected.release();
  const blocker = Bun.listen({
    hostname: "127.0.0.1", port: selected.backendPort, exclusive: true,
    socket: { data(socket) { socket.terminate(); } },
  });
  try {
    await expect(reserveDevelopmentPorts({ ...selected, fixedPorts: true })).rejects.toThrow(/already in use/);
    // The frontend reservation from the rejected pair must be available now.
    const frontend = Bun.listen({
      hostname: "127.0.0.1", port: selected.frontendPort, exclusive: true,
      socket: { data(socket) { socket.terminate(); } },
    });
    frontend.stop(true);
  } finally {
    blocker.stop(true);
  }
  const reacquired = await reserveDevelopmentPorts({ ...selected, fixedPorts: true });
  await reacquired.release();
});

it("hands off reserved ports even when a browser reconnects during startup", async () => {
  const config = await developmentConfig(path.join(directory, "checkout"), {});
  const selected = await reserveDevelopmentPorts(config);
  const sockets = [];
  const closed = [];
  let timer;
  try {
    await Promise.all(
      [selected.frontendPort, selected.backendPort].map((port) => {
        const finished = Promise.withResolvers();
        closed.push(finished.promise);
        return Bun.connect({
          hostname: "127.0.0.1", port,
          socket: {
            open(socket) {
              sockets.push(socket);
              socket.write(
                "GET /api/v1/health HTTP/1.1\r\nHost: localhost\r\n\r\n",
              );
            },
            data() {},
            // Resetting a reservation connection is intentional.
            error() {},
            close() { finished.resolve(); },
          },
        });
      }),
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
    const reacquired = await reserveDevelopmentPorts({ ...selected, fixedPorts: true });
    await reacquired.release();
  } finally {
    clearTimeout(timer);
    sockets.forEach((socket) => socket.terminate());
    await selected.release();
  }
});

it("prevents overlapping checkout runs and removes only its own locks", async () => {
  const config = await developmentConfig(path.join(directory, "checkout"), {
    LUDOCK_DEV_HOME: path.join(directory, "locks"),
  });
  const release = await lockDevelopmentState(config);
  await expect(lockDevelopmentState(config)).rejects.toThrow(/already has a development lock/);
  const lockFile = path.join(config.stateDirectory, "dev.lock");
  const owner = JSON.parse(await readFile(lockFile, "utf8"));
  expect(owner.pid).toBe(process.pid);
  await release();
  const releaseAgain = await lockDevelopmentState(config);
  await writeFile(lockFile, JSON.stringify({ nonce: "a different owner" }));
  await releaseAgain();
  expect(JSON.parse(await readFile(lockFile, "utf8")).nonce).toBe("a different owner");
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
    await expect(lockDevelopmentState(second)).rejects.toThrow(/already has a development lock/);
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
    expect(await verifyDevelopmentBackend(identity, port)).toBe(true);
    identity = "abcdef123456";
    await expect(verifyDevelopmentBackend("012345abcdef", port)).rejects.toThrow(/different backend/);
  } finally {
    await server.stop(true);
  }
  expect(await verifyDevelopmentBackend(identity, port)).toBe(false);
});

it("retains development locks until children drain after repeated shutdown signals", async () => {
  const checkout = path.join(directory, "shutdown-checkout");
  for (const folder of [
    "scripts",
    "packages/shared/src",
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
    expect.unreachable(`Development fixture did not report ${event}`);
  };
  try {
    await waitFor("frontend-started");
    child.kill("SIGTERM");
    await waitFor("backend-stopping");
    child.kill("SIGTERM");
    child.kill("SIGINT");
    await Bun.sleep(20);
    expect(child.exitCode, "Launcher must remain alive while the backend drains").toBe(null);
    await expect(lockDevelopmentState(config)).rejects.toThrow(/already has a development lock/);
    expect(await child.exited).toBe(0);
    expect(await readFile(events, "utf8")).toMatch(/backend-drained\n/);
    const release = await lockDevelopmentState(config);
    await release();
  } finally {
    if (child.exitCode === null) {
      child.kill("SIGTERM");
      await child.exited;
    }
  }
}, 10_000);
