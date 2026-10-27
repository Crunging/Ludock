// Builds Ludock and the fake Docker daemon, then starts one of each on fresh
// data for every worker, completes first-administrator setup, and saves the
// administrator's signed-in state.
import { spawn, execFileSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { port } from "./playwright.config.mjs";

const root = resolve(import.meta.dirname, "..");
export const admin = { username: "admin", password: "correct horse battery staple" };
const setupCode = "e2e-setup-code-0123456789abcdef-0123";

async function waitFor(check, what, timeout = 30_000) {
  const deadline = Date.now() + timeout;
  for (;;) {
    try {
      if (await check()) return;
    } catch {
      // Not ready yet.
    }
    if (Date.now() > deadline) throw new Error(`Timed out waiting for ${what}`);
    await new Promise((done) => setTimeout(done, 100));
  }
}

function start(command, args, env, log) {
  const child = spawn(command, args, { env: { ...process.env, ...env }, stdio: ["ignore", "pipe", "pipe"] });
  const lines = [];
  for (const stream of [child.stdout, child.stderr]) {
    stream.on("data", (chunk) => {
      lines.push(chunk.toString());
      if (process.env.LUDOCK_E2E_LOGS) process.stdout.write(`[${log}] ${chunk}`);
    });
  }
  child.on("exit", (code) => {
    if (code && !child.stopping) console.error(`${log} exited with ${code}:\n${lines.join("")}`);
  });
  return child;
}

// startStack starts one Ludock and fake daemon on fresh data under dir and
// signs in the setup administrator. Each worker gets its own stack.
async function startStack(bin, dir, port) {
  const dockerDir = join(dir, "docker");
  const backups = join(dir, "backups");
  const fake = start(join(bin, "fakedocker"), ["-dir", dockerDir, "-backups", backups], {}, "fakedocker");
  const socket = join(dockerDir, "docker.sock");
  await waitFor(() => existsSync(socket), "the fake Docker socket");

  const baseURL = `http://127.0.0.1:${port}`;
  const ludock = start(join(bin, "ludock"), ["serve"], {
    LUDOCK_LISTEN: `127.0.0.1:${port}`,
    LUDOCK_DATA_DIR: join(dir, "data"),
    LUDOCK_DOCKER_SOCKET: socket,
    LUDOCK_SETUP_CODE: setupCode,
    LUDOCK_SELF_CONTAINER: "ludock-dev",
    LUDOCK_BACKUP_ROOTS: backups,
    LUDOCK_HELPER_IMAGE: "ghcr.io/crunging/ludock@sha256:" + "ab".repeat(32),
    LUDOCK_MAX_UPLOAD_SIZE: "5 MiB",
    LUDOCK_LOG_LEVEL: "info",
  }, "ludock");
  await waitFor(async () => (await fetch(`${baseURL}/api/v1/health`)).ok, "Ludock's health check");

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
  return { children: [ludock, fake], env: { LUDOCK_E2E_URL: baseURL, LUDOCK_E2E_ADMIN_STATE: state, LUDOCK_E2E_DIR: dir } };
}

export default async function setup(config) {
  const dir = mkdtempSync(join(tmpdir(), "ludock-e2e-"));
  const bin = join(dir, "bin");
  const env = { ...process.env, CGO_ENABLED: "0" };
  execFileSync("go", ["build", "-o", bin + "/", "./cmd/ludock", "./cmd/fakedocker"], { cwd: root, env, stdio: "inherit" });

  const stacks = await Promise.all(Array.from({ length: config.workers }, (_, index) => {
    const stackDir = join(dir, String(index));
    mkdirSync(stackDir);
    return startStack(bin, stackDir, port + index);
  }));
  // fixtures.mjs picks the stack for its worker's parallel index.
  process.env.LUDOCK_E2E_STACKS = JSON.stringify(stacks.map((stack) => stack.env));

  return async () => {
    await Promise.all(stacks.map(async (stack) => {
      for (const child of stack.children) {
        child.stopping = true;
        child.kill("SIGTERM");
        await new Promise((done) => (child.exitCode !== null ? done() : child.once("exit", done)));
      }
    }));
    if (!process.env.LUDOCK_E2E_KEEP) rmSync(dir, { recursive: true, force: true });
  };
}
