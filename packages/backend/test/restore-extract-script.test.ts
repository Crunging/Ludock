import { fixtureBytes } from "./fixtures/bytes.js";
import { concatBytes, encodeText } from "../src/bytes.js";
import {
  mkdir,
  readFile,
  readdir,
  rm,
  stat,
  symlink,
} from "node:fs/promises";
import path from "node:path";
import { expect, describe, it } from "bun:test";
import { RESTORE_EXTRACT_SCRIPT } from "../src/helper-scripts.js";
import { type RestoreFixture, withRestoreFixture } from "./restore-test-fixture.js";

const stage = ".ludock-restore-11111111-1111-4111-8111-111111111111";
type ExtractionFixture = RestoreFixture & {
  destination: string;
  run: (
    input: Uint8Array,
    options?: {
      prelude?: string;
      preludeData?: Record<string, string>;
      maxBytes?: number;
      okay?: boolean;
    },
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
        preludeData: options.preludeData,
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
): Uint8Array {
  return concatBytes([
    fixtureBytes(JSON.stringify({
        name,
        type: "file",
        mode: 0o640,
        uid: process.getuid!(),
        gid: process.getgid!(),
        mtime: 1_700_000_000,
        size: encodeText(body).byteLength,
        ...options,
      }) + "\n"),
    fixtureBytes(body),
  ]);
}
describe.skipIf(process.platform !== "linux")(
  "descriptor-confined restore extraction",
  () => {
    it("restores nested binary data and empty directories with ownership, modes and times", extractionTest(async ({ destination, run, sentinel }) => {
      await run(
        concatBytes([
          record("settings", "", { type: "directory", mode: 0o750 }),
          record("settings/world", "hello\0world\n"),
          record("empty", "", { type: "directory" }),
          record("zero", ""),
        ]),
      );
      expect(await readFile(path.join(destination, "settings/world"), "utf8")).toBe("hello\0world\n");
      const info = await stat(path.join(destination, "settings/world"));
      expect(info.mode & 0o777).toBe(0o640);
      expect(info.uid).toBe(process.getuid!());
      expect(info.gid).toBe(process.getgid!());
      expect(info.mtimeMs).toBe(1_700_000_000_000);
      expect((await stat(path.join(destination, "settings"))).mode & 0o777).toBe(0o750);
      expect(await readdir(path.join(destination, "empty"))).toStrictEqual([]);
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
      await run(fixtureBytes('{"name":"incomplete'), { okay: false });
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
        concatBytes([
          record("duplicate", "first"),
          record("duplicate", "second"),
        ]),
        { okay: false },
      );
      expect(await readFile(path.join(destination, "duplicate"), "utf8")).toBe("first");
      await sentinel();
    }));
    it("pins a directory before an attacker substitutes a symlink during extraction", extractionTest(async ({ outside, destination, run, sentinel }) => {
      const victim = path.join(destination, "nested"),
        detached = path.join(destination, "detached");
      await mkdir(victim);
      const prelude = `
        const testFs = require("node:fs/promises");
        const testPaths = JSON.parse(process.env.LUDOCK_RESTORE_TEST_DATA);
        const originalOpen = testFs.open;
        let replaced = false;
        testFs.open = async function(filename, flags) {
          const handle = await originalOpen.call(this, filename, flags);
          if (!replaced && String(filename).endsWith("/nested")) {
            replaced = true;
            await testFs.rename(testPaths.victim, testPaths.detached);
            await testFs.symlink(testPaths.outside, testPaths.victim);
          }
          return handle;
        };
      `;
      await run(record("nested/sentinel", "staged-data"), {
        prelude,
        preludeData: { victim, detached, outside },
      });
      expect(await readFile(path.join(detached, "sentinel"), "utf8")).toBe("staged-data");
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
