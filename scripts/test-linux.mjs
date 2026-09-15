#!/usr/bin/env bun
// Check the production bundle, then run the backend suites with its Linux
// runtime and dependencies. Neither fixture gets a Docker socket or network.
import { readdir, realpath } from "node:fs/promises";
import path from "node:path";

const repository = await realpath(path.resolve(import.meta.dir, ".."));
const name = "ludock-linux-tests-" + crypto.randomUUID();
const image = process.env.LUDOCK_TEST_IMAGE || "ludock:test";

// This function runs inside the image without source mounts, so missing bundle
// files, production dependencies, or frontend assets fail before source tests.
async function smokeProductionBundle() {
  if (["node", "npm", "npx"].some((command) => Bun.which(command))) {
    throw new Error("The production image must use Bun as its only JavaScript runtime");
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
  console.log("Production bundle serves health, frontend, and built assets.");
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
      "-v", repository + "/packages/backend/src:/app/packages/backend/src:ro",
      "-v", repository + "/packages/backend/test:/app/packages/backend/test:ro",
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
