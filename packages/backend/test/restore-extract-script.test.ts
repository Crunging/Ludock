import assert from "node:assert/strict";
import {
  mkdir,
  readFile,
  readdir,
  rm,
  stat,
  symlink,
} from "node:fs/promises";
import path from "node:path";
import { describe, it } from "bun:test";
import { RESTORE_EXTRACT_SCRIPT } from "../src/restore-extract-script.js";
import { type RestoreFixture, withRestoreFixture } from "./restore-test-fixture.js";

const stage = ".ludock-restore-11111111-1111-4111-8111-111111111111";
type ExtractionFixture = RestoreFixture & {
  destination: string;
  run: (
    input: Buffer,
    options?: { prelude?: string; maxBytes?: number; okay?: boolean },
  ) => Promise<string>;
};

function extractionTest(body: (fixture: ExtractionFixture) => Promise<void>) {
  return withRestoreFixture(async (fixture) => {
    const destination = path.join(fixture.root, stage, "new");
    await mkdir(destination, { recursive: true });
    const run: ExtractionFixture["run"] = (input, options = {}) => fixture.runScript(
      (options.prelude || "") + RESTORE_EXTRACT_SCRIPT,
      { root: fixture.root, stage, maxBytes: options.maxBytes ?? 1_000_000 },
      {
        input,
        rejection: options.okay === false
          ? "Restore extraction failed: the archive or destination changed or is unsafe."
          : undefined,
      },
    );
    await body({ ...fixture, destination, run });
  });
}

function record(
  name: string,
  body: string,
  options: Record<string, unknown> = {},
): Buffer {
  return Buffer.concat([
    Buffer.from(
      JSON.stringify({
        name,
        type: "file",
        mode: 0o640,
        uid: process.getuid!(),
        gid: process.getgid!(),
        mtime: 1_700_000_000,
        size: Buffer.byteLength(body),
        ...options,
      }) + "\n",
    ),
    Buffer.from(body),
  ]);
}
describe.skipIf(process.platform !== "linux")(
  "descriptor-confined restore extraction",
  () => {
    it("restores nested binary data and empty directories with ownership, modes and times", extractionTest(async ({ destination, run, sentinel }) => {
      await run(
        Buffer.concat([
          record("settings", "", { type: "directory", mode: 0o750 }),
          record("settings/world", "hello\0world\n"),
          record("empty", "", { type: "directory" }),
          record("zero", ""),
        ]),
      );
      assert.equal(
        await readFile(path.join(destination, "settings/world"), "utf8"),
        "hello\0world\n",
      );
      const info = await stat(path.join(destination, "settings/world"));
      assert.equal(info.mode & 0o777, 0o640);
      assert.equal(info.uid, process.getuid!());
      assert.equal(info.gid, process.getgid!());
      assert.equal(info.mtimeMs, 1_700_000_000_000);
      assert.equal(
        (await stat(path.join(destination, "settings"))).mode & 0o777,
        0o750,
      );
      assert.deepEqual(await readdir(path.join(destination, "empty")), []);
      await sentinel();
    }));
    it("rejects unsafe record paths and leaves outside files untouched", extractionTest(async ({ run, sentinel }) => {
      for (const name of [
        "../outside/sentinel",
        "/outside/sentinel",
        "a/../../outside/sentinel",
        "a//world",
        "a\\world",
        ".ludock-restore-other/world",
      ])
        await run(record(name, "changed"), { okay: false });
      await sentinel();
    }));
    it("rejects unsupported, malformed, oversized, and truncated records without changing outside files", extractionTest(async ({ run, sentinel }) => {
      await run(record("link", "", { type: "symlink" }), { okay: false });
      await run(record("huge", "bytes"), { maxBytes: 1, okay: false });
      await run(record("negative", "", { size: -1 }), { okay: false });
      await run(record("truncated", "short", { size: 100 }), { okay: false });
      await run(Buffer.from('{"name":"incomplete'), { okay: false });
      await sentinel();
    }));
    it("rejects file or directory symlinks and duplicate files in staging", extractionTest(async ({ outside, destination, run, sentinel }) => {
      await symlink(outside, path.join(destination, "escape"));
      await symlink(
        path.join(outside, "sentinel"),
        path.join(destination, "link"),
      );
      await run(record("escape/sentinel", "changed"), { okay: false });
      await run(record("link", "changed"), { okay: false });
      await run(
        Buffer.concat([
          record("duplicate", "first"),
          record("duplicate", "second"),
        ]),
        { okay: false },
      );
      assert.equal(
        await readFile(path.join(destination, "duplicate"), "utf8"),
        "first",
      );
      await sentinel();
    }));
    it("pins a directory before an attacker substitutes a symlink during extraction", extractionTest(async ({ outside, destination, run, sentinel }) => {
      const victim = path.join(destination, "nested"),
        detached = path.join(destination, "detached");
      await mkdir(victim);
      const prelude = `const testFs=require("node:fs/promises");const originalOpen=testFs.open;let replaced=false;testFs.open=async function(filename,flags){const handle=await originalOpen.call(this,filename,flags);if(!replaced&&String(filename).endsWith("/nested")){replaced=true;await testFs.rename(${JSON.stringify(victim)},${JSON.stringify(detached)});await testFs.symlink(${JSON.stringify(outside)},${JSON.stringify(victim)});}return handle;};`;
      await run(record("nested/sentinel", "staged-data"), { prelude });
      assert.equal(
        await readFile(path.join(detached, "sentinel"), "utf8"),
        "staged-data",
      );
      await sentinel();
    }));
    it("rejects a symlink stage or new directory before writing records", extractionTest(async ({ root, outside, destination, run, sentinel }) => {
      await rm(destination, { recursive: true });
      await symlink(outside, destination);
      await run(record("sentinel", "changed"), { okay: false });
      await sentinel();
      await rm(path.join(root, stage), { recursive: true });
      await symlink(outside, path.join(root, stage));
      await run(record("sentinel", "changed"), { okay: false });
      await sentinel();
    }));
  },
);
