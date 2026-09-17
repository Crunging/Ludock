#!/usr/bin/env bun
// Explicit Docker client and file acceptance. Build ludock:test first; all game data uses
// disposable named volumes and the harness never discovers unrelated servers.
import { realpath } from "node:fs/promises";
import path from "node:path";
import { backendSourceMounts } from "./test-source-mounts.mjs";

const repository = await realpath(path.resolve(import.meta.dir, ".."));
const name = "ludock-file-harness-" + crypto.randomUUID();
try {
  const result = Bun.spawnSync(
    [
      "docker",
      "run", "--rm", "--name", name,
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
