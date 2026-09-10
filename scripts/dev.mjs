#!/usr/bin/env bun
import { createHash, randomUUID } from "node:crypto";
import {
  lstat,
  mkdir,
  open,
  readFile,
  realpath,
  stat,
  unlink,
} from "node:fs/promises";
import { createRequire } from "node:module";
import { createServer } from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { setTimeout as delay } from "node:timers/promises";

const repository = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
);

function port(value, name) {
  if (!/^\d+$/.test(String(value)))
    throw new Error(`${name} must be a port number.`);
  const number = Number(value);
  if (!Number.isInteger(number) || number < 1024 || number > 65535) {
    throw new Error(`${name} must be between 1024 and 65535.`);
  }
  return number;
}

export async function developmentConfig(
  checkout,
  env = process.env,
  home = os.homedir(),
) {
  const directory = await realpath(checkout);
  const instance = createHash("sha256")
    .update(directory)
    .digest("hex")
    .slice(0, 12);
  const preferredPort =
    30000 + (Number.parseInt(instance.slice(0, 8), 16) % 14000) * 2;
  const frontendPort = port(
    env.LUDOCK_DEV_PORT ?? preferredPort,
    "LUDOCK_DEV_PORT",
  );
  const backendPort = port(
    env.LUDOCK_DEV_API_PORT ?? env.PORT ?? frontendPort + 1,
    "LUDOCK_DEV_API_PORT",
  );
  if (frontendPort === backendPort)
    throw new Error("Development frontend and API ports must differ.");
  const homeDirectory = path.resolve(
    directory,
    env.LUDOCK_DEV_HOME || path.join(home, ".local/state/ludock/dev"),
  );
  const name = path
    .basename(directory)
    .replace(/[^a-zA-Z0-9_-]/g, "-")
    .slice(0, 40);
  const stateDirectory = path.join(homeDirectory, `${name}-${instance}`);
  return {
    directory,
    instance,
    stateDirectory,
    database: env.LUDOCK_DB_PATH
      ? env.LUDOCK_DB_PATH === ":memory:"
        ? ":memory:"
        : path.resolve(directory, env.LUDOCK_DB_PATH)
      : path.join(stateDirectory, "ludock.db"),
    dockerSocket:
      env.DOCKER_SOCKET ||
      path.join(stateDirectory, "docker-disconnected.sock"),
    dockerConfigured: Boolean(env.DOCKER_SOCKET),
    frontendPort,
    backendPort,
    fixedPorts:
      env.LUDOCK_DEV_PORT !== undefined ||
      env.LUDOCK_DEV_API_PORT !== undefined ||
      env.PORT !== undefined,
  };
}

function reservePort(number) {
  return new Promise((resolve, reject) => {
    // Existing browser tabs may reconnect during startup. Reservations do not
    // serve requests, and accepted sockets must not delay handing off the port.
    const server = createServer((socket) => socket.destroy());
    server.once("error", reject);
    server.listen(number, "127.0.0.1", () => resolve(server));
  });
}
function closePort(server) {
  return new Promise((resolve) => server.close(resolve));
}

export async function reserveDevelopmentPorts(config) {
  for (let offset = 0; offset < (config.fixedPorts ? 1 : 100); offset++) {
    const frontendPort = config.frontendPort + offset * 2;
    const backendPort = config.backendPort + offset * 2;
    if (Math.max(frontendPort, backendPort) > 65535) break;
    const reservations = [];
    try {
      reservations.push(await reservePort(frontendPort));
      reservations.push(await reservePort(backendPort));
      let released = false;
      return {
        frontendPort,
        backendPort,
        release: async () => {
          if (released) return;
          released = true;
          await Promise.all(reservations.map(closePort));
        },
      };
    } catch (error) {
      await Promise.all(reservations.map(closePort));
      if (error.code !== "EADDRINUSE") throw error;
      if (config.fixedPorts)
        throw new Error(
          `A requested development port (${frontendPort} or ${backendPort}) is already in use.`,
        );
    }
  }
  throw new Error(
    "No free development port pair was found. Set LUDOCK_DEV_PORT and LUDOCK_DEV_API_PORT.",
  );
}

