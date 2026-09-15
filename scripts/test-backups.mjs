#!/usr/bin/env bun
// Build ludock:test first. Tests run inside that Linux image against disposable
// Docker volumes and a dedicated mounted destination; all fixtures are removed.
import { mkdir, mkdtemp, realpath, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { backendSourceMounts } from "./test-source-mounts.mjs";

const repository = await realpath(path.resolve(import.meta.dir, ".."));
const folder = await realpath(
  await mkdtemp(path.join(os.tmpdir(), "ludock-backup-harness-")),
);
const backups = path.join(folder, "backups");
const name = "ludock-backup-harness-" + crypto.randomUUID();
try {
  await mkdir(backups);
  const result = Bun.spawnSync(
    [
      "docker",
      "run", "--rm", "--name", name,
      "--label", "ludock.enable=false",
      "-e", "LUDOCK_DOCKER_TESTS=1",
      "-e", "LUDOCK_SELF_CONTAINER=" + name,
      "-e", "LUDOCK_TEST_BACKUP_DIRECTORY=/backup-fixtures",
      "-v", "/var/run/docker.sock:/var/run/docker.sock",
      ...backendSourceMounts(repository),
      "-v", backups + ":/backup-fixtures",
      process.env.LUDOCK_TEST_IMAGE || "ludock:test",
      "bun", "test", "--isolate",
      "./packages/backend/test/backup-docker.test.ts",
    ],
    { stdin: "inherit", stdout: "inherit", stderr: "inherit" },
  );
  process.exitCode = result.exitCode ?? 1;
} finally {
  Bun.spawnSync(["docker", "rm", "-fv", name], { stdin: "ignore", stdout: "ignore", stderr: "ignore" });
  await rm(folder, { recursive: true, force: true });
}
