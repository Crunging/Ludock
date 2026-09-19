#!/usr/bin/env bun
// Exercise the shipped API and embedded helper programs with disposable data.
// Build ludock:test first. Run sequentially with the other Docker harnesses.
import { expect } from "bun:test";
import { hardenedContainerArguments } from "./test-container-options.mjs";

const name = `ludock-packaged-${crypto.randomUUID()}`;
const app = `${name}-app`;
const game = `${name}-game`;
const volumes = [`${name}-world`, `${name}-backups`];
const token = `fixture-${crypto.randomUUID()}`;
const image = process.env.LUDOCK_TEST_IMAGE || "ludock:test";
const expectedBun = (await Bun.file(new URL("../.bun-version", import.meta.url)).text()).trim();
let base;
let serverId;
let root;
let passed = false;
const createdVolumes = [];
const createdContainers = [];

function docker(...args) {
  const result = Bun.spawnSync(["docker", ...args], { stdout: "pipe", stderr: "pipe" });
  if (result.exitCode !== 0) throw new Error(`Docker ${args[0]} failed: ${result.stderr.toString().slice(-2000).replaceAll(token, "[redacted]")}`);
  return result.stdout.toString().trim();
}
const inspect = (container) => JSON.parse(docker("inspect", container))[0];
const running = () => inspect(game).State.Running;
async function response(endpoint, options = {}) {
  const deadline = Date.now() + 30_000;
  while (true) {
    const result = await fetch(`${base}/api/v1${endpoint}`, {
      ...options,
      headers: { Authorization: `Bearer ${token}`, ...options.headers },
      signal: AbortSignal.timeout(30_000),
    });
    if (result.ok) return result;
    const body = await result.text();
    // EOF can reach the client before helper cleanup releases the server lock.
    // Only this explicit pre-dispatch rejection is safe to retry for mutations.
    if (result.status === 409 && JSON.parse(body).code === "OPERATION_CONFLICT" && Date.now() < deadline) {
      await Bun.sleep(100);
      continue;
    }
    throw new Error(`${endpoint}: HTTP ${result.status} ${body}`);
  }
}
async function json(endpoint, body, method = body === undefined ? "GET" : "POST") {
  return (await response(endpoint, {
    method, headers: { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  })).json();
}
const files = (suffix, path, extra = {}) => `/servers/${serverId}/files${suffix}?${new URLSearchParams({ root, path, ...extra })}`;
async function upload(path, name, bytes) {
  await (await response(files("/upload", path, { name }), {
    method: "PUT", headers: { "Content-Type": "application/octet-stream" }, body: bytes,
  })).arrayBuffer();
}
const download = async (path) => new Uint8Array(await (await response(files("/download", path))).arrayBuffer());
const checksum = (bytes) => new Bun.CryptoHasher("sha256").update(bytes).digest("hex");
async function operation(endpoint, body) {
  let { operation } = await json(`/servers/${serverId}/${endpoint}`, body);
  const deadline = Date.now() + 120_000;
  while (["queued", "running"].includes(operation.status) && Date.now() < deadline) {
    await Bun.sleep(200);
    ({ operation } = await json(`/operations/${operation.id}`));
  }
  expect(operation.status, JSON.stringify(operation)).toBe("succeeded");
  return operation.result;
}

try {
  expect(docker("run", "--rm", "--network", "none", "--entrypoint", "/usr/local/bin/bun", image, "--version")).toBe(expectedBun);
  for (const volume of volumes) {
    docker("volume", "create", volume);
    createdVolumes.push(volume);
  }
  docker("create", "--name", game, "--init", "--network", "none",
    "--label", "ludock.enable=true", "--label", `ludock.name=${game}`,
    "-v", `${volumes[0]}:/data`, "--entrypoint", "/usr/local/bin/bun", image,
    "-e", "process.on('SIGTERM', () => process.exit(0)); setInterval(() => {}, 3600000)");
  createdContainers.push(game);
  // Only the socket and a fresh backup volume are mounted. The image owns its
  // entry point, dependencies, helper source strings, and disposable /data.
  docker("create", "--name", app, "--label", "ludock.enable=false", "-p", "127.0.0.1::3000",
    ...hardenedContainerArguments,
    "-e", `LUDOCK_API_TOKEN=${token}`,
    "-v", `${volumes[1]}:/backups`,
    "-v", "/var/run/docker.sock:/var/run/docker.sock", image);
  createdContainers.push(app);
  docker("start", app);
  base = `http://127.0.0.1:${inspect(app).NetworkSettings.Ports["3000/tcp"][0].HostPort}`;
  const deadline = Date.now() + 20_000;
  let ready = false;
  while (Date.now() < deadline) {
    try { await json("/health"); ready = true; break; }
    catch { await Bun.sleep(200); }
  }
  expect(ready, "Packaged API must become healthy").toBe(true);
  expect((await json("/settings/deployment")).backupRoots).toEqual(["/backups"]);
  const server = (await json("/servers")).servers.find((entry) => entry.displayName === game);
  expect(server, "Discover only the unique game fixture").toBeTruthy();
  serverId = server.id;
  root = server.fileRoots.find((entry) => entry.path === "/data")?.id;
  expect(root).toBeTruthy();
  expect(serverId).not.toBe(inspect(game).Id);
  await json("/settings/backups", { destination: "/backups", retentionCount: 10, maxBytes: 100_000_000, reserveBytes: 0 }, "PUT");

  const payload = Uint8Array.from({ length: 2 * 1024 * 1024 + 37 }, (_, index) => index % 251);
  for (const state of ["stopped", "running"]) {
    if (state === "running") docker("start", game);
    const initiallyRunning = state === "running";
    await json(`/servers/${serverId}/files/directory`, { root, path: "", name: state });
    await upload(state, "world.bin", payload);
    expect(checksum(await download(`${state}/world.bin`))).toBe(checksum(payload));
    expect((await json(files("", state))).entries.map((entry) => entry.name)).toEqual(["world.bin"]);

    const { backupId } = await operation("backups", {});
    expect(running(), `Backup must preserve ${state} state`).toBe(initiallyRunning);
    const backup = (await json(`/servers/${serverId}/backups`)).backups.find((entry) => entry.id === backupId);
    expect(backup.state).toBe("complete");
    const archive = new Uint8Array(await (await response(`/servers/${serverId}/backups/${backupId}/download`)).arrayBuffer());
    expect(archive.length).toBe(backup.size);
    expect(checksum(archive)).toBe(backup.checksum);
    // Bun.Archive independently checks the generated tar and its contents.
    const archived = await new Bun.Archive(archive).files();
    expect(checksum(await archived.get(`snapshot/${root}/${state}/world.bin`).arrayBuffer())).toBe(checksum(payload));

    await upload(state, "world.bin", new TextEncoder().encode("changed-world"));
    await upload(state, "extra.txt", new TextEncoder().encode("created-after-backup"));
    const restored = await operation("restores", { backupId, confirmation: game });
    expect(restored.restoredBackupId).toBe(backupId);
    expect(running(), `Restore must preserve ${state} state`).toBe(initiallyRunning);
    expect(checksum(await download(`${state}/world.bin`))).toBe(checksum(payload));
    expect((await json(files("", state))).entries.map((entry) => entry.name)).toEqual(["world.bin"]);
    expect((await json(`/servers/${serverId}/backups`)).backups.some((entry) => entry.id === restored.safetyBackupId)).toBe(true);
  }
  docker("stop", "--time", "10", app);
  expect(inspect(app).State.ExitCode, "Packaged backend must finish cleanup before shutdown").toBe(0);
  passed = true;
  console.log(JSON.stringify({ result: "pass", packagedApi: true, fileRoundTrip: true, backupChecksum: true, restoreContents: true, safetyBackups: true, runningAndStoppedState: true }));
} finally {
  if (!passed && createdContainers.includes(app)) {
    const logs = Bun.spawnSync(["docker", "logs", "--tail", "100", app], { stdout: "pipe", stderr: "pipe" });
    console.error((logs.stdout.toString() + logs.stderr.toString()).replaceAll(token, "[redacted]"));
  }
  // Stop the app before removing its fixture data. All resource names are owned
  // by this invocation; cleanup failures must remain visible to CI.
  const errors = [];
  if (createdContainers.includes(app)) {
    try { docker("stop", "--time", "15", app); } catch (error) { errors.push(error); }
  }
  for (const container of createdContainers.toReversed()) {
    try { docker("rm", "-fv", container); } catch (error) { errors.push(error); }
  }
  for (const volume of createdVolumes.toReversed()) {
    try { docker("volume", "rm", volume); } catch (error) { errors.push(error); }
  }
  if (errors.length) {
    console.error(new AggregateError(errors, "Packaged fixture cleanup failed"));
    process.exitCode = 1;
  }
}
