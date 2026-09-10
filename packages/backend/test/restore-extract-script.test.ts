import assert from "node:assert/strict";
import {
  mkdtemp,
  mkdir,
  readFile,
  readdir,
  rm,
  stat,
  symlink,
  writeFile,
} from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { afterEach, beforeEach, describe, it } from "bun:test";
import { RESTORE_EXTRACT_SCRIPT } from "../src/restore-extract-script.js";

const stage = ".ludock-restore-11111111-1111-4111-8111-111111111111";
let directory: string, root: string, outside: string, destination: string;
beforeEach(async () => {
  directory = await mkdtemp(path.join(os.tmpdir(), "ludock-extract-helper-"));
  root = path.join(directory, "root");
  outside = path.join(directory, "outside");
  destination = path.join(root, stage, "new");
  await mkdir(destination, { recursive: true });
  await mkdir(outside);
  await writeFile(path.join(outside, "sentinel"), "outside-data");
});
afterEach(async () => {
  await rm(directory, { recursive: true, force: true });
});
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
function run(
  input: Buffer,
  options: { prelude?: string; maxBytes?: number; okay?: boolean } = {},
) {
  const result = Bun.spawnSync(
    [
      process.execPath,
      "-e",
      (options.prelude || "") + RESTORE_EXTRACT_SCRIPT,
      JSON.stringify({ root, stage, maxBytes: options.maxBytes ?? 1_000_000 }),
    ],
    { stdin: input, stdout: "pipe", stderr: "pipe", timeout: 10_000 },
  );
  if (options.okay === false)
    assert.notEqual(result.exitCode, 0, "Unsafe extraction succeeded");
  else assert.equal(result.exitCode, 0, result.stderr.toString());
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
  "descriptor-confined restore extraction",
    () => {
    it("restores nested binary data and empty directories with ownership, modes and times", async () => {
      run(
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
    });
    it("rejects unsafe records and leaves outside files untouched", async () => {
      for (const name of [
        "../outside/sentinel",
        "/outside/sentinel",
        "a/../../outside/sentinel",
        "a//world",
        "a\\world",
        ".ludock-restore-other/world",
      ])
        run(record(name, "changed"), { okay: false });
      run(record("link", "", { type: "symlink" }), { okay: false });
      run(record("huge", "bytes"), { maxBytes: 1, okay: false });
      run(record("negative", "", { size: -1 }), { okay: false });
      run(record("truncated", "short", { size: 100 }), { okay: false });
      run(Buffer.from('{"name":"incomplete'), { okay: false });
      await sentinel();
    });
    it("rejects file or directory symlinks and duplicate files in staging", async () => {
      await symlink(outside, path.join(destination, "escape"));
      await symlink(
        path.join(outside, "sentinel"),
        path.join(destination, "link"),
      );
      run(record("escape/sentinel", "changed"), { okay: false });
      run(record("link", "changed"), { okay: false });
      run(
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
    });
    it("pins a directory before an attacker substitutes a symlink during extraction", async () => {
      const victim = path.join(destination, "nested"),
        detached = path.join(destination, "detached");
      await mkdir(victim);
      const prelude = `const testFs=require("node:fs/promises");const originalOpen=testFs.open;let replaced=false;testFs.open=async function(filename,flags){const handle=await originalOpen.call(this,filename,flags);if(!replaced&&String(filename).endsWith("/nested")){replaced=true;await testFs.rename(${JSON.stringify(victim)},${JSON.stringify(detached)});await testFs.symlink(${JSON.stringify(outside)},${JSON.stringify(victim)});}return handle;};`;
      run(record("nested/sentinel", "staged-data"), { prelude });
      assert.equal(
        await readFile(path.join(detached, "sentinel"), "utf8"),
        "staged-data",
      );
      await sentinel();
    });
    it("rejects a symlink stage or new directory before writing records", async () => {
      await rm(destination, { recursive: true });
      await symlink(outside, destination);
      run(record("sentinel", "changed"), { okay: false });
      await sentinel();
      await rm(path.join(root, stage), { recursive: true });
      await symlink(outside, path.join(root, stage));
      run(record("sentinel", "changed"), { okay: false });
      await sentinel();
    });
  },
);
