// A manual rig: Ludock from the local image beside one real game server, as
// an owner would run them, with a headless Minecraft player. It runs only
// against a disposable Docker daemon and never in CI. See docs/DEVELOPMENT.md.
//
//   node real/rig.mjs up [GAME]    start Ludock and a game from real/games.mjs (minecraft)
//   node real/rig.mjs remove GAME  remove that game's containers, volumes, network, and data
//   node real/rig.mjs bot NAME     join the Minecraft server as NAME and log what the player sees
//   node real/rig.mjs down         remove every container, volume, and network the rig made
import { execFileSync, spawn } from "node:child_process";
import { existsSync, mkdirSync, readdirSync, rmdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { admin } from "../journeys/fixtures.mjs";
import { games } from "./games.mjs";

const root = resolve(import.meta.dirname, "../..");
const dir = resolve(process.env.LUDOCK_RIG_DIR || join(tmpdir(), "ludock-real-rig"));
const image = process.env.LUDOCK_TEST_IMAGE || "ludock:test";
const setupCode = "real-rig-setup-code-0123456789abcdef0123";
const marker = ".ludock-real-rig";
const ludock = ["compose", "--project-name", "ludocktest-ludock", "-f", join(root, "compose.yaml"), "-f", join(dir, "ludock.override.yaml")];
const project = (game) => ["compose", "--project-name", `ludocktest-${game}`, "--project-directory", join(dir, game),
  "-f", join(dir, game, "compose.yaml")];
// The games up started, which down and remove take apart.
const started = () => Object.keys(games).filter((game) => existsSync(join(dir, game, "compose.yaml")));

// Games print a lot while they start, such as world generation progress.
const docker = (...args) => execFileSync("docker", args,
  { encoding: "utf8", stdio: ["ignore", "pipe", "inherit"], maxBuffer: 1 << 28 }).trim();

async function waitFor(check, what, timeout) {
  const deadline = Date.now() + timeout;
  while (!(await check().catch(() => false))) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise((done) => setTimeout(done, 2000));
  }
}

function confirmDaemon() {
  if (process.env.LUDOCK_DOCKER_E2E !== "disposable-daemon") {
    throw new Error("Set LUDOCK_DOCKER_E2E=disposable-daemon to confirm this Docker daemon runs no real game servers.");
  }
}

function gameNamed(name) {
  const game = games[name];
  if (!game) throw new Error(`unknown game ${name}; real/games.mjs has ${Object.keys(games).join(", ")}`);
  return game;
}

async function up() {
  confirmDaemon();
  const name = process.argv[3] || "minecraft";
  const game = gameNamed(name);
  if (game.licence && process.env[game.licence.env] !== "TRUE") {
    throw new Error(`Set ${game.licence.env}=TRUE to accept ${game.licence.terms} for the test servers.`);
  }
  if (existsSync(dir) && !existsSync(join(dir, marker)) && readdirSync(dir).length) {
    throw new Error(`${dir} isn't empty; set LUDOCK_RIG_DIR to a new folder.`);
  }
  const others = started().filter((other) => other !== name);
  if (others.length) throw new Error(`${others.join(", ")} is up; run node real/rig.mjs remove ${others[0]} first.`);
  mkdirSync(join(dir, name, "data"), { recursive: true });
  // down removes only a folder that up made.
  writeFileSync(join(dir, marker), "");
  // Every port is published on loopback only.
  writeFileSync(join(dir, "ludock.override.yaml"), `services:
  ludock:
    image: ${image}
    pull_policy: never
    ports: !override
      - "127.0.0.1:3000:3000"
    environment:
      LUDOCK_SETUP_CODE: ${setupCode}
    volumes:
      # A named volume, since the rig folder may be kept in memory.
      - backups-copy:/backups-copy
volumes:
  backups-copy:
`);
  const service = { restart: "unless-stopped", ...game.service,
    ports: game.service.ports.map((port) => `127.0.0.1:${port}`),
    volumes: [...(game.data ? [`./data:${game.data}`] : []), ...(game.service.volumes || [])] };
  // Named volumes, such as a game's own install, live on Docker's disk; the
  // scratch folder may be kept in memory.
  const named = service.volumes.map((volume) => volume.split(":")[0]).filter((source) => !/^[./]/.test(source));
  // Compose reads JSON as YAML.
  writeFileSync(join(dir, name, "compose.yaml"), JSON.stringify({ services: { [name]: service },
    volumes: Object.fromEntries(named.map((volume) => [volume, {}])) }, null, 2));
  docker(...project(name), "up", "-d");
  docker(...ludock, "up", "-d");

  await waitFor(async () => (await fetch("http://127.0.0.1:3000/api/v1/health")).ok, "Ludock", 90_000);
  const response = await fetch("http://127.0.0.1:3000/setup", {
    method: "POST", redirect: "manual",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({ bootstrapCode: setupCode, username: admin.username, password: admin.password, confirmation: admin.password }),
  });
  if (response.status !== 303) console.log("Setup was already done, or failed with", response.status);
  const container = docker(...project(name), "ps", "--quiet", name);
  const ready = game.ready === "healthy"
    ? async () => docker("inspect", "--format", "{{.State.Health.Status}}", container) === "healthy"
    : async () => game.ready.test(docker("logs", container));
  await waitFor(ready, `${name} to start`, game.timeout || 900_000);
  console.log(`Ludock: http://127.0.0.1:3000 (${admin.username} / ${admin.password})
${name}: ${service.ports.join(", ")}, container ${container.slice(0, 12)}, data in ${join(dir, name, "data")}`);
}

