// A manual rig: Ludock from the local image beside a real Minecraft server,
// with a headless player. It runs only against a disposable Docker daemon and
// never in CI. See docs/DEVELOPMENT.md.
//
//   node real/rig.mjs up        start Ludock and Minecraft Java
//   node real/rig.mjs bot NAME  join the Java server as NAME and log what the player sees
//   node real/rig.mjs down      remove every container, volume, and network the rig made
import { execFileSync, spawn } from "node:child_process";
import { existsSync, mkdirSync, readdirSync, rmdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { admin } from "../journeys/fixtures.mjs";

const root = resolve(import.meta.dirname, "../..");
const dir = resolve(process.env.LUDOCK_RIG_DIR || join(tmpdir(), "ludock-real-rig"));
const image = process.env.LUDOCK_TEST_IMAGE || "ludock:test";
const javaImage = process.env.LUDOCK_RIG_JAVA_IMAGE || "itzg/minecraft-server:java21";
// Mineflayer supports this release; the bot can't join a newer one.
const version = process.env.LUDOCK_RIG_MINECRAFT_VERSION || "1.21.11";
const setupCode = "real-rig-setup-code-0123456789abcdef0123";
const marker = ".ludock-real-rig";
const projects = {
  ludock: ["compose", "--project-name", "ludocktest-ludock", "-f", join(root, "compose.yaml"), "-f", join(dir, "ludock.override.yaml")],
  java: ["compose", "--project-name", "ludocktest-minecraft", "--project-directory", join(dir, "java"), "-f", join(dir, "java", "compose.yaml")],
};

const docker = (...args) => execFileSync("docker", args, { encoding: "utf8", stdio: ["ignore", "pipe", "inherit"] }).trim();

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

async function up() {
  confirmDaemon();
  if (process.env.MINECRAFT_EULA !== "TRUE") {
    throw new Error("Set MINECRAFT_EULA=TRUE to accept the Minecraft EULA (https://aka.ms/MinecraftEULA) for the test servers.");
  }
  if (existsSync(dir) && !existsSync(join(dir, marker)) && readdirSync(dir).length) {
    throw new Error(`${dir} isn't empty; set LUDOCK_RIG_DIR to a new folder.`);
  }
  for (const folder of ["java/data", "backups-copy"]) mkdirSync(join(dir, folder), { recursive: true });
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
      - ${join(dir, "backups-copy")}:/backups-copy
`);
  writeFileSync(join(dir, "java", "compose.yaml"), `services:
  minecraft:
    image: ${javaImage}
    environment:
      EULA: "TRUE"
      VERSION: "${version}"
      MEMORY: 1G
      VIEW_DISTANCE: "4"
      SIMULATION_DISTANCE: "4"
      ONLINE_MODE: "FALSE"
      MOTD: Ludock real rig
    mem_limit: 2g
    ports:
      - "127.0.0.1:25565:25565"
    volumes:
      - ./data:/data
    restart: unless-stopped
`);
  docker(...projects.java, "up", "-d");
  docker(...projects.ludock, "up", "-d");

  await waitFor(async () => (await fetch("http://127.0.0.1:3000/api/v1/health")).ok, "Ludock", 90_000);
  const response = await fetch("http://127.0.0.1:3000/setup", {
    method: "POST", redirect: "manual",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({ bootstrapCode: setupCode, username: admin.username, password: admin.password, confirmation: admin.password }),
  });
  if (response.status !== 303) console.log("Setup was already done, or failed with", response.status);
  const container = docker(...projects.java, "ps", "--quiet", "minecraft");
  await waitFor(async () => docker("inspect", "--format", "{{.State.Health.Status}}", container) === "healthy", "Minecraft to start", 600_000);
  console.log(`Ludock: http://127.0.0.1:3000 (${admin.username} / ${admin.password})
Minecraft Java: 127.0.0.1:25565, version ${version}, offline mode
Rig folder: ${dir}`);
}

async function bot() {
  const name = process.argv[3] || "RigBot";
  const modules = join(dir, "bot");
  if (!existsSync(join(modules, "node_modules", "mineflayer"))) {
    mkdirSync(modules, { recursive: true });
    execFileSync("npm", ["install", "--prefix", modules, "--no-save", "--silent", "mineflayer@4.39.0"], { stdio: "inherit" });
  }
  const child = spawn(process.execPath, [join(import.meta.dirname, "bot.mjs"), name, version],
    { stdio: "inherit", env: { ...process.env, NODE_PATH: join(modules, "node_modules") } });
  child.on("exit", (code) => process.exit(code ?? 1));
}

function down() {
  confirmDaemon();
  if (!existsSync(join(dir, marker))) throw new Error(`${dir} wasn't made by real/rig.mjs up; leaving it alone.`);
  // A failed teardown stops here, before any game data is removed.
  for (const project of Object.values(projects)) docker(...project, "down", "--volumes", "--remove-orphans", "--timeout", "30");
  // Game data written by the containers belongs to their users, so a
  // container removes what the rig made, and nothing else in its folder.
  const made = ["java", "backups-copy", "bot", "ludock.override.yaml"].filter((name) => existsSync(join(dir, name)));
  if (made.length) docker("run", "--rm", "--volume", `${dir}:/rig`, "alpine:3", "rm", "-rf", ...made.map((name) => `/rig/${name}`));
  rmSync(join(dir, marker));
  try {
    rmdirSync(dir);
  } catch {
    // Not empty: it holds files the rig didn't make.
  }
  console.log("Removed the rig. Images stay; remove them with docker image rm if you pulled them for it.");
}

const commands = { up, bot, down };
const command = commands[process.argv[2]];
if (!command) {
  console.error("usage: node real/rig.mjs up | bot [NAME] | down");
  process.exit(2);
}
await command();
