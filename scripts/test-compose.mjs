#!/usr/bin/env bun
import { expect } from "bun:test";
// Opt-in Docker acceptance against unique fixture containers and volumes.
// Build ludock:test first, or set LUDOCK_TEST_IMAGE to another local image.
import { mkdtemp, realpath, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";

// The update API intentionally requires a pullable tag. Check its current
// contents against the reviewed pin before creating any fixture services.
const expectedFixtureImage = "alpine:3@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b";
const project = `ludock-compose-smoke-${crypto.randomUUID().slice(0, 8)}`;
const root = await realpath(await mkdtemp(path.join(process.env.LUDOCK_TEST_DIRECTORY || tmpdir(), `${project}-`)));
const app = `${project}-app`;
const token = `test-only-${crypto.randomUUID()}`;
const compose = path.join(root, "compose.yaml");
let base;
let serverId;
await Bun.write(compose, `services:
  game:
    image: alpine:3
    command: ["sleep", "infinity"]
    environment:
      WORLD: "\${FIXTURE_WORLD}"
      LITERAL: "cash$$value"
      BRACED: "$\u0024{literal}"
    labels:
      ludock.enable: "true"
      ludock.name: "Compose Smoke"
    depends_on: [dependency]
  dependency:
    image: alpine:3
    command: ["sleep", "infinity"]
`);

await Bun.write(path.join(root, ".env"), "FIXTURE_WORLD=automatic-source\n");

function docker(...args) {
  const result = Bun.spawnSync(["docker", ...args], { stdout: "pipe", stderr: "pipe" });
  if (result.exitCode !== 0)
    throw new Error(`Docker command failed: ${result.stderr.toString().slice(-1000).replaceAll(token, "[redacted]")}`);
  return result.stdout.toString().trim();
}
const cli = (...args) => docker("compose", "--project-name", project, "--project-directory", root, "-f", compose, ...args);
const inspect = (container) => JSON.parse(docker("inspect", container))[0];
async function request(endpoint, body, method = body === undefined ? "GET" : "POST") {
  const response = await fetch(`${base}/api/v1${endpoint}`, {
    method,
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(30_000),
  });
  if (!response.ok) throw new Error(`${response.status} ${await response.text()}`);
  return response.json();
}
async function update(forceRecreate) {
  let { operation } = await request(`/servers/${serverId}/updates`, {
    createBackup: false, skipBackupConfirmation: "Compose Smoke", forceRecreate,
  });
  const deadline = Date.now() + 300_000;
  while (Date.now() < deadline) {
    ({ operation } = await request(`/operations/${operation.id}`));
    if (!["queued", "running"].includes(operation.status)) return operation;
    await Bun.sleep(500);
  }
  throw new Error("Operation timeout");
}

try {
  const [fixtureTag, expectedDigest] = expectedFixtureImage.split("@");
  docker("pull", fixtureTag);
  const fixture = JSON.parse(docker("image", "inspect", fixtureTag))[0];
  expect(fixture.RepoDigests.some((reference) => reference.endsWith(`@${expectedDigest}`)), "The fixture image tag changed; review and update its pin before running Compose acceptance").toBeTruthy();
  cli("up", "-d");
  const before = cli("ps", "-q", "game");
  const dependency = cli("ps", "-q", "dependency");
  docker("run", "-d", "--name", app, "--label", "ludock.enable=false", "-p", "127.0.0.1::3000",
    "-e", `LUDOCK_API_TOKEN=${token}`, "-e", `LUDOCK_COMPOSE_ROOTS=${root}`,
    "-e", "UNRELATED_LUDOCK_SECRET=must-not-be-inherited",
    "-v", "/var/run/docker.sock:/var/run/docker.sock", "-v", `${root}:${root}:ro`,
    process.env.LUDOCK_TEST_IMAGE || "ludock:test");
  const port = inspect(app).NetworkSettings.Ports["3000/tcp"][0].HostPort;
  base = `http://127.0.0.1:${port}`;
  let ready = false;
  for (let attempt = 0; attempt < 100; attempt++) {
    try { await request("/health"); ready = true; break; }
    catch { await Bun.sleep(200); }
  }
  expect(ready, "Fixture API must become healthy").toBeTruthy();
  serverId = (await request("/servers")).servers.find((server) => server.displayName === "Compose Smoke").id;
  const { capability } = await request(`/servers/${serverId}/update-capability`);
  expect(capability.available, capability.unavailableReason).toBe(true);
  const unchanged = await update(false);
  expect(unchanged.status).toBe("already_current");
  expect(cli("ps", "-q", "game")).toBe(before);
  const forced = await update(true);
  expect(forced.status).toBe("succeeded");
  const after = cli("ps", "-q", "game");
  expect(after).not.toBe(before);
  expect(cli("ps", "-q", "dependency")).toBe(dependency);
  const environment = inspect(after).Config.Env;
  expect(environment.includes("WORLD=automatic-source")).toBeTruthy();
  expect(environment.includes("LITERAL=cash$value")).toBeTruthy();
  expect(environment.includes("BRACED=${literal}")).toBeTruthy();
  expect(environment.every((entry) => !entry.includes("must-not-be-inherited") && !entry.includes(token))).toBeTruthy();
  // Discovery must survive both snapshot cleanup and an application restart.
  docker("restart", app);
  base = `http://127.0.0.1:${inspect(app).NetworkSettings.Ports["3000/tcp"][0].HostPort}`;
  let restarted = false;
  for (let attempt = 0; attempt < 100; attempt++) {
    try { await request("/health"); restarted = true; break; }
    catch { await Bun.sleep(200); }
  }
  expect(restarted, "Fixture API must recover after restart").toBeTruthy();
  await request(`/servers/${serverId}/stop`, {}, "POST");
  const stopped = await update(true);
  expect(stopped.status).toBe("succeeded");
  const final = cli("ps", "-a", "-q", "game");
  expect(final).not.toBe(after);
  expect(inspect(final).State.Running).toBe(false);
  expect(cli("ps", "-q", "dependency")).toBe(dependency);

  // Verify Docker Desktop has propagated the host write before validation.
  const source = await Bun.file(compose).text() + "\n# changed owner source\n";
  await Bun.write(compose, source);
  const changedHash = new Bun.CryptoHasher("sha256").update(source).digest("hex");
  const deadline = Date.now() + 5_000;
  let propagated = false;
  while (Date.now() < deadline) {
    if (docker("exec", app, "sha256sum", compose).split(/\s+/)[0] === changedHash) {
      propagated = true;
      break;
    }
    await Bun.sleep(100);
  }
  expect(propagated, "Changed Compose source must reach the fixture mount").toBeTruthy();
  expect((await request(`/servers/${serverId}/update-capability`)).capability.available).toBe(true);
  const edited = await update(true);
  expect(edited.status).toBe("succeeded");
  expect(inspect(cli("ps", "-a", "-q", "game")).State.Running).toBe(false);
  console.log(JSON.stringify({
    result: "pass", logicalIdentitySurvived: true, alreadyCurrent: unchanged.status,
    forcedRunning: forced.status, forcedStopped: stopped.status, dependencyUntouched: true,
    literalDollarPreserved: true, defaultEnvPreserved: true, applicationRestartSurvived: true, sourceChangeAccepted: true, registrationRequired: false,
  }));
} finally {
  Bun.spawnSync(["docker", "rm", "-fv", app], { stdout: "ignore", stderr: "ignore" });
  Bun.spawnSync(["docker", "compose", "--project-name", project, "-f", compose, "down", "--volumes", "--remove-orphans"], { stdout: "ignore", stderr: "ignore" });
  await rm(root, { recursive: true, force: true });
}
