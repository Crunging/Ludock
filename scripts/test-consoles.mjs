#!/usr/bin/env bun
// Run only against a dedicated test daemon with disposable game data.
import { expect } from "bun:test";
import { mkdtemp, realpath, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { createTestDocker, cleanupFixtures, waitForReady } from "./test-docker.mjs";

const factorioImage = "factoriotools/factorio:stable@sha256:2552b97f548f999217bdc9a4e1cc2d4e8c638e9272452e58c978910bc3c01074";
const project = `ludock-console-${crypto.randomUUID().slice(0, 8)}`;
const token = `test-only-${crypto.randomUUID()}`;
const password = `console-only-${crypto.randomUUID()}`;
const { docker, image, socketArguments } = createTestDocker({ secrets: [token, password] });
const root = await realpath(await mkdtemp(path.join(process.env.LUDOCK_TEST_DIRECTORY || tmpdir(), `${project}-`)));
const gamesFile = path.join(root, "games.yaml");
const appFile = path.join(root, "app.yaml");
const compose = (file, suffix, ...args) => docker("compose", "--project-name", `${project}-${suffix}`, "-f", file, ...args);
const games = (...args) => compose(gamesFile, "games", ...args);
const app = (...args) => compose(appFile, "app", ...args);
const inspect = id => JSON.parse(docker("inspect", id))[0];
const helpers = () => docker("ps", "-aq", "--filter", "label=ludock.internal=console-helper").split(/\s+/).filter(Boolean);
const sockets = new Set();
let base;

const fixture = `
if (process.env.MODE === "rust") {
  Bun.serve({ hostname: "127.0.0.1", port: 28016,
    fetch(request, server) {
      if (new URL(request.url).pathname !== "/" + encodeURIComponent(process.env.RCON_PASSWORD)) return new Response(null, {status: 401});
      return server.upgrade(request) ? undefined : new Response(null, {status: 400});
    },
    websocket: { message(socket, raw) {
      const request = JSON.parse(String(raw));
      if (request.Message === "stall") return;
      socket.send(JSON.stringify({Identifier: request.Identifier, Message: "WebRCON ready " + process.env.RCON_PASSWORD}));
    }}
  });
} else {
  Bun.listen({ hostname: "127.0.0.1", port: 8081,
    socket: {
      open(socket) { socket.write("Password: "); },
      data(socket, raw) {
        const line = String(raw).trim();
        if (line === process.env.TELNET_PASSWORD) socket.write("Logged in\\n");
        else if (line !== "stall") socket.write("Telnet ready " + process.env.TELNET_PASSWORD + "\\n");
      }
    }
  });
}
`;

await Bun.write(gamesFile, JSON.stringify({ services: {
  factorio: {
    image: factorioImage,
    environment: { RCON_PASSWORD: password, DLC_SPACE_AGE: "false" },
    entrypoint: ["/bin/bash", "-c"],
    command: [String.raw`
set -eu
mkdir -p /factorio/config
printf '%s\n' "$$RCON_PASSWORD" > /factorio/config/rconpw
jq '.visibility.public=false | .visibility.lan=false | .require_user_verification=false' /opt/factorio/data/server-settings.example.json > /factorio/config/server-settings.json
sed -i 's/--rcon-port "[^"]*"/--rcon-bind 127.0.0.1:27015/' /docker-entrypoint.sh
exec /docker-entrypoint.sh
`],
    labels: { "ludock.enable": "true", "ludock.name": "Console Factorio" },
  },
  rust: {
    image, entrypoint: ["bun", "-e"], command: [fixture],
    environment: { MODE: "rust", RCON_PASSWORD: password },
    labels: { "ludock.enable": "true", "ludock.name": "Console Rust", "ludock.console": "rust-webrcon" },
  },
  telnet: {
    image, entrypoint: ["bun", "-e"], command: [fixture],
    environment: { MODE: "telnet", TELNET_PASSWORD: password },
    labels: { "ludock.enable": "true", "ludock.name": "Console Telnet", "ludock.console": "telnet-console", "ludock.console.port": "8081" },
  },
} }));
await Bun.write(appFile, JSON.stringify({ services: { app: {
  image,
  read_only: true,
  cap_drop: ["ALL"], cap_add: ["DAC_OVERRIDE"], security_opt: ["no-new-privileges"],
  tmpfs: ["/tmp:rw,nosuid,nodev,noexec,size=256m,mode=1777"],
  environment: { LUDOCK_API_TOKEN: token },
  labels: { "ludock.enable": "false" },
  // Reuse the harness's checked daemon-visible socket mapping.
  volumes: [socketArguments[1].replace("type=bind,source=", "").replace(",target=", ":").replace(",readonly", ":ro")],
  ports: ["127.0.0.1::3000"],
} } }));

async function request(endpoint) {
  const response = await fetch(`${base}/api/v1${endpoint}`, {
    headers: { Authorization: `Bearer ${token}` }, signal: AbortSignal.timeout(10_000),
  });
  if (!response.ok) throw new Error(`Fixture API returned ${response.status}`);
  return response.json();
}

async function consoleSocket(serverId) {
  const socket = new WebSocket(`${base.replace(/^http/, "ws")}/ws/v1/game-console/${serverId}`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  sockets.add(socket);
  const messages = [];
  socket.addEventListener("message", event => messages.push(JSON.parse(String(event.data))));
  await waitForReady(() => {
    expect(messages.some(message => message.type === "system" && message.data.startsWith("Connected to"))).toBe(true);
  }, "Console must open");
  return { socket, messages, send(command) { socket.send(JSON.stringify({ type: "input", data: command })); } };
}

try {
  expect(helpers()).toEqual([]);
  docker("pull", factorioImage);
  games("up", "-d");
  app("up", "-d");
  const appId = app("ps", "-q", "app");
  const appInfo = inspect(appId);
  const hostname = process.env.LUDOCK_TEST_HOST || "127.0.0.1";
  base = `http://${hostname}:${appInfo.NetworkSettings.Ports["3000/tcp"][0].HostPort}`;
  // A runner in the daemon host's namespace can reach its loopback publishing.
  await waitForReady(() => request("/health"), "Fixture API must become healthy");
  const { servers } = await request("/servers");
  const find = name => servers.find(server => server.displayName === name).id;
  for (const service of ["factorio", "rust", "telnet"]) {
    const info = inspect(games("ps", "-q", service));
    expect(Object.keys(info.NetworkSettings.Networks).some(network => network in appInfo.NetworkSettings.Networks)).toBe(false);
    expect(Object.values(info.NetworkSettings.Ports).every(binding => binding === null)).toBe(true);
  }
  // The Factorio image uses Box64 on ARM64. Allow time to generate its disposable world.
  await waitForReady(() => {
    const id = games("ps", "-a", "-q", "factorio");
    expect(inspect(id).State.Running, "Factorio must stay running").toBe(true);
    expect(docker("logs", id)).toContain("Starting RCON interface");
  }, "Factorio must start its real loopback RCON endpoint", 180_000);
  const factorio = await consoleSocket(find("Console Factorio"));
  factorio.send("/players online");
  await waitForReady(() => {
    expect(factorio.messages.some(message => message.type === "stdout" && /online players/i.test(message.data))).toBe(true);
  }, "Real Factorio RCON command must work across separate Compose networks");

  for (const [name, expected] of [["Console Rust", "WebRCON ready [redacted]"], ["Console Telnet", "Telnet ready [redacted]"]]) {
    const connection = await consoleSocket(find(name));
    connection.send("status");
    await waitForReady(() => {
      expect(connection.messages.some(message => message.type === "stdout" && message.data.includes(expected))).toBe(true);
    }, `${name} must return redacted loopback output`);
    expect(JSON.stringify(connection.messages)).not.toContain(password);
  }
  await waitForReady(() => expect(helpers()).toEqual([]), "Successful helpers must be removed");

  for (const disconnect of [false, true]) {
    const connection = await consoleSocket(find("Console Rust"));
    connection.send("stall");
    let helperId;
    await waitForReady(() => {
      helperId = helpers()[0];
      expect(helperId).toBeTruthy();
      const info = inspect(helperId);
      expect(info.HostConfig.NetworkMode).toBe(`container:${games("ps", "-q", "rust")}`);
      expect(JSON.stringify(info.Config)).not.toContain(password);
    }, "Stalled command must use a credential-free namespace helper");
    if (disconnect) connection.socket.close();
    else await waitForReady(() => {
      expect(connection.messages.some(message => message.type === "error")).toBe(true);
    }, "Stalled command must time out", 15_000);
    await waitForReady(() => expect(helpers()).toEqual([]), "Timeout/disconnect must remove the helper");
  }
  expect(JSON.stringify(factorio.messages)).not.toContain(password);
  console.log(JSON.stringify({ result: "pass", realFactorio: true, separateComposeNetworks: true,
    loopbackOnly: true, webRcon: true, telnet: true, redacted: true, stdinCredentials: true,
    timeoutCleanup: true, disconnectCleanup: true,
    architecture: docker("info", "--format", "{{.Architecture}}"),
    factorioArm64Emulation: "Box64 when running the ARM64 image",
  }));
} finally {
  for (const socket of sockets) socket.terminate();
  await cleanupFixtures([
    () => app("down", "--volumes", "--remove-orphans"),
    () => games("down", "--volumes", "--remove-orphans"),
    ...helpers().map(id => () => docker("rm", "-fv", id)),
    () => rm(root, { recursive: true, force: true }),
  ]);
}
