#!/usr/bin/env node
// Explicit Docker acceptance. Build ludock:test first; all game data uses
// disposable named volumes and the harness never discovers unrelated servers.
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
  await mkdtemp(path.join(os.tmpdir(), "ludock-file-harness-")),
);
const name = `ludock-file-harness-${randomUUID()}`;
try {
  const tests = ["file-helper-linux.test.ts", "docker-storage.integration.ts"];
  for (const file of tests) {
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
      path.join(folder, file.replace(/\.ts$/, ".mjs")),
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
      "-v",
      "/var/run/docker.sock:/var/run/docker.sock",
      "-v",
      `${folder}:/app/packages/backend/test:ro`,
      process.env.LUDOCK_TEST_IMAGE || "ludock:test",
      "node",
      "--test",
      ...tests.map(
        (file) => `packages/backend/test/${file.replace(/\.ts$/, ".mjs")}`,
      ),
    ],
    { stdio: "inherit" },
  );
  process.exitCode = result.status ?? 1;
} finally {
  spawnSync("docker", ["rm", "-fv", name], { stdio: "ignore" });
  await rm(folder, { recursive: true, force: true });
}