// removeHelpers removes the file, backup, and console helpers the rig's
// Ludock started from its image, which may still hold a game's volumes.
function removeHelpers() {
  const helpers = docker("ps", "--all", "--quiet", "--filter", "label=ludock.internal", "--filter", `ancestor=${image}`);
  if (helpers) docker("rm", "--force", "--volumes", ...helpers.split("\n"));
}

// remove takes one game apart, so another can start beside the same Ludock.
function remove() {
  confirmDaemon();
  const name = process.argv[3];
  gameNamed(name);
  if (!existsSync(join(dir, marker)) || !started().includes(name)) throw new Error(`${name} isn't up in ${dir}.`);
  removeHelpers();
  // A failed teardown stops here, before any game data is removed.
  docker(...project(name), "down", "--volumes", "--remove-orphans", "--timeout", "60");
  clear([name]);
}

// clear removes folders the rig made. Game data written by the containers
// belongs to their users, so a container removes it.
function clear(names) {
  const made = names.filter((name) => existsSync(join(dir, name)));
  if (made.length) docker("run", "--rm", "--network", "none", "--volume", `${dir}:/rig`, "alpine:3", "rm", "-rf",
    ...made.map((name) => `/rig/${name}`));
}

async function bot() {
  const name = process.argv[3] || "RigBot";
  const modules = join(dir, "bot");
  if (!existsSync(join(modules, "node_modules", "mineflayer"))) {
    mkdirSync(modules, { recursive: true });
    execFileSync("npm", ["install", "--prefix", modules, "--no-save", "--silent", "mineflayer@4.39.0"], { stdio: "inherit" });
  }
  const child = spawn(process.execPath, [join(import.meta.dirname, "bot.mjs"), name, games.minecraft.service.environment.VERSION],
    { stdio: "inherit", env: { ...process.env, NODE_PATH: join(modules, "node_modules") } });
  child.on("exit", (code) => process.exit(code ?? 1));
}

function down() {
  confirmDaemon();
  if (!existsSync(join(dir, marker))) throw new Error(`${dir} wasn't made by real/rig.mjs up; leaving it alone.`);
  // A failed teardown stops here, before any game data is removed.
  docker(...ludock, "down", "--volumes", "--remove-orphans", "--timeout", "30");
  removeHelpers();
  for (const name of started()) docker(...project(name), "down", "--volumes", "--remove-orphans", "--timeout", "60");
  clear([...Object.keys(games), "bot", "ludock.override.yaml"]);
  rmSync(join(dir, marker));
  try {
    rmdirSync(dir);
  } catch {
    // Not empty: it holds files the rig didn't make.
  }
  console.log("Removed the rig. Images stay; remove them with docker image rm if you pulled them for it.");
}

const commands = { up, remove, bot, down };
const command = commands[process.argv[2]];
if (!command) {
  console.error("usage: node real/rig.mjs up [GAME] | remove GAME | bot [NAME] | down");
  process.exit(2);
}
await command();
