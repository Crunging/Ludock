#!/usr/bin/env node
// Run the ordinary backend suites against the built Linux production modules.
// This fixture gets no Docker socket, external network, or persistent app data.
import {
  mkdtemp,
  mkdir,
  readFile,
  readdir,
  realpath,
  rm,
  writeFile,
} from "node:fs/promises";
import { createRequire } from "node:module";
import { spawnSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import { fileURLToPath } from "node:url";
import os from "node:os";
import path from "node:path";

const repository = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
);
const require = createRequire(
  path.join(repository, "packages/backend/package.json"),
);
const ts = require("typescript");
const folder = await realpath(
  await mkdtemp(path.join(os.tmpdir(), "ludock-linux-tests-")),
);
const tests = path.join(folder, "tests");
const name = `ludock-linux-tests-${randomUUID()}`;
await mkdir(tests);
try {
  const files = (await readdir(path.join(repository, "packages/backend/test")))
    .filter((file) => file.endsWith(".test.ts"))
    .sort();
  for (const file of files) {
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
      "--network",
      "none",
      "--label",
      "ludock.enable=false",
      "-v",
      `${tests}:/app/packages/backend/test:ro`,
      process.env.LUDOCK_TEST_IMAGE || "ludock:test",
      "node",
      "--test",
      ...files.map(
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
