#!/usr/bin/env bun
// Check the production bundle, then run the backend suites with its Linux
// runtime. Neither fixture gets a Docker socket or network.
import { readdir, realpath } from "node:fs/promises";
import path from "node:path";
import { backendSourceMounts } from "./test-source-mounts.mjs";

const repository = await realpath(path.resolve(import.meta.dir, ".."));
const name = "ludock-linux-tests-" + crypto.randomUUID();
const image = process.env.LUDOCK_TEST_IMAGE || "ludock:test";

// This function runs inside the image without source mounts, so missing bundle
// files, production dependencies, or frontend assets fail before source tests.
async function smokeProductionBundle() {
  if (["node", "npm", "npx"].some((command) => Bun.which(command))) {
    throw new Error("The production image must use Bun as its only JavaScript runtime");
  }
  if ([...new Bun.Glob("**/node_modules").scanSync({ cwd: "/app", onlyFiles: false })].length) {
    throw new Error("Production must run its bundles without installed packages");
  }
  const inventory = await Bun.file("packages/backend/dist/dependencies.cdx.json").json();
  if (inventory.bomFormat !== "CycloneDX" || !inventory.components.length) {
    throw new Error("Production bundle dependency inventory is missing");
  }
  for (const entry of ["index", "recovery"]) {
    if (!(await Bun.file(`packages/backend/dist/${entry}.js.map`).exists())) {
      throw new Error(`Production ${entry} source map is missing`);
    }
  }
  const recovery = Bun.spawnSync([process.execPath, "packages/backend/dist/recovery.js"], {
    env: { ...process.env, LUDOCK_DB_PATH: ":memory:", LUDOCK_RECOVERY_PASSWORD: "" },
    stdout: "pipe", stderr: "pipe",
  });
  if (recovery.exitCode !== 2 || !new TextDecoder().decode(recovery.stderr).includes("Usage:")) {
    throw new Error("Production account recovery entry point did not load");
  }
  for (const command of [
    [process.execPath, "--version"],
    ["docker", "--version"],
    ["docker", "compose", "version"],
  ]) {
    const result = Bun.spawnSync(command, { stdout: "inherit", stderr: "inherit" });
    if (result.exitCode !== 0) {
      throw new Error("Production runtime command failed: " + command.join(" "));
    }
  }
  const base = "http://127.0.0.1:3000";
  let ready = false;
  const deadline = Date.now() + 15_000;
  while (Date.now() < deadline) {
    try {
      const response = await fetch(base + "/api/v1/health", {
        signal: AbortSignal.timeout(1000),
      });
      const body = await response.json();
      if (response.status === 503 && body.status === "degraded") {
        ready = true;
        break;
      }
    } catch {
      // The listener may still be starting.
    }
    await Bun.sleep(100);
  }
  if (!ready) throw new Error("Production bundle did not serve health within 15s");
  const response = await fetch(base, { signal: AbortSignal.timeout(2000) });
  const html = await response.text();
  if (!response.ok || !response.headers.get("content-type")?.includes("text/html")) {
    throw new Error("Production bundle did not serve the frontend document");
  }
  const scriptPath = html.match(/<script\b[^>]*\bsrc=["']([^"']+)["']/)?.[1];
  if (!scriptPath) {
    throw new Error("Production frontend document has no built script");
  }
  const scriptUrl = new URL(scriptPath, base);
  if (scriptUrl.origin !== base || !scriptUrl.pathname.endsWith(".js")) {
    throw new Error("Production frontend document has no local bundled script");
  }
  const script = await fetch(scriptUrl, {
    signal: AbortSignal.timeout(2000),
  });
  if (!script.ok || !script.headers.get("content-type")?.includes("javascript")) {
    throw new Error("Production bundle did not serve its frontend script");
  }
  await script.arrayBuffer();
  const username = "bundle-fixture-admin";
  const oldPassword = "fixture-original-password-0123456789";
  const newPassword = "fixture-recovered-password-0123456789";
  const post = (endpoint, body) => fetch(base + "/api/v1" + endpoint, {
    method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
    signal: AbortSignal.timeout(15_000),
  });
  const setup = await post("/auth/setup", { username, password: oldPassword, bootstrapCode: process.env.LUDOCK_SETUP_CODE });
  if (!setup.ok) throw new Error("Production bundle administrator setup failed");
  const cookie = setup.headers.getSetCookie().map((value) => value.split(";", 1)[0]).join("; ");
  await setup.arrayBuffer();
  if (!cookie) throw new Error("Production setup did not create a session");
  const recovered = Bun.spawnSync([process.execPath, "packages/backend/dist/recovery.js", username], {
    env: { ...process.env, LUDOCK_RECOVERY_PASSWORD: newPassword }, stdout: "pipe", stderr: "pipe",
  });
  if (recovered.exitCode !== 0) throw new Error("Production account recovery failed");
  const status = await fetch(base + "/api/v1/auth/status", { headers: { Cookie: cookie } });
  if ((await status.json()).authenticated) throw new Error("Account recovery left the old session active");
  const oldLogin = await post("/auth/login", { username, password: oldPassword });
  await oldLogin.arrayBuffer();
  if (oldLogin.status !== 401) throw new Error("Account recovery left the old password usable");
  const newLogin = await post("/auth/login", { username, password: newPassword });
  await newLogin.arrayBuffer();
  if (!newLogin.ok) throw new Error("Recovered account could not log in");
  console.log("Production bundle serves health/assets and recovers accounts with session revocation.");
}

function runDocker(args, stdout = "inherit") {
  const result = Bun.spawnSync(["docker", ...args], { stdin: "inherit", stdout, stderr: "inherit" });
  if (result.exitCode !== 0) throw new Error("Docker " + args[0] + " failed (exit " + result.exitCode + ")");
  return result;
}

try {
  // Start the image's default command with its own disposable /data volume.
  runDocker([
    "run", "-d", "--name", name + "-smoke",
    "--network", "none", "--label", "ludock.enable=false",
    "-e", "LUDOCK_SETUP_CODE=fixture-setup-" + crypto.randomUUID(),
    image,
  ]);
  runDocker(["exec", name + "-smoke", "bun", "-e", "await (" + smokeProductionBundle.toString() + ")()"]);
  runDocker(["stop", "--time", "5", name + "-smoke"]);
  const exitCode = runDocker(["inspect", "--format", "{{.State.ExitCode}}", name + "-smoke"], "pipe")
    .stdout.toString().trim();
  if (exitCode !== "0") {
    throw new Error("Production bundle did not shut down cleanly (exit " + exitCode + ")");
  }
  const files = (await readdir(path.join(repository, "packages/backend/test")))
    .filter((file) => file.endsWith(".test.ts"))
    .sort();
  const result = Bun.spawnSync(
    [
      "docker",
      "run", "--rm", "--name", name,
      "--network", "none", "--label", "ludock.enable=false",
      ...backendSourceMounts(repository),
      image, "bun", "test", "--isolate",
      ...files.map((file) => "./packages/backend/test/" + file),
    ],
    { stdin: "inherit", stdout: "inherit", stderr: "inherit" },
  );
  process.exitCode = result.exitCode ?? 1;
} catch (error) {
  console.error(error instanceof Error ? error.message : error);
  Bun.spawnSync(["docker", "logs", name + "-smoke"], { stdout: "inherit", stderr: "inherit" });
  process.exitCode = 1;
} finally {
  Bun.spawnSync(["docker", "rm", "-fv", name + "-smoke", name], { stdin: "ignore", stdout: "ignore", stderr: "ignore" });
}
