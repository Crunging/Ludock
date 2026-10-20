// Deploys the image under test with compose.yaml plus a test override, starts
// a small Compose game stack, and completes first-administrator setup.
import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, realpathSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { admin } from "./journeys/fixtures.mjs";

const root = resolve(import.meta.dirname, "..");
const image = process.env.LUDOCK_TEST_IMAGE || "ludock:test";
// A busybox-based game stand-in: it stays up, writes its data under /data,
// and prints a line every few seconds.
export const gameImage = "alpine:3@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6";
const setupCode = "docker-e2e-setup-code-0123456789abcdef";

export function docker(...args) {
  return execFileSync("docker", args, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
}

async function waitFor(check, what, timeout = 90_000) {
  const deadline = Date.now() + timeout;
  for (;;) {
    try {
      if (await check()) return;
    } catch {
      // Not ready yet.
    }
    if (Date.now() > deadline) throw new Error(`Timed out waiting for ${what}`);
    await new Promise((done) => setTimeout(done, 500));
  }
}

export default async function setup() {
  if (process.env.LUDOCK_DOCKER_E2E !== "disposable-daemon") {
    throw new Error("Set LUDOCK_DOCKER_E2E=disposable-daemon to confirm this Docker daemon runs no real game servers.");
  }
  // Compose sources must have the same absolute path inside Ludock.
  const dir = realpathSync(mkdtempSync(join(process.env.RUNNER_TEMP || tmpdir(), "ludock-docker-e2e-")));
  const stack = join(dir, "stacks", "game");
  mkdirSync(join(stack, "data"), { recursive: true });
  writeFileSync(join(stack, "data", "server.properties"), "motd=Hello from the container test\n");
  writeFileSync(join(stack, "compose.yaml"), `services:
  game:
    image: ${gameImage}
    command: ["sh", "-c", "mkdir -p /data/world; while true; do echo tick $$(date +%s); sleep 5; done"]
    labels:
      ludock.enable: "true"
      ludock.name: Test Game
      ludock.files: /data
    volumes:
      - ./data:/data
    stop_signal: SIGKILL
`);
  const override = join(dir, "compose.override.yaml");
  writeFileSync(override, `services:
  ludock:
    image: ${image}
    pull_policy: never
    environment:
      LUDOCK_SETUP_CODE: ${setupCode}
      LUDOCK_COMPOSE_ROOTS: ${join(dir, "stacks")}
    volumes:
      - ${join(dir, "stacks")}:${join(dir, "stacks")}:ro
`);
  const ludock = ["compose", "--project-name", "ludock-e2e", "-f", join(root, "compose.yaml"), "-f", override];
  const game = ["compose", "--project-name", "game", "--project-directory", stack, "-f", join(stack, "compose.yaml")];
  docker(...game, "up", "-d");
  docker(...ludock, "up", "-d");
  process.env.LUDOCK_DOCKER_LUDOCK = JSON.stringify(ludock);
  process.env.LUDOCK_DOCKER_GAME = JSON.stringify(game);
  process.env.LUDOCK_DOCKER_STACK = stack;

  await waitFor(async () => (await fetch("http://127.0.0.1:3000/api/v1/health")).ok, "Ludock's health check");
  const response = await fetch("http://127.0.0.1:3000/setup", {
    method: "POST", redirect: "manual",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({ bootstrapCode: setupCode, username: admin.username, password: admin.password, confirmation: admin.password }),
  });
  if (response.status !== 303) throw new Error(`Setup failed with ${response.status}`);
  const token = response.headers.getSetCookie().find((value) => value.startsWith("ludock_session=")).split(";")[0].split("=")[1];
  const state = join(dir, "admin.json");
  writeFileSync(state, JSON.stringify({ cookies: [{ name: "ludock_session", value: token, domain: "127.0.0.1", path: "/",
    expires: -1, httpOnly: true, secure: false, sameSite: "Strict" }], origins: [] }));
  process.env.LUDOCK_E2E_ADMIN_STATE = state;

  return async () => {
    if (process.env.LUDOCK_E2E_LOGS) console.log(docker(...ludock, "logs", "ludock"));
    for (const project of [ludock, game]) {
      try {
        docker(...project, "down", "--volumes", "--remove-orphans", "--timeout", "5");
      } catch (error) {
        console.error(String(error));
      }
    }
    rmSync(dir, { recursive: true, force: true });
  };
}
