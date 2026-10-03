#!/usr/bin/env bun
// Explicit Docker client and file acceptance. Build ludock:test first; all game data uses
// disposable named volumes and the harness never discovers unrelated servers.
import { realpath } from "node:fs/promises";
import path from "node:path";
import { backendSourceMounts } from "./test-source-mounts.mjs";
import { hardenedContainerArguments } from "./test-container-options.mjs";
import { FALLBACK_HELPER_IMAGE } from "../packages/backend/src/runtime-images.ts";
import { createTestDocker, cleanupFixtures } from "./test-docker.mjs";

const args = process.argv.slice(2);
if (args.length && (args.length !== 1 || args[0] !== "--fallback-helper")) {
  throw new Error("Usage: bun scripts/test-files.mjs [--fallback-helper]");
}
const fallback = args.length === 1;

const repository = await realpath(path.resolve(import.meta.dir, ".."));
const name = "ludock-file-harness-" + crypto.randomUUID();
const { docker, run, image, socketArguments, removeContainer } = createTestDocker();
try {
  if (fallback) {
    run(["pull", FALLBACK_HELPER_IMAGE], "inherit");
    const version = docker("run", "--rm", "--network", "none", "--entrypoint", "bun", FALLBACK_HELPER_IMAGE, "--version");
    const required = (await Bun.file(new URL("../package.json", import.meta.url)).json()).engines.bun;
    if (!Bun.semver.satisfies(version, required)) {
      throw new Error("Fallback helper does not meet the required Bun version");
    }
  }
  run(
    [
      "run", "--rm", "--name", name,
      ...(fallback ? ["--hostname", `${name}-custom`] : []),
      ...hardenedContainerArguments,
      "--label", "ludock.enable=false",
      "-e", "LUDOCK_DOCKER_TESTS=1",
      ...socketArguments,
      ...backendSourceMounts(repository),
      image,
      "bun", "test", "--isolate",
      "./packages/backend/test/docker-client.integration.ts",
      "./packages/backend/test/docker-storage.integration.ts",
    ],
    "inherit",
  );
} finally {
  await cleanupFixtures([() => removeContainer(name)]);
}
