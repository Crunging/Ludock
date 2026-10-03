#!/usr/bin/env bun
// Run the backend (restarted on change) and the frontend (hot reloaded).
// Docker stays disconnected unless DOCKER_SOCKET names a dedicated test daemon.
import path from "node:path";

const root = path.resolve(import.meta.dir, "..");
export function developmentEnvironment(environment = process.env) {
  const state = path.join(root, "data/dev");
  const apiPort = environment.PORT || "3001";
  return {
    ...environment,
    NODE_ENV: "development",
    HOST: "127.0.0.1",
    PORT: apiPort,
    LUDOCK_DB_PATH: environment.LUDOCK_DB_PATH || path.join(state, "ludock.db"),
    DOCKER_SOCKET: environment.DOCKER_SOCKET || path.join(state, "docker-disconnected.sock"),
    LUDOCK_DEV_API_ORIGIN: environment.LUDOCK_DEV_API_ORIGIN || `http://127.0.0.1:${apiPort}`,
  };
}

if (import.meta.main) {
  const args = process.argv.slice(2);
  if (args.length > 1 || (args.length && !["--backend", "--frontend"].includes(args[0]))) {
    throw new Error("Usage: bun scripts/dev.mjs [--backend|--frontend]");
  }
  const env = developmentEnvironment();
  console.log(process.env.LUDOCK_DB_PATH ? "Database: custom development path" : "Database: data/dev/ludock.db");
  console.log(process.env.DOCKER_SOCKET
    ? "Docker: explicit socket configured"
    : "Docker: disconnected (set DOCKER_SOCKET to a test daemon)");

  const commands = [
    ["backend", [process.execPath, "--watch", "src/index.ts"]],
    ["frontend", [process.execPath, "scripts/dev.ts"]],
  ];
  const children = commands.filter(([name]) => !args.length || args[0] === `--${name}`)
    .map(([name, command]) => Bun.spawn(command, {
      cwd: path.join(root, "packages", name), env, stdio: ["ignore", "inherit", "inherit"],
    }));
  const stop = () => { for (const child of children) child.kill("SIGTERM"); };
  process.on("SIGINT", stop);
  process.on("SIGTERM", stop);
  // If either process exits, stop the other.
  await Promise.race(children.map((child) => child.exited));
  stop();
  const codes = await Promise.all(children.map((child) => child.exited));
  process.exitCode = codes.find((code) => code !== 0 && code !== null) ?? 0;
}