async function lockFile(file, instance) {
  let handle;
  try {
    handle = await open(file, "wx", 0o600);
  } catch (error) {
    if (error.code !== "EEXIST") throw error;
    throw new Error(
      `This resource already has a development lock: ${file}. Stop its development run first. If that run crashed, verify its recorded PID has ended before removing this lock.`,
    );
  }
  const nonce = randomUUID();
  try {
    await handle.writeFile(
      JSON.stringify({
        pid: process.pid,
        instance,
        nonce,
        startedAt: new Date().toISOString(),
      }) + "\n",
    );
  } catch (error) {
    await unlink(file);
    throw error;
  } finally {
    await handle.close();
  }
  return async () => {
    const current = JSON.parse(await readFile(file, "utf8"));
    if (current.nonce === nonce) await unlink(file);
  };
}

export async function lockDevelopmentState(config) {
  await mkdir(config.stateDirectory, { recursive: true, mode: 0o700 });
  const releases = [];
  try {
    releases.push(
      await lockFile(
        path.join(config.stateDirectory, "dev.lock"),
        config.instance,
      ),
    );
    if (config.database !== ":memory:") {
      await mkdir(path.dirname(config.database), {
        recursive: true,
        mode: 0o700,
      });
      // Resolve directory aliases as well as existing file aliases, so two
      // checkouts cannot bypass the database lock with different path spellings.
      const existing = await lstat(config.database).catch((error) => {
        if (error.code !== "ENOENT") throw error;
        return undefined;
      });
      const database = existing
        ? await realpath(config.database)
        : path.join(
            await realpath(path.dirname(config.database)),
            path.basename(config.database),
          );
      if (existing) {
        const metadata = await stat(database);
        if (!metadata.isFile() || metadata.nlink !== 1)
          throw new Error(
            "The development database must be a regular file without hard links.",
          );
      }
      releases.push(await lockFile(`${database}.dev.lock`, config.instance));
    }
  } catch (error) {
    await Promise.all(releases.map((release) => release()));
    throw error;
  }
  return async () => {
    await Promise.all(releases.map((release) => release()));
  };
}

export async function verifyDevelopmentBackend(instance, backendPort) {
  let response;
  try {
    response = await fetch(`http://127.0.0.1:${backendPort}/api/v1/health`, {
      signal: AbortSignal.timeout(1000),
    });
  } catch {
    return false;
  }
  await response.body?.cancel();
  if (response.headers.get("x-ludock-dev-instance") !== instance) {
    throw new Error(
      `Port ${backendPort} belongs to a different backend. Stopping this development run.`,
    );
  }
  return true;
}

function script(packageName, module) {
  return createRequire(
    path.join(repository, "packages", packageName, "package.json"),
  ).resolve(module);
}

