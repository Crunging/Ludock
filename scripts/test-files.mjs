#!/usr/bin/env bun
// Explicit Docker client and file acceptance. Build ludock:test first; all game data uses
// disposable named volumes and the harness never discovers unrelated servers.
import { realpath } from "node:fs/promises";
import path from "node:path";
import { backendSourceMounts } from "./test-source-mounts.mjs";
import { hardenedContainerArguments } from "./test-container-options.mjs";
import { FALLBACK_HELPER_IMAGE } from "../packages/backend/src/runtime-images.ts";

const args = process.argv.slice(2);
if (args.length && (args.length !== 1 || args[0] !== "--fallback-helper")) {
  throw new Error("Usage: bun scripts/test-files.mjs [--fallback-helper]");
}
const fallback = args.length === 1;

const repository = await realpath(path.resolve(import.meta.dir, ".."));
const name = "ludock-file-harness-" + crypto.randomUUID();
try {
  if (fallback) {
    const pull = Bun.spawnSync(["docker", "pull", FALLBACK_HELPER_IMAGE], { stdout: "inherit", stderr: "inherit" });
    if (pull.exitCode !== 0) throw new Error("Fallback helper pull failed");
    const version = Bun.spawnSync(["docker", "run", "--rm", "--network", "none", "--entrypoint", "bun", FALLBACK_HELPER_IMAGE, "--version"], { stdout: "pipe", stderr: "inherit" });
    const required = (await Bun.file(new URL("../package.json", import.meta.url)).json()).engines.bun;
    if (version.exitCode !== 0 || !Bun.semver.satisfies(version.stdout.toString().trim(), required)) {
      throw new Error("Fallback helper does not meet the required Bun version");
    }
  }
  const result = Bun.spawnSync(
    [
      "docker",
      "run", "--rm", "--name", name,
      ...(fallback ? ["--hostname", `${name}-custom`] : []),
      ...hardenedContainerArguments,
      "--label", "ludock.enable=false",
      "-e", "LUDOCK_DOCKER_TESTS=1",
      "-v", "/var/run/docker.sock:/var/run/docker.sock",
      ...backendSourceMounts(repository),
      process.env.LUDOCK_TEST_IMAGE || "ludock:test",
      "bun", "test", "--isolate",
      "./packages/backend/test/docker-client.integration.ts",
      "./packages/backend/test/docker-storage.integration.ts",
    ],
    { stdin: "inherit", stdout: "inherit", stderr: "inherit" },
  );
  process.exitCode = result.exitCode ?? 1;
} finally {
  Bun.spawnSync(["docker", "rm", "-fv", name], { stdin: "ignore", stdout: "ignore", stderr: "ignore" });
}
