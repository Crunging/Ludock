// Upgrades an install of the last release to this checkout's build, without
// Docker: builds the release tag (default v0.6.0) and this checkout, runs the
// old Ludock against the fake daemon long enough to set up an administrator,
// a member with a shared server, a schedule, a backup, monitoring, and a
// notification destination, stops it, starts the new Ludock on the same data,
// and checks each of those is still there and works. It then starts the new
// Ludock on a database no release can read and checks that it says so once
// and stays up instead of exiting into a restart loop.
//
// Run from e2e/: node upgrade.mjs [tag]. It is not part of the journeys,
// since it builds an old tag. LUDOCK_E2E_LOGS=1 prints both servers' logs.
import { spawn, execFileSync } from "node:child_process";
import { copyFileSync, existsSync, mkdirSync, mkdtempSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { DatabaseSync } from "node:sqlite";

const root = resolve(import.meta.dirname, "..");
const tag = process.argv[2] || "v0.6.0";
const setupCode = "e2e-setup-code-0123456789abcdef-0123";
const admin = { username: "admin", password: "correct horse battery staple" };
const friend = { username: "friend", password: "another horse battery staple" };
const hook = "https://hooks.example.com/ludock-upgrade";
const dir = mkdtempSync(join(tmpdir(), "ludock-upgrade-"));
const children = [];
const failures = [];

/** Keeps a page a check read, with LUDOCK_E2E_KEEP, to see why it failed. */
function keep(name, text) {
  if (process.env.LUDOCK_E2E_KEEP) writeFileSync(join(dir, name + ".html"), text);
  return text;
}

function check(ok, what) {
  console.log(`${ok ? "ok  " : "FAIL"} ${what}`);
  if (!ok) failures.push(what);
}

async function waitFor(probe, what, timeout = 30_000) {
  for (const deadline = Date.now() + timeout; ;) {
    try {
      const value = await probe();
      if (value) return value;
    } catch {
      // Not ready yet.
    }
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise((done) => setTimeout(done, 200));
  }
}

function start(name, command, args, env = {}) {
  const child = spawn(command, args, { env: { ...process.env, ...env }, stdio: ["ignore", "pipe", "pipe"] });
  child.lines = [];
  for (const stream of [child.stdout, child.stderr]) {
    stream.on("data", (chunk) => {
      child.lines.push(chunk.toString());
      if (process.env.LUDOCK_E2E_LOGS) process.stdout.write(`[${name}] ${chunk}`);
    });
  }
  child.exited = new Promise((done) => child.on("exit", (code) => done(code)));
  children.push(child);
  return child;
}

async function stop(child) {
  if (child.exitCode === null) child.kill("SIGTERM");
  await child.exited;
}

function build(source, bin) {
  execFileSync("go", ["build", "-o", bin + "/", "./cmd/ludock", "./cmd/fakedocker"], { cwd: source, env: { ...process.env, CGO_ENABLED: "0" }, stdio: "inherit" });
}

/** A signed-in browser: a cookie, its CSRF token, and form posts like htmx's. */
class Session {
  constructor(base) {
    this.base = base;
    this.cookie = "";
  }
  async get(path) {
    const response = await fetch(this.base + path, { headers: { Cookie: this.cookie }, redirect: "manual" });
    const text = await response.text();
    const token = text.match(/name="csrf-token" content="([^"]+)"/);
    if (token) this.csrf = token[1];
    return { status: response.status, text };
  }
  async post(path, values, signIn = false) {
    const body = new URLSearchParams(values);
    if (!signIn) body.append("_csrf", this.csrf);
    const response = await fetch(this.base + path, { method: "POST", redirect: "manual", body,
      headers: { "Content-Type": "application/x-www-form-urlencoded", "HX-Request": "true", Cookie: this.cookie } });
    const session = response.headers.getSetCookie().find((value) => value.startsWith("ludock_session="));
    if (session) this.cookie = session.split(";")[0];
    return { status: response.status, headers: response.headers, text: await response.text() };
  }
}