export async function runDevelopment(config) {
  const unlock = await lockDevelopmentState(config);
  let ports;
  let backendMonitor;
  const children = new Set();
  let stopping = false;
  let stopped;
  const done = new Promise((resolve) => {
    stopped = resolve;
  });
  const stop = () => {
    if (stopping) {
      if (children.size === 0) stopped();
      return;
    }
    stopping = true;
    for (const child of children) child.kill("SIGTERM");
    if (children.size === 0) stopped();
  };
  const launch = (label, executable, args, cwd, env) => {
    if (stopping) return;
    const child = Bun.spawn([executable, ...args], {
      cwd,
      env,
      stdin: "ignore",
      stdout: "inherit",
      stderr: "inherit",
      detached: process.platform !== "win32",
    });
    children.add(child);
    void child.exited.then((code) => {
      children.delete(child);
      if (!stopping) {
        console.error(
          `${label} stopped (${child.signalCode || code}). Stopping the development run.`,
        );
        process.exitCode = 1;
        stop();
      }
      if (stopping && children.size === 0) stopped();
    });
    return child;
  };
  // Repeated signals must keep waiting for children to drain; removing a
  // one-shot listener would restore immediate termination during cleanup.
  process.on("SIGINT", stop);
  process.on("SIGTERM", stop);
  try {
    ports = await reserveDevelopmentPorts(config);
    if (stopping) return;
    const tsc = script("shared", "typescript/bin/tsc");
    // Build once before either app imports the contracts, then watch all three
    // packages. The backend watcher waits for shutdown before each restart.
    const built = Bun.spawn([process.execPath, tsc], {
      cwd: path.join(config.directory, "packages/shared"),
      stdin: "ignore",
      stdout: "inherit",
      stderr: "inherit",
      detached: process.platform !== "win32",
    });
    children.add(built);
    const buildCode = await built.exited.finally(() => {
      children.delete(built);
      if (stopping && children.size === 0) stopped();
    });
    if (stopping) return;
    if (buildCode !== 0) throw new Error("Shared contract build failed.");
    const env = {
      ...process.env,
      NODE_ENV: "development",
      HOST: "127.0.0.1",
      PORT: String(ports.backendPort),
      LUDOCK_DB_PATH: config.database,
      LUDOCK_DEV_INSTANCE: config.instance,
      LUDOCK_DEV_API_ORIGIN: `http://127.0.0.1:${ports.backendPort}`,
      DOCKER_SOCKET: config.dockerSocket,
    };
    await ports.release();
    if (stopping) return;
    launch(
      "Shared contracts",
      process.execPath,
      [tsc, "--watch", "--preserveWatchOutput"],
      path.join(config.directory, "packages/shared"),
      env,
    );
    launch(
      "Backend",
      process.execPath,
      [
        path.join(config.directory, "scripts/watch-backend.mjs"),
      ],
      path.join(config.directory, "packages/backend"),
      env,
    );
    let ready = false;
    for (let attempt = 0; attempt < 100 && !stopping; attempt++) {
      if (await verifyDevelopmentBackend(config.instance, ports.backendPort)) {
        ready = true;
        break;
      }
      await delay(100);
    }
    if (stopping) return;
    if (!ready)
      throw new Error(
        "The development backend did not become ready. Check its output above.",
      );
    console.log(
      `Ludock development: http://127.0.0.1:${ports.frontendPort}\nAPI: http://127.0.0.1:${ports.backendPort}\nDatabase: ${config.database}\nDocker: ${config.dockerConfigured ? config.dockerSocket : "disconnected (set DOCKER_SOCKET for a development daemon)"}`,
    );
    launch(
      "Frontend",
      process.execPath,
      [
        "scripts/dev.ts",
        "--host",
        "127.0.0.1",
        "--port",
        String(ports.frontendPort),
        "--strictPort",
      ],
      path.join(config.directory, "packages/frontend"),
      env,
    );
    let checking = false;
    backendMonitor = setInterval(() => {
      if (checking || stopping) return;
      checking = true;
      void verifyDevelopmentBackend(config.instance, ports.backendPort)
        .catch((error) => {
          console.error(error.message);
          process.exitCode = 1;
          stop();
        })
        .finally(() => {
          checking = false;
        });
    }, 2000);
    backendMonitor.unref();
    await done;
  } finally {
    clearInterval(backendMonitor);
    stop();
    await done;
    await ports?.release();
    await unlock();
    process.removeListener("SIGINT", stop);
    process.removeListener("SIGTERM", stop);
  }
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  const args = process.argv.slice(2).filter((argument) => argument !== "--");
  try {
    if (args.includes("--help")) {
      console.log(
        "bun run dev [--print-config]\nPer-checkout storage, ports and cookies. Overrides: LUDOCK_DEV_HOME, LUDOCK_DEV_PORT, LUDOCK_DEV_API_PORT, LUDOCK_DB_PATH, DOCKER_SOCKET. Stop with Ctrl+C.",
      );
    } else {
      if (args.some((argument) => argument !== "--print-config"))
        throw new Error("Unknown development option. Run bun run dev --help.");
      const config = await developmentConfig(repository);
      if (args.includes("--print-config"))
        console.log(JSON.stringify(config, null, 2));
      else await runDevelopment(config);
    }
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
