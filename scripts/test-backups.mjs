#!/usr/bin/env node
// Build ludock:test first. Tests run inside that Linux image against disposable
// Docker volumes and a dedicated mounted destination; all fixtures are removed.
import { mkdtemp, readFile, writeFile, rm, realpath } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import { randomUUID } from "node:crypto";
const repository = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
);
const require = createRequire(
  path.join(repository, "packages/backend/package.json"),
);
const ts = require("typescript");
const folder = await realpath(
  await mkdtemp(path.join(os.tmpdir(), "ludock-backup-harness-")),
);
const tests = path.join(folder, "tests"),
  backups = path.join(folder, "backups");
const { mkdir } = await import("node:fs/promises");
await mkdir(tests);
await mkdir(backups);
const name = `ludock-backup-harness-${randomUUID()}`;
try {
  for (const file of [
    "backup-storage.test.ts",
    "backup-docker.test.ts",
    "restore-helper-script.test.ts",
    "restore-extract-script.test.ts",
  ]) {
    const source = (
      await readFile(
        path.join(repository, "packages/backend/test", file),
        "utf8",
      )
    ).replaceAll("../src/", "../dist/");
    const result = ts.transpileModule(source, {
      compilerOptions: {
        target: ts.ScriptTarget.ES2022,
        module: ts.ModuleKind.ESNext,
      },
    });
    await writeFile(
      path.join(tests, file.replace(/\.ts$/, ".mjs")),
      result.outputText,
    );
  }
  const result = spawnSync(
    "docker",
    [
      "run",
      "--rm",
      "--name",
      name,
      "--label",
      "ludock.enable=false",
      "-e",
      "LUDOCK_DOCKER_TESTS=1",
      "-e",
      `LUDOCK_SELF_CONTAINER=${name}`,
      "-e",
      "LUDOCK_TEST_BACKUP_DIRECTORY=/backup-fixtures",
      "-v",
      "/var/run/docker.sock:/var/run/docker.sock",
      "-v",
      `${tests}:/app/packages/backend/test:ro`,
      "-v",
      `${backups}:/backup-fixtures`,
      process.env.LUDOCK_TEST_IMAGE || "ludock:test",
      "node",
      "--test",
      "packages/backend/test/backup-storage.test.mjs",
      "packages/backend/test/backup-docker.test.mjs",
      "packages/backend/test/restore-helper-script.test.mjs",
      "packages/backend/test/restore-extract-script.test.mjs",
    ],
    { stdio: "inherit" },
  );
  process.exitCode = result.status ?? 1;
} finally {
  spawnSync("docker", ["rm", "-fv", name], { stdio: "ignore" });
  await rm(folder, { recursive: true, force: true });
}
