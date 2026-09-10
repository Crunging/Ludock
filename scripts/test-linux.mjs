#!/usr/bin/env bun
// Check the production bundle, then run the backend suites with its Linux
// runtime and dependencies. Neither fixture gets a Docker socket or network.
import { readdir, realpath } from "node:fs/promises";
import { randomUUID } from "node:crypto";
import { fileURLToPath } from "node:url";
import path from "node:path";

const repository = await realpath(path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
));
const name = "ludock-linux-tests-" + randomUUID();
const image = process.env.LUDOCK_TEST_IMAGE || "ludock:test";

// This function runs inside the image without source mounts, so missing bundle
// files, production dependencies, or frontend assets fail before source tests.
async function smokeProductionBundle() {
  const child = Bun.spawn([process.execPath, "packages/backend/dist/index.js"], {
    env: {
      ...process.env,
      HOST: "127.0.0.1",
      PORT: "3000",
      LUDOCK_DB_PATH: "/tmp/ludock-bundle-smoke.db",
      DOCKER_SOCKET: "/tmp/ludock-unavailable.sock",
      LOG_LEVEL: "error",
    },
    stdout: "inherit",
    stderr: "inherit",
  });
  const base = "http://127.0.0.1:3000";
  try {
    let ready = false;
    const deadline = Date.now() + 15_000;
    while (Date.now() < deadline) {
      if (child.exitCode !== null) {
        throw new Error("Production bundle exited with " + child.exitCode);
      }
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
  } finally {
    child.kill("SIGTERM");
    const stopTimer = setTimeout(() => child.kill("SIGKILL"), 5000);
    try {
      await child.exited;
    } finally {
      clearTimeout(stopTimer);
    }
  }
}

try {
  const smoke = Bun.spawnSync(
    [
      "docker",
      "run", "--rm", "--name", name + "-smoke",
      "--network", "none", "--label", "ludock.enable=false",
      image, "bun", "-e", "await (" + smokeProductionBundle.toString() + ")()",
    ],
    { stdin: "inherit", stdout: "inherit", stderr: "inherit" },
  );
  if (smoke.exitCode !== 0) {
    process.exitCode = smoke.exitCode ?? 1;
  } else {
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
  }
} finally {
  Bun.spawnSync(["docker", "rm", "-fv", name + "-smoke", name], { stdin: "ignore", stdout: "ignore", stderr: "ignore" });
}
