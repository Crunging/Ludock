import assert from "node:assert/strict";
import { copyFile, mkdir, mkdtemp, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { it } from "bun:test";

const root = fileURLToPath(new URL("../../", import.meta.url));

// Exercise the real configs against isolated code so a tooling migration cannot
// silently drop type-aware rules, React checks, or the JavaScript helper checks.
async function lintFixture(workspace, files) {
  const directory = await mkdtemp(path.join(tmpdir(), "ludock-lint-"));
  try {
    const cwd = path.join(directory, "packages", workspace);
    const installed = path.join(root, "packages", workspace, "node_modules");
    await mkdir(cwd, { recursive: true });
    // This fixture runs outside the checkout's bunfig/PATH shim. A Node
    // shebang must not silently use a separately installed Node runtime.
    const bin = path.join(directory, "bin");
    await mkdir(bin);
    await writeFile(path.join(bin, "node"), "#!/bin/sh\necho 'Node is unavailable in this fixture' >&2\nexit 127\n", { mode: 0o755 });
    await writeFile(path.join(cwd, "package.json"), JSON.stringify({
      private: true, scripts: { lint: "oxlint --format=json ." },
    }));
    await copyFile(path.join(root, "oxlint.base.json"), path.join(directory, "oxlint.base.json"));
    await copyFile(path.join(root, "packages", workspace, ".oxlintrc.json"), path.join(cwd, ".oxlintrc.json"));
    await symlink(installed, path.join(cwd, "node_modules"), "dir");
    await writeFile(path.join(cwd, "tsconfig.json"), JSON.stringify({
      compilerOptions: {
        target: "ES2022", module: "ESNext", moduleResolution: "bundler",
        strict: true, noEmit: true, types: [], jsx: "react-jsx",
      },
      include: ["src"],
    }));
    for (const [name, code] of Object.entries(files)) {
      const destination = path.join(cwd, name);
      await mkdir(path.dirname(destination), { recursive: true });
      await writeFile(destination, code);
    }
    const result = Bun.spawnSync([
      process.execPath, "run", "--bun", "lint",
    ], {
      cwd,
      env: { ...process.env, PATH: [bin, process.env.PATH].join(path.delimiter) },
      stdout: "pipe", stderr: "pipe",
    });
    const output = result.stdout.toString();
    assert.notEqual(result.exitCode, null, result.stderr.toString());
    assert.ok(output, result.stderr.toString());
    let report;
    assert.doesNotThrow(() => { report = JSON.parse(output); }, output + result.stderr.toString());
    return { exitCode: result.exitCode, report, output };
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
}

it("keeps promise safety in backend source and undefined-name checks in helpers", async () => {
  const result = await lintFixture("backend", {
    "src/work.ts": "export function run() { Promise.resolve(1); }",
    "src/helpers/probe.js": "export function probe() { return missingHelper(); }",
  });
  assert.equal(result.exitCode, 1, result.output);
  const codes = result.report.diagnostics.map((diagnostic) => diagnostic.code);
  assert.ok(codes.includes("typescript(no-floating-promises)"), result.output);
  assert.ok(codes.includes("eslint(no-undef)"), result.output);

  const valid = await lintFixture("backend", {
    "src/work.ts": "export async function run() { await Promise.resolve(1); }",
    "test/work.test.ts": "Promise.resolve(1);",
    "src/helpers/probe.js": "export function probe() { return process.platform; }",
  });
  assert.equal(valid.exitCode, 0, valid.output);
  assert.deepEqual(valid.report.diagnostics, []);
});

it("keeps React hook ordering, effect dependencies, purity, and component exports", async () => {
  const result = await lintFixture("frontend", {
    "src/Order.tsx": `import { useState } from "react";
      export function Order({ enabled }: { enabled: boolean }) {
        if (enabled) useState(0);
        return <div />;
      }`,
    "src/Effect.tsx": `import { useEffect } from "react";
      export function Effect({ value }: { value: string }) {
        useEffect(() => { document.title = value; }, []);
        return <div />;
      }`,
    "src/Impure.tsx": "export function Impure() { return <div>{Math.random()}</div>; }",
    "src/Exports.tsx": "export const value = 1; export function Example() { return <div />; }",
  });
  assert.equal(result.exitCode, 1, result.output);
  const codes = result.report.diagnostics.map((diagnostic) => diagnostic.code);
  for (const code of [
    "react-hooks(rules-of-hooks)", "react-hooks(exhaustive-deps)",
    "react(purity)", "react(only-export-components)",
  ]) {
    assert.ok(codes.includes(code), result.output);
  }

  const valid = await lintFixture("frontend", {
    "src/Example.tsx": `import { useEffect, useState } from "react";
      export function Example({ value }: { value: string }) {
        const [count, setCount] = useState(0);
        useEffect(() => { document.title = value; }, [value]);
        return <button onClick={() => setCount(count + 1)}>{count}</button>;
      }`,
  });
  assert.equal(valid.exitCode, 0, valid.output);
  assert.deepEqual(valid.report.diagnostics, []);
});
