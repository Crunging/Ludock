import assert from "node:assert/strict";
import {
  mkdtemp,
  mkdir,
  readFile,
  readdir,
  rename,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { afterEach, beforeEach, describe, it } from "bun:test";
import { RESTORE_HELPER_SCRIPT } from "../src/restore-helper-script.js";

const stage = ".ludock-restore-11111111-1111-4111-8111-111111111111";
let directory: string, root: string, outside: string;
beforeEach(async () => {
  directory = await mkdtemp(path.join(os.tmpdir(), "ludock-restore-helper-"));
  root = path.join(directory, "root");
  outside = path.join(directory, "outside");
  await mkdir(root);
  await mkdir(outside);
  await writeFile(path.join(outside, "sentinel"), "outside-data");
});
afterEach(async () => {
  await rm(directory, { recursive: true, force: true });
});
function run(
  operation: string,
  options: {
    root?: string;
    stage?: string;
    prelude?: string;
    okay?: boolean;
  } = {},
) {
  const result = Bun.spawnSync(
    [
      process.execPath,
      "-e",
      (options.prelude || "") + RESTORE_HELPER_SCRIPT,
      JSON.stringify({
        operation,
        root: options.root || root,
        stage: options.stage || stage,
      }),
    ],
    { stdin: "ignore", stdout: "pipe", stderr: "pipe", timeout: 10_000 },
  );
  if (options.okay === false) {
    assert.notEqual(result.exitCode, 0, "Unsafe operation succeeded");
    return undefined;
  }
  assert.equal(result.exitCode, 0, result.stderr.toString());
  return JSON.parse(result.stdout.toString()) as { ok?: boolean; availableBytes?: number };
}
async function sentinel() {
  assert.equal(
    await readFile(path.join(outside, "sentinel"), "utf8"),
    "outside-data",
  );
}

describe.skipIf(Boolean(process.platform !== "linux"
        ? "Linux /proc/self/fd traversal runs in the Docker harness"
        : false))(
  "descriptor-confined restore helpers",
    () => {
    it("creates exclusive staging, moves dotfiles, and cleans committed old data", async () => {
      await writeFile(path.join(root, "world"), "old-world");
      await writeFile(path.join(root, ".settings"), "old-settings");
      assert.ok(run("space")!.availableBytes! > 0);
      run("stage");
      run("stage", { okay: false });
      await writeFile(path.join(root, stage, "new", "world"), "restored-world");
      run("moveOld");
      assert.equal(
        await readFile(path.join(root, stage, "old", ".settings"), "utf8"),
        "old-settings",
      );
      run("moveNew");
      assert.equal(
        await readFile(path.join(root, "world"), "utf8"),
        "restored-world",
      );
      run("cleanup");
      run("cleanup");
      assert.deepEqual(await readdir(root), ["world"]);
      await sentinel();
    });
    it("rolls replacement back and safely resumes a partially completed rollback", async () => {
      await writeFile(path.join(root, "one"), "old-one");
      await writeFile(path.join(root, "two"), "old-two");
      run("stage");
      await writeFile(path.join(root, stage, "new", "one"), "replacement");
      run("moveOld");
      run("moveNew");
      run("rollbackClean");
      await rename(
        path.join(root, stage, "old", "one"),
        path.join(root, "one"),
      );
      run("rollbackOld");
      run("rollbackOld");
      run("cleanup");
      assert.equal(await readFile(path.join(root, "one"), "utf8"), "old-one");
      assert.equal(await readFile(path.join(root, "two"), "utf8"), "old-two");
    });
    it("refuses to overwrite an existing root entry during replacement or rollback", async () => {
      run("stage");
      await writeFile(path.join(root, "world"), "keep-me");
      await writeFile(path.join(root, stage, "new", "world"), "new");
      run("moveNew", { okay: false });
      assert.equal(await readFile(path.join(root, "world"), "utf8"), "keep-me");
      await writeFile(path.join(root, stage, "old", "world"), "older");
      run("rollbackOld", { okay: false });
      assert.equal(await readFile(path.join(root, "world"), "utf8"), "keep-me");
    });
    it("rejects symlink roots, ancestor traversal, and an unsafe stage name", async () => {
      const alias = path.join(directory, "alias");
      await symlink(outside, alias);
      run("stage", { root: alias, okay: false });
      run("stage", { root: `${alias}/child`, okay: false });
      run("stage", { root: `${root}/../outside`, okay: false });
      run("stage", { stage: "../../outside", okay: false });
      await sentinel();
    });
    it("rejects a substituted stage/new or stage/old directory before mutation", async () => {
      run("stage");
      await rm(path.join(root, stage, "new"), { recursive: true });
      await symlink(outside, path.join(root, stage, "new"));
      run("moveNew", { okay: false });
      await rm(path.join(root, stage, "old"), { recursive: true });
      await symlink(outside, path.join(root, stage, "old"));
      run("moveOld", { okay: false });
      run("rollbackClean", { okay: false });
      run("rollbackOld", { okay: false });
      await sentinel();
    });
    it("rejects a symlink stage and preserves other operations' staging data", async () => {
      await symlink(outside, path.join(root, stage));
      run("cleanup", { okay: false });
      await sentinel();
      await rm(path.join(root, stage));
      run("stage");
      await mkdir(
        path.join(root, ".ludock-restore-22222222-2222-4222-8222-222222222222"),
      );
      run("moveOld", { okay: false });
      assert.ok(
        (await readdir(root)).includes(
          ".ludock-restore-22222222-2222-4222-8222-222222222222",
        ),
      );
    });
    it("cleanup unlinks nested symlinks without following their outside targets", async () => {
      run("stage");
      await symlink(outside, path.join(root, stage, "old", "escape"));
      run("cleanup");
      await sentinel();
      assert.deepEqual(await readdir(root), []);
    });
    it("pins the root before an ancestor is replaced with an outside symlink", async () => {
      await writeFile(path.join(root, "world"), "old-world");
      run("stage");
      const moved = path.join(directory, "detached-root");
      const prelude = `const testFs = require("node:fs/promises"); const originalOpen = testFs.open; let replaced = false; testFs.open = async function(filename, flags) { const handle = await originalOpen.call(this, filename, flags); if (!replaced && String(filename).endsWith("/${stage}")) { replaced=true; await testFs.rename(${JSON.stringify(root)}, ${JSON.stringify(moved)}); await testFs.symlink(${JSON.stringify(outside)}, ${JSON.stringify(root)}); } return handle; };`;
      run("moveOld", { prelude });
      await sentinel();
      assert.equal(
        await readFile(path.join(moved, stage, "old", "world"), "utf8"),
        "old-world",
      );
      assert.equal((await readdir(outside)).includes("world"), false);
    });
    it("pins recursive cleanup directories during a symlink substitution race", async () => {
      run("stage");
      const victim = path.join(root, stage, "old", "nested");
      await mkdir(victim);
      await writeFile(path.join(victim, "inside"), "inside");
      const moved = path.join(root, stage, "old", "detached");
      const prelude = `const testFs = require("node:fs/promises"); const originalRead = testFs.readdir; let replaced = false; testFs.readdir = async function(filename, ...args) { const entries = await originalRead.call(this, filename, ...args); if (!replaced && entries.includes("inside")) { replaced=true; await testFs.rename(${JSON.stringify(victim)}, ${JSON.stringify(moved)}); await testFs.symlink(${JSON.stringify(outside)}, ${JSON.stringify(victim)}); } return entries; };`;
      run("cleanup", { prelude, okay: false });
      await sentinel();
      assert.equal(
        await readFile(path.join(outside, "sentinel"), "utf8"),
        "outside-data",
      );
    });
  },
);