async function ludock(bin, port) {
  const child = start(`ludock ${bin.endsWith("old") ? tag : "new"}`, join(bin, "ludock"), ["serve"], {
    LUDOCK_LISTEN: `127.0.0.1:${port}`,
    LUDOCK_DATA_DIR: join(dir, "data"),
    LUDOCK_DOCKER_SOCKET: join(dir, "docker", "docker.sock"),
    LUDOCK_SETUP_CODE: setupCode,
    LUDOCK_SELF_CONTAINER: "ludock-dev",
    LUDOCK_BACKUP_ROOTS: join(dir, "backups"),
    LUDOCK_HELPER_IMAGE: "ghcr.io/crunging/ludock@sha256:" + "ab".repeat(32),
  });
  const base = `http://127.0.0.1:${port}`;
  await waitFor(async () => child.exitCode === null && (await fetch(base + "/api/v1/health")).ok, "Ludock's health check");
  return { child, base };
}

function serverID(html, name) {
  return html.match(new RegExp(`href="/servers/([^"/?]+)"[^>]*>${name}<`))?.[1];
}

try {
  const old = join(dir, "old"), oldBin = join(dir, "bin-old"), newBin = join(dir, "bin-new");
  mkdirSync(old);
  execFileSync("sh", ["-c", `git -C "${root}" archive "${tag}" | tar -x -C "${old}"`], { stdio: "inherit" });
  build(old, oldBin);
  build(root, newBin);

  // One fake daemon for both, so the servers keep their containers. It is
  // this checkout's, which answers what both builds ask.
  start("fakedocker", join(newBin, "fakedocker"), ["-dir", join(dir, "docker"), "-backups", join(dir, "backups")]);
  await waitFor(() => existsSync(join(dir, "docker", "docker.sock")), "the fake Docker socket");

  // The release: an administrator, and the state an owner builds up.
  const before = await ludock(oldBin, 38411);
  const owner = new Session(before.base);
  const setup = await owner.post("/setup", { bootstrapCode: setupCode, username: admin.username, password: admin.password,
    confirmation: admin.password }, true);
  if (setup.status !== 303) throw new Error(`setup failed with ${setup.status}`);
  const dashboard = await waitFor(async () => {
    const page = await owner.get("/");
    return serverID(page.text, "Survival") && page;
  }, "the release to find Survival");
  const survival = serverID(dashboard.text, "Survival");
  const posted = async (what, path, values) => {
    const response = await owner.post(path, values);
    if (response.status >= 300 || /role="alert"|alert--error/.test(response.text)) throw new Error(`${what}: ${response.status} ${response.text.slice(0, 400)}`);
    return response;
  };
  await posted("create a member", "/users", { username: friend.username, password: friend.password, role: "operator" });
  const users = (await owner.get("/users")).text;
  const friendID = users.slice(users.indexOf(friend.username)).match(/\/users\/([0-9a-f-]{36})\//)[1];
  await posted("share Survival", `/users/${friendID}/grants`, new URLSearchParams([[`grant:${survival}`, "server.view"], [`grant:${survival}`, "server.start"]]));
  await posted("turn on monitoring", `/servers/${survival}/availability`, { enabled: "true", graceSeconds: "120" });
  await posted("configure a webhook", "/settings/notifications/webhook", { enabled: "true", url: hook });
  await posted("schedule a restart", `/servers/${survival}/schedules`, new URLSearchParams([["kind", "recurring"], ["action", "restart"],
    ["time", "04:00"], ["timezone", "UTC"], ["enabled", "true"], ...[0, 1, 2, 3, 4, 5, 6].map((day) => ["days", String(day)])]));
  await posted("back up Survival", `/servers/${survival}/backups`, {});
  const backup = await waitFor(async () => (await owner.get(`/servers/${survival}?tab=backups`)).text.match(/\/backups\/([0-9a-f-]{36})\/download/)?.[1],
    "the release's backup", 60_000);
  await stop(before.child);
  if (process.env.LUDOCK_E2E_KEEP) copyFileSync(join(dir, "data", "ludock.db"), join(dir, "release.db"));

  // This build, on the same data.
  const after = await ludock(newBin, 38412);
  const database = new DatabaseSync(join(dir, "data", "ludock.db"), { readOnly: true });
  const { user_version: version } = database.prepare("PRAGMA user_version").get();
  database.close();
  check(version === 14, `migrated the database to version 14 (found ${version})`);
  check(!after.child.lines.join("").includes("ERROR"), "started without errors");

  const admin2 = new Session(after.base);
  check((await admin2.post("/login", { username: admin.username, password: admin.password }, true)).status === 303, "the administrator signs in");
  const page = await admin2.get("/");
  check(serverID(page.text, "Survival") === survival, "Survival is the same server");
  const member = new Session(after.base);
  check((await member.post("/login", { username: friend.username, password: friend.password }, true)).status === 303, "the member signs in");
  const shared = await member.get("/");
  keep("member", shared.text);
  check(serverID(shared.text, "Survival") === survival && !serverID(shared.text, "valheim"), "the member still sees only the shared server");
  check((await member.get(`/servers/${survival}`)).status === 200, "the member can open the shared server");

  const schedules = keep("schedules", (await admin2.get(`/servers/${survival}?tab=schedules`)).text);
  check(/Restart/.test(schedules) && /04:00/.test(schedules), "the schedule is still listed");
  check(!/Review the schedule settings and server state|suspended|Waiting for an administrator/.test(schedules), "the schedule is still due");

  const backups = (await admin2.get(`/servers/${survival}?tab=backups`)).text;
  check(backups.includes(backup), "the backup is still listed");
  const restore = await admin2.post(`/servers/${survival}/restores`, { backupId: backup, confirmation: "Survival" });
  keep("restore", restore.status + " " + JSON.stringify([...restore.headers]) + restore.text);
  const operation = (restore.headers.get("HX-Location") || "").match(/operation=([0-9a-f-]{36})/)?.[1];
  check(!!operation, "the backup can be restored");
  if (operation) {
    const done = await waitFor(async () => {
      const text = (await admin2.get(`/activity/${operation}`)).text;
      return /Succeeded|Failed/.test(text) && text;
    }, "the restore", 60_000).catch(() => "");
    check(/Succeeded/.test(done), "the restore succeeds");
  }

  const monitoring = (await admin2.get(`/servers/${survival}`)).text;
  check(/name="enabled"[^>]*value="true"[^>]*checked|checked[^>]*name="enabled"/.test(monitoring), "monitoring is still on");
  const settings = keep("settings", (await admin2.get("/settings")).text);
  check(/id="notification-webhook-state">\s*(<[^>]*>\s*)*On\b/.test(settings), "the webhook is still configured and on");
  await stop(after.child);

  // A database from before the schema began, as v0.5.0 left: refused once,
  // and the process stays up with the message instead of exiting.
  renameSync(join(dir, "data"), join(dir, "upgraded"));
  mkdirSync(join(dir, "data"));
  const foreign = new DatabaseSync(join(dir, "data", "ludock.db"));
  foreign.exec("CREATE TABLE users (id TEXT); PRAGMA user_version = 3;");
  foreign.close();
  const refused = start("ludock refusing", join(newBin, "ludock"), ["serve"], { LUDOCK_LISTEN: "127.0.0.1:38413", LUDOCK_DATA_DIR: join(dir, "data"),
    LUDOCK_DOCKER_SOCKET: join(dir, "docker", "docker.sock"), LUDOCK_SETUP_CODE: setupCode, LUDOCK_BACKUP_ROOTS: join(dir, "backups") });
  const page503 = await waitFor(async () => {
    const response = await fetch("http://127.0.0.1:38413/");
    return response.status === 503 && (await response.text());
  }, "the refusal page").catch(() => "");
  check(page503.includes("This database is incompatible with Ludock"), "the panel's address shows why it refused");
  await new Promise((done) => setTimeout(done, 1000));
  const log = refused.lines.join("");
  check(refused.exitCode === null, "it stays up instead of exiting into a restart loop");
  check(log.split("This database is incompatible").length === 2, "its log says why once");
  writeFileSync(join(dir, "refused.log"), log);
} catch (error) {
  failures.push(String(error));
  console.error(error);
} finally {
  await Promise.all(children.map(stop));
  if (!process.env.LUDOCK_E2E_KEEP) rmSync(dir, { recursive: true, force: true });
}
if (failures.length) {
  console.error(`${failures.length} failed`);
  process.exit(1);
}
console.log(`Upgrading from ${tag} works.`);
