// Builds Ludock and the fake Docker daemon, then starts one of each on fresh
// data for every worker, completes first-administrator setup, and saves the
// administrator's signed-in state.
import { spawn, execFileSync } from "node:child_process";
import { createWriteStream, existsSync, mkdirSync, mkdtempSync, statfsSync, writeFileSync, rmSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { port } from "./playwright.config.mjs";

const root = resolve(import.meta.dirname, "..");
export const admin = { username: "admin", password: "correct horse battery staple" };
const setupCode = "e2e-setup-code-0123456789abcdef-0123";

const exited = (child) => child.exitCode !== null || child.signalCode !== null || child.failed;

// waitFor waits for check to pass, failing at once with child's output if
// child exits first.
async function waitFor(check, what, child, timeout = 30_000) {
  const deadline = Date.now() + timeout;
  for (;;) {
    if (exited(child)) {
      throw new Error(`${child.log} exited while waiting for ${what}:\n${child.lines.join("")}`);
    }
    try {
      if (await check()) return;
    } catch {
      // Not ready yet.
    }
    if (Date.now() > deadline) throw new Error(`Timed out waiting for ${what}:\n${child.lines.join("")}`);
    await new Promise((done) => setTimeout(done, 100));
  }
}

// Backups keep 5 GiB free in their folder and refuse to run below it, so
// stacks live in the temporary folder only when it has room to spare, as a
// small memory-backed /tmp often doesn't.
const room = 8 * 1024 ** 3;
function free(path) {
  try {
    const stat = statfsSync(path);
    return stat.bavail * stat.bsize;
  } catch {
    return 0;
  }
}
function scratch() {
  if (free(tmpdir()) >= room) return tmpdir();
  const cache = join(process.env.XDG_CACHE_HOME || join(homedir(), ".cache"), "ludock-e2e");
  mkdirSync(cache, { recursive: true });
  return free(cache) > free(tmpdir()) ? cache : tmpdir();
}

// start runs a child process; file, if given, keeps its output for journeys
// that read Ludock's log.
function start(command, args, env, log, file) {
  const child = spawn(command, args, { env: { ...process.env, ...env }, stdio: ["ignore", "pipe", "pipe"] });
  const lines = [];
  Object.assign(child, { lines, log });
  // A command that can't start never exits; its error is its output.
  child.on("error", (error) => {
    lines.push(String(error));
    child.failed = true;
  });
  const saved = file && createWriteStream(file);
  for (const stream of [child.stdout, child.stderr]) {
    stream.on("data", (chunk) => {
      lines.push(chunk.toString());
      saved?.write(chunk);
      if (process.env.LUDOCK_E2E_LOGS) process.stdout.write(`[${log}] ${chunk}`);
    });
  }
  child.on("exit", (code) => {
    if (code && !child.stopping) console.error(`${log} exited with ${code}:\n${lines.join("")}`);
  });
  return child;
}

// startStack starts one Ludock and fake daemon on fresh data under dir and
// signs in the setup administrator, adding each process to children as it
// starts. Each worker gets its own stack; a journey that needs a fresh
// install starts another with freshStack. port 0 has the system choose one.
// With found, the folder dir/found stands in for one mounted at
// /backups-copy.
async function startStack(bin, dir, port, { found = false, children = [] } = {}) {
  const dockerDir = join(dir, "docker");
  // Backups go to the first root; the backups journey chooses the second
  // for second copies.
  const backups = join(dir, "backups") + ":" + join(dir, "copies");
  const mounted = found ? backups + ":" + join(dir, "found") : backups;
  const fake = start(join(bin, "fakedocker"), ["-dir", dockerDir, "-backups", mounted, "-data", join(dir, "data")], {}, "fakedocker");
  children.push(fake);
  const socket = join(dockerDir, "docker.sock");
  await waitFor(() => existsSync(socket), "the fake Docker socket", fake);

  const launch = async (listen) => {
    const ludock = start(join(bin, "ludock"), ["serve"], {
      LUDOCK_LISTEN: `127.0.0.1:${listen}`,
      LUDOCK_DATA_DIR: join(dir, "data"),
      LUDOCK_DOCKER_SOCKET: socket,
      LUDOCK_SETUP_CODE: setupCode,
      LUDOCK_SELF_CONTAINER: "ludock-dev",
      LUDOCK_BACKUP_ROOTS: backups,
      LUDOCK_HELPER_IMAGE: "ghcr.io/crunging/ludock@sha256:" + "ab".repeat(32),
      // Its default level logs the address it listens on, which this reads.
      LUDOCK_LOG_LEVEL: "",
      ...(found ? { LUDOCK_DEV_COPY_ROOT: join(dir, "found") } : {}),
    }, "ludock", join(dir, "ludock.log"));
    children.push(ludock);
    // The port this Ludock bound, from its own log, so nothing else that
    // answers on a port can stand in for it.
    let listening;
    await waitFor(() => (listening = ludock.lines.join("").match(/Ludock listening.*?localhost:(\d+)"/)), "Ludock to listen", ludock);
    const url = `http://127.0.0.1:${listening[1]}`;
    await waitFor(async () => (await fetch(`${url}/api/v1/health`)).ok, "Ludock's health check", ludock);
    return { ludock, url, port: listening[1] };
  };
  let running = await launch(port);
  const baseURL = running.url;
  // restart stops Ludock and starts it again on the same data and port, as
  // an update of Ludock would, leaving the fake daemon and its games as
  // they are.
  const restart = async () => {
    await stopStack({ children: [running.ludock] });
    children.splice(children.indexOf(running.ludock), 1);
    running = await launch(running.port);
  };

  const form = (values) => ({ method: "POST", redirect: "manual", body: new URLSearchParams(values),
    headers: { "Content-Type": "application/x-www-form-urlencoded" } });
  const setupResponse = await fetch(`${baseURL}/setup`, form({ bootstrapCode: setupCode, username: admin.username,
    password: admin.password, confirmation: admin.password }));
  if (setupResponse.status !== 303) throw new Error(`Setup failed with ${setupResponse.status}`);
  const cookie = setupResponse.headers.getSetCookie().find((value) => value.startsWith("ludock_session="));
  if (!cookie) throw new Error("Setup returned no session");
  const token = cookie.split(";")[0].split("=")[1];
  const state = join(dir, "admin.json");
  writeFileSync(state, JSON.stringify({ cookies: [{ name: "ludock_session", value: token, domain: "127.0.0.1", path: "/",
    expires: -1, httpOnly: true, secure: false, sameSite: "Strict" }], origins: [] }));
  return { children, restart, env: { LUDOCK_E2E_URL: baseURL, LUDOCK_E2E_ADMIN_STATE: state, LUDOCK_E2E_DIR: dir } };
}

// stopStack stops Ludock before the fake daemon, which it may still need
// to finish its work.
async function stopStack(stack) {
  for (const child of stack.children.toReversed()) {
    child.stopping = true;
    child.kill("SIGTERM");
    await new Promise((done) => (exited(child) ? done() : child.once("exit", done)));
  }
}

/**
 * Starts a stack that no other journey has used, on a port of its own, and
 * returns its env, restart(), which restarts its Ludock alone, and a
 * function that stops it and removes its data.
 */
export async function freshStack(index, options) {
  const dir = mkdtempSync(join(scratch(), "ludock-e2e-fresh-"));
  const children = [];
  const stop = async () => {
    await stopStack({ children });
    if (!process.env.LUDOCK_E2E_KEEP) rmSync(dir, { recursive: true, force: true });
  };
  // A hook that times out abandons this call, so the worker's exit stops
  // whatever it started.
  process.once("exit", () => children.forEach((child) => child.kill("SIGTERM")));
  try {
    const { env, restart } = await startStack(process.env.LUDOCK_E2E_BIN, dir, port && port + 50 + index, { ...options, children });
    return { env, restart, stop };
  } catch (error) {
    await stop();
    throw error;
  }
}

export default async function setup(config) {
  const dir = mkdtempSync(join(scratch(), "ludock-e2e-"));
  const bin = join(dir, "bin");
  const env = { ...process.env, CGO_ENABLED: "0" };
  execFileSync("go", ["build", "-o", bin + "/", "./cmd/ludock", "./cmd/fakedocker"], { cwd: root, env, stdio: "inherit" });

  const started = Array.from({ length: config.workers }, () => ({ children: [] }));
  const teardown = async () => {
    await Promise.all(started.map(stopStack));
    if (!process.env.LUDOCK_E2E_KEEP) rmSync(dir, { recursive: true, force: true });
  };
  const results = await Promise.allSettled(started.map((stack, index) => {
    const stackDir = join(dir, String(index));
    mkdirSync(stackDir);
    return startStack(bin, stackDir, port && port + index, { children: stack.children });
  }));
  // A stack that couldn't start would leave the others running.
  const failed = results.find((result) => result.status === "rejected");
  if (failed) {
    await teardown();
    throw failed.reason;
  }
  // fixtures.mjs picks the stack for its worker's parallel index.
  process.env.LUDOCK_E2E_STACKS = JSON.stringify(results.map((result) => result.value.env));
  process.env.LUDOCK_E2E_BIN = bin;
  return teardown;
}
