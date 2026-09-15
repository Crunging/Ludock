import { copyFile, mkdir, mkdtemp, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { expect, it } from "bun:test";

const root = Bun.fileURLToPath(new URL("../../", import.meta.url));

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
    expect(result.exitCode, result.stderr.toString()).not.toBe(null);
    expect(output, result.stderr.toString()).toBeTruthy();
    let report;
    expect(() => { report = JSON.parse(output); }, output + result.stderr.toString()).not.toThrow();
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
  expect(result.exitCode, result.output).toBe(1);
  const codes = result.report.diagnostics.map((diagnostic) => diagnostic.code);
  expect(codes.includes("typescript(no-floating-promises)"), result.output).toBeTruthy();
  expect(codes.includes("eslint(no-undef)"), result.output).toBeTruthy();

  const valid = await lintFixture("backend", {
    "src/work.ts": "export async function run() { await Promise.resolve(1); }",
    "test/work.test.ts": "Promise.resolve(1);",
    "src/helpers/probe.js": "export function probe() { return process.platform; }",
  });
  expect(valid.exitCode, valid.output).toBe(0);
  expect(valid.report.diagnostics).toStrictEqual([]);
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
  expect(result.exitCode, result.output).toBe(1);
  const codes = result.report.diagnostics.map((diagnostic) => diagnostic.code);
  for (const code of [
    "react-hooks(rules-of-hooks)", "react-hooks(exhaustive-deps)",
    "react(purity)", "react(only-export-components)",
  ]) {
    expect(codes.includes(code), result.output).toBeTruthy();
  }

  const valid = await lintFixture("frontend", {
    "src/Example.tsx": `import { useEffect, useState } from "react";
      export function Example({ value }: { value: string }) {
        const [count, setCount] = useState(0);
        useEffect(() => { document.title = value; }, [value]);
        return <button onClick={() => setCount(count + 1)}>{count}</button>;
      }`,
  });
  expect(valid.exitCode, valid.output).toBe(0);
  expect(valid.report.diagnostics).toStrictEqual([]);
});
