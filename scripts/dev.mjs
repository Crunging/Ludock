#!/usr/bin/env bun
// Run the backend (restarted on change) and the frontend (hot reloaded).
// Docker stays disconnected unless DOCKER_SOCKET names a dedicated test daemon.
import path from "node:path";

const root = path.resolve(import.meta.dir, "..");
const state = path.join(root, "data/dev");
const apiPort = process.env.PORT || "3001";
const env = {
  ...process.env,
  NODE_ENV: "development",
  HOST: "127.0.0.1",
  PORT: apiPort,
  LUDOCK_DB_PATH: process.env.LUDOCK_DB_PATH || path.join(state, "ludock.db"),
  DOCKER_SOCKET: process.env.DOCKER_SOCKET || path.join(state, "docker-disconnected.sock"),
  LUDOCK_DEV_API_ORIGIN: `http://127.0.0.1:${apiPort}`,
};

console.log(`Database: ${env.LUDOCK_DB_PATH}`);
console.log(`Docker: ${process.env.DOCKER_SOCKET || "disconnected (set DOCKER_SOCKET to a test daemon)"}`);

const children = [
  Bun.spawn([process.execPath, "--watch", "src/index.ts"], {
    cwd: path.join(root, "packages/backend"), env, stdio: ["ignore", "inherit", "inherit"],
  }),
  Bun.spawn([process.execPath, "scripts/dev.ts"], {
    cwd: path.join(root, "packages/frontend"), env, stdio: ["ignore", "inherit", "inherit"],
  }),
];
const stop = () => { for (const child of children) child.kill("SIGTERM"); };
process.on("SIGINT", stop);
process.on("SIGTERM", stop);
// If either process exits, stop the other.
await Promise.race(children.map((child) => child.exited));
stop();
const codes = await Promise.all(children.map((child) => child.exited));
process.exitCode = codes.find((code) => code !== 0 && code !== null) ?? 0;
