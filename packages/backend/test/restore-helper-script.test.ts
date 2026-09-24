import {
  mkdir,
  readFile,
  readdir,
  rename,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import path from "node:path";
import { expect, describe, it } from "bun:test";
import { RESTORE_HELPER_SCRIPT } from "../src/helper-scripts.js";
import { type RestoreFixture, withRestoreFixture } from "./restore-test-fixture.js";

const stage = ".ludock-restore-11111111-1111-4111-8111-111111111111";
type RunOptions = {
  root?: string;
  stage?: string;
  prelude?: string;
  preludeData?: Record<string, string>;
  okay?: boolean;
};
type Fixture = RestoreFixture & {
  run: (
    operation: string,
    options?: RunOptions,
  ) => Promise<{ ok?: boolean; availableBytes?: number } | undefined>;
};

function restoreTest(name: string, body: (fixture: Fixture) => Promise<void>) {
  it(name, withRestoreFixture(async (fixture) => {
    const run: Fixture["run"] = async (operation, options = {}) => {
      const stdout = await fixture.runScript(
        (options.prelude || "") + RESTORE_HELPER_SCRIPT,
        { operation, root: options.root || fixture.root, stage: options.stage || stage },
        {
          preludeData: options.preludeData,
          rejection: options.okay === false
            ? "Restore data changed, is unsafe, or cannot be accessed. Keep the server stopped and review recovery."
            : undefined,
        },
      );
      return options.okay === false
        ? undefined
        : JSON.parse(stdout) as { ok?: boolean; availableBytes?: number };
    };
    await body({ ...fixture, run });
  }));
}

describe.skipIf(process.platform !== "linux")(
  "descriptor-confined restore helpers",
  () => {
    restoreTest("creates exclusive staging, moves dotfiles, and cleans committed old data", async ({ root, run, sentinel }) => {
      await writeFile(path.join(root, "world"), "old-world");
      await writeFile(path.join(root, ".settings"), "old-settings");
      expect((await run("space"))!.availableBytes! > 0).toBeTruthy();
      await run("stage");
      await run("stage", { okay: false });
      await writeFile(path.join(root, stage, "new", "world"), "restored-world");
      await run("moveOld");
      expect(await readFile(path.join(root, stage, "old", ".settings"), "utf8")).toBe("old-settings");
      await run("moveNew");
      expect(await readFile(path.join(root, "world"), "utf8")).toBe("restored-world");
      await run("cleanup");
      await run("cleanup");
      expect(await readdir(root)).toStrictEqual(["world"]);
      await sentinel();
    });
    restoreTest("rolls replacement back and safely resumes a partially completed rollback", async ({ root, run }) => {
      await writeFile(path.join(root, "one"), "old-one");
      await writeFile(path.join(root, "two"), "old-two");
      await run("stage");
      await writeFile(path.join(root, stage, "new", "one"), "replacement");
      await run("moveOld");
      await run("moveNew");
      await run("rollbackClean");
      await rename(
        path.join(root, stage, "old", "one"),
        path.join(root, "one"),
      );
      await run("rollbackOld");
      await run("rollbackOld");
      await run("cleanup");
      expect(await readFile(path.join(root, "one"), "utf8")).toBe("old-one");
      expect(await readFile(path.join(root, "two"), "utf8")).toBe("old-two");
    });
    restoreTest("refuses to overwrite an existing root entry during replacement or rollback", async ({ root, run }) => {
      await run("stage");
      await writeFile(path.join(root, "world"), "keep-me");
      await writeFile(path.join(root, stage, "new", "world"), "new");
      await run("moveNew", { okay: false });
      expect(await readFile(path.join(root, "world"), "utf8")).toBe("keep-me");
      await writeFile(path.join(root, stage, "old", "world"), "older");
      await run("rollbackOld", { okay: false });
      expect(await readFile(path.join(root, "world"), "utf8")).toBe("keep-me");
    });
    restoreTest("rejects symlink roots, ancestor traversal, and an unsafe stage name", async ({ directory, root, outside, run, sentinel }) => {
      const alias = path.join(directory, "alias");
      await symlink(outside, alias);
      await run("stage", { root: alias, okay: false });
      await run("stage", { root: `${alias}/child`, okay: false });
      await run("stage", { root: `${root}/../outside`, okay: false });
      await run("stage", { stage: "../../outside", okay: false });
      await sentinel();
    });
    restoreTest("rejects a substituted stage/new or stage/old directory before mutation", async ({ root, outside, run, sentinel }) => {
      await run("stage");
      await rm(path.join(root, stage, "new"), { recursive: true });
      await symlink(outside, path.join(root, stage, "new"));
      await run("moveNew", { okay: false });
      await rm(path.join(root, stage, "old"), { recursive: true });
      await symlink(outside, path.join(root, stage, "old"));
      await run("moveOld", { okay: false });
      await run("rollbackClean", { okay: false });
      await run("rollbackOld", { okay: false });
      await sentinel();
    });
    restoreTest("rejects a symlink stage and preserves other operations' staging data", async ({ root, outside, run, sentinel }) => {
      await symlink(outside, path.join(root, stage));
      await run("cleanup", { okay: false });
      await sentinel();
      await rm(path.join(root, stage));
      await run("stage");
      await mkdir(
        path.join(root, ".ludock-restore-22222222-2222-4222-8222-222222222222"),
      );
      await run("moveOld", { okay: false });
      expect((await readdir(root)).includes(
          ".ludock-restore-22222222-2222-4222-8222-222222222222",
        )).toBeTruthy();
    });
    restoreTest("cleanup unlinks nested symlinks without following their outside targets", async ({ root, outside, run, sentinel }) => {
      await run("stage");
      await symlink(outside, path.join(root, stage, "old", "escape"));
      await run("cleanup");
      await sentinel();
      expect(await readdir(root)).toStrictEqual([]);
    });
    restoreTest("pins the root before an ancestor is replaced with an outside symlink", async ({ directory, root, outside, run, sentinel }) => {
      await writeFile(path.join(root, "world"), "old-world");
      await run("stage");
      const moved = path.join(directory, "detached-root");
      const prelude = `
        const testFs = require("node:fs/promises");
        const testPaths = JSON.parse(process.env.LUDOCK_RESTORE_TEST_DATA);
        const originalOpen = testFs.open;
        let replaced = false;
        testFs.open = async function(filename, flags) {
          const handle = await originalOpen.call(this, filename, flags);
          if (!replaced && String(filename).endsWith("/" + testPaths.stage)) {
            replaced = true;
            await testFs.rename(testPaths.root, testPaths.moved);
            await testFs.symlink(testPaths.outside, testPaths.root);
          }
          return handle;
        };
      `;
      await run("moveOld", { prelude, preludeData: { root, moved, outside, stage } });
      await sentinel();
      expect(await readFile(path.join(moved, stage, "old", "world"), "utf8")).toBe("old-world");
      expect((await readdir(outside)).includes("world")).toBe(false);
    });
    restoreTest("pins recursive cleanup directories during a symlink substitution race", async ({ root, outside, run, sentinel }) => {
      await run("stage");
      const victim = path.join(root, stage, "old", "nested");
      await mkdir(victim);
      await writeFile(path.join(victim, "inside"), "inside");
      const moved = path.join(root, stage, "old", "detached");
      const prelude = `
        const testFs = require("node:fs/promises");
        const testPaths = JSON.parse(process.env.LUDOCK_RESTORE_TEST_DATA);
        const originalRead = testFs.readdir;
        let replaced = false;
        testFs.readdir = async function(filename, ...args) {
          const entries = await originalRead.call(this, filename, ...args);
          if (!replaced && entries.includes("inside")) {
            replaced = true;
            await testFs.rename(testPaths.victim, testPaths.moved);
            await testFs.symlink(testPaths.outside, testPaths.victim);
          }
          return entries;
        };
      `;
      await run("cleanup", { prelude, preludeData: { victim, moved, outside }, okay: false });
      await sentinel();
      expect(await readFile(path.join(outside, "sentinel"), "utf8")).toBe("outside-data");
    });
  },
);
