import { fixtureBytes } from "./fixtures/bytes.js";
import { concatBytes, decodeText, encodeText } from "../src/bytes.js";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { expect, afterEach, beforeEach, describe, it } from "bun:test";
import { FILE_HELPER_SCRIPT } from "../src/helper-scripts.js";
import { decodeTarHeader } from "../src/tar.js";

// Run directly with Bun in the Linux container as part of Docker acceptance.
// macOS has no /proc/self/fd and must not substitute weaker path semantics.
describe.skipIf(Boolean(process.platform !== "linux"))(
  "Linux file helper descriptor protections",
    () => {
    let directory: string;
    let root: string;
    beforeEach(() => {
      directory = fs.mkdtempSync(path.join(os.tmpdir(), "ludock-file-helper-"));
      root = path.join(directory, "safe");
      fs.mkdirSync(root);
    });
    afterEach(() => fs.rmSync(directory, { recursive: true, force: true }));

    // Keep pipe I/O and child exit handling on the event loop. Repeated
    // spawnSync calls can stall under Bun's isolated Linux test runner.
    async function runProcess(command: string[], input?: Uint8Array) {
      const child = Bun.spawn(command, {
        stdin: input ?? "ignore",
        stdout: "pipe",
        stderr: "pipe",
        timeout: 10_000,
      });
      try {
        const [stdout, stderr, exitCode] = await Promise.all([
          new Response(child.stdout).arrayBuffer(),
          new Response(child.stderr).text(),
          child.exited,
        ]);
        return { exitCode, stdout: fixtureBytes(stdout), stderr };
      } finally {
        if (child.exitCode === null) {
          child.kill("SIGKILL");
          await child.exited;
        }
      }
    }

    async function run(
      operation: string,
      relative = "",
      extra: Record<string, unknown> = {},
      input?: string,
    ) {
      const result = await runProcess(
        [
          process.execPath,
          "-e",
          FILE_HELPER_SCRIPT,
          JSON.stringify({
            operation,
            root,
            path: relative,
            blocked: [],
            ...extra,
          }),
        ],
        input === undefined ? undefined : fixtureBytes(input),
      );
      return { ...result, stdout: decodeText(result.stdout) };
    }

    it("lists, reads, writes, renames, and deletes files without a shell or archive mutation", async () => {
      expect((await run("mkdir", "world")).exitCode).toBe(0);
      expect((await run("upload", "world/config.txt", { size: 12 }, "hello world!")).exitCode).toBe(0);
      expect((await run("download", "world/config.txt")).stdout).toBe("hello world!");
      const listing = JSON.parse((await run("list", "world")).stdout) as Array<{
        name: string;
        type: string;
      }>;
      expect(listing.map(({ name, type }) => ({ name, type }))).toStrictEqual([{ name: "config.txt", type: "file" }]);
      expect((await run("rename", "world/config.txt", { destination: "world/renamed.txt" }))
          .exitCode).toBe(0);
      expect((await run("download", "world/config.txt")).exitCode).toBe(1);
      expect((await run("delete", "world")).exitCode).toBe(0);
      expect(fs.existsSync(path.join(root, "world"))).toBe(false);
    });

    it("leaves existing files intact and new files absent after incomplete or invalid uploads", async () => {
      const existing = path.join(root, "world.cfg");
      fs.writeFileSync(existing, "original-world-settings");
      for (const relative of ["world.cfg", "new.cfg"]) {
        for (const input of [
          { size: 10, body: "short" },
          { size: 2, body: "too much" },
          { size: -1, body: "" },
          { size: 1.5, body: "x" },
        ]) {
          const result = await run("upload", relative, { size: input.size }, input.body);
          expect(result.exitCode, `${relative}: ${JSON.stringify(input)}`).toBe(1);
          expect(fs.readFileSync(existing, "utf8")).toBe("original-world-settings");
          expect(fs.existsSync(path.join(root, "new.cfg"))).toBe(false);
          expect(fs.readdirSync(root), "Failed uploads must remove their temporary files").toStrictEqual(["world.cfg"]);
        }
      }
    });

    it("replaces complete uploads while preserving existing file ownership and permissions", async () => {
      const filename = path.join(root, "world.cfg");
      fs.writeFileSync(filename, "old-settings");
      if (process.getuid!() === 0) fs.chownSync(filename, 1234, 2345);
      fs.chmodSync(filename, 0o640);
      const original = fs.statSync(filename);
      for (const body of ["new\0settings\n", ""]) {
        const result = await run("upload", "world.cfg", { size: encodeText(body).byteLength }, body);
        expect(result.exitCode, result.stderr).toBe(0);
        expect(fs.readFileSync(filename, "utf8")).toBe(body);
        const replaced = fs.statSync(filename);
        expect(replaced.mode & 0o777).toBe(0o640);
        expect(replaced.uid).toBe(original.uid);
        expect(replaced.gid).toBe(original.gid);
        expect(fs.readdirSync(root)).toStrictEqual(["world.cfg"]);
      }
    });

    it("refuses to replace a destination symlink during upload", async () => {
      const outside = path.join(directory, "outside.cfg");
      const link = path.join(root, "world.cfg");
      fs.writeFileSync(outside, "outside-settings");
      fs.symlinkSync(outside, link);
      expect((await run("upload", "world.cfg", { size: 3 }, "new")).exitCode).toBe(1);
      expect(fs.lstatSync(link).isSymbolicLink()).toBe(true);
      expect(fs.readFileSync(outside, "utf8")).toBe("outside-settings");
      expect(fs.readdirSync(root)).toStrictEqual(["world.cfg"]);
    });

    it("commits a production upload only after its complete payload and matching completion token", async () => {
      const filename = path.join(root, "world.cfg");
      const uploadId = "11111111-1111-4111-8111-111111111111";
      const anotherId = "22222222-2222-4222-8222-222222222222";
      const body = "new-settings";
      fs.writeFileSync(filename, "old-settings");
      for (const input of [body, body + anotherId, body + uploadId + "extra"]) {
        const result = await run("upload", "world.cfg", { size: body.length, uploadId }, input);
        expect(result.exitCode).toBe(1);
        expect(fs.readFileSync(filename, "utf8")).toBe("old-settings");
        expect(fs.readdirSync(root)).toStrictEqual(["world.cfg"]);
      }
      const committed = await run("upload", "world.cfg", { size: body.length, uploadId }, body + uploadId);
      expect(committed.exitCode, committed.stderr).toBe(0);
      expect(fs.readFileSync(filename, "utf8")).toBe(body);
      expect(fs.readdirSync(root)).toStrictEqual(["world.cfg"]);
    });

    it("cleans up only the selected upload's temporary sibling within its approved parent", async () => {
      const uploadId = "11111111-1111-4111-8111-111111111111";
      const otherId = "22222222-2222-4222-8222-222222222222";
      const temporary = `.ludock-upload-${uploadId}.tmp`;
      const otherTemporary = `.ludock-upload-${otherId}.tmp`;
      const outside = path.join(directory, "outside");
      fs.mkdirSync(outside);
      fs.mkdirSync(path.join(root, "world"));
      fs.writeFileSync(path.join(root, "world", "settings.cfg"), "original-settings");
      fs.writeFileSync(path.join(root, "world", temporary), "partial-upload");
      fs.writeFileSync(path.join(root, "world", otherTemporary), "other-upload");
      fs.writeFileSync(path.join(outside, temporary), "outside-data");
      fs.symlinkSync(outside, path.join(root, "escape"));
      expect((await run("upload-cleanup", "world/settings.cfg", { uploadId: "../escape" })).exitCode).toBe(1);
      expect((await run("upload-cleanup", "escape/settings.cfg", { uploadId })).exitCode).toBe(1);
      expect(fs.readFileSync(path.join(outside, temporary), "utf8")).toBe("outside-data");
      const result = await run("upload-cleanup", "world/settings.cfg", { uploadId });
      expect(result.exitCode, result.stderr).toBe(0);
      expect(fs.existsSync(path.join(root, "world", temporary))).toBe(false);
      expect(fs.readFileSync(path.join(root, "world", otherTemporary), "utf8")).toBe("other-upload");
      expect(fs.readFileSync(path.join(root, "world", "settings.cfg"), "utf8")).toBe("original-settings");
    });

    it("refuses uploads when their destination is created or replaced before commit", async () => {
      const filename = path.join(root, "world.cfg");
      for (const existing of [true, false]) {
        if (existing) fs.writeFileSync(filename, "original-settings");
        const uploadId = crypto.randomUUID();
        const temporary = path.join(root, `.ludock-upload-${uploadId}.tmp`);
        const upload = Bun.spawn([
          process.execPath, "-e", FILE_HELPER_SCRIPT,
          JSON.stringify({ operation: "upload", root, path: "world.cfg", size: 12, uploadId, blocked: [] }),
        ], { stdin: "pipe", stdout: "pipe", stderr: "pipe" });
        try {
          upload.stdin.write("new-");
          await upload.stdin.flush();
          for (let attempt = 0; attempt < 200 && !fs.existsSync(temporary); attempt++) {
            expect(upload.exitCode, "Upload exited before creating temporary storage").toBe(null);
            await Bun.sleep(5);
          }
          expect(fs.existsSync(temporary), "Upload must prepare its temporary file before the target changes").toBe(true);
          if (existing) fs.renameSync(filename, path.join(root, "previous.cfg"));
          fs.writeFileSync(filename, "external-replacement");
          upload.stdin.write("settings" + uploadId);
          upload.stdin.end();
          expect(await upload.exited).toBe(1);
          expect(fs.readFileSync(filename, "utf8")).toBe("external-replacement");
          expect(fs.existsSync(temporary)).toBe(false);
        } finally {
          if (upload.exitCode === null) {
            upload.kill("SIGKILL");
            await upload.exited;
          }
        }
        fs.unlinkSync(filename);
      }
      expect(fs.readFileSync(path.join(root, "previous.cfg"), "utf8")).toBe("original-settings");
    });

    it("rejects root and intermediate symbolic links for both reads and writes", async () => {
      const outside = path.join(directory, "outside");
      fs.mkdirSync(outside);
      fs.writeFileSync(path.join(outside, "canary"), "outside-secret");
      fs.symlinkSync(outside, path.join(root, "escape"));
      expect((await run("download", "escape/canary")).exitCode).toBe(1);
      expect((await run("upload", "escape/canary", { size: 7 }, "changed")).exitCode).toBe(1);
      expect((await run("list", "escape")).exitCode).toBe(1);
      expect((await run("check", "", { root: path.join(root, "escape") })).exitCode).toBe(1);
      expect(fs.readFileSync(path.join(outside, "canary"), "utf8")).toBe("outside-secret");
      expect((await run("delete", "escape")).exitCode, "deleting the link itself remains allowed").toBe(0);
      expect(fs.existsSync(path.join(outside, "canary"))).toBe(true);
    });

    it("rejects downloads of hard links, symlinks, and directories containing special files", async () => {
      fs.mkdirSync(path.join(root, "world"));
      fs.writeFileSync(path.join(root, "world", "file"), "contents");
      fs.linkSync(
        path.join(root, "world", "file"),
        path.join(root, "hardlink"),
      );
      expect((await run("download", "hardlink")).exitCode).toBe(1);
      expect((await run("upload", "hardlink", { size: 1 }, "x")).exitCode).toBe(1);
      expect(fs.readFileSync(path.join(root, "world", "file"), "utf8")).toBe("contents");
      fs.unlinkSync(path.join(root, "hardlink"));
      expect((await runProcess(["mkfifo", path.join(root, "world", "pipe")])).exitCode).toBe(0);
      expect((await run("download", "world")).exitCode).toBe(1);
    });

    it("never crosses a blocked nested mount, including ancestor archive and mutations", async () => {
      fs.mkdirSync(path.join(root, "world"));
      fs.mkdirSync(path.join(root, "world", "private"));
      const blocked = [path.join(root, "world", "private")];
      expect(JSON.parse((await run("list", "world", { blocked })).stdout)).toStrictEqual([]);
      expect((await run("list", "world/private", { blocked })).exitCode).toBe(1);
      expect((await run("download", "world", { blocked })).exitCode).toBe(1);
      expect((await run("delete", "world", { blocked })).exitCode).toBe(1);
      expect((await run("rename", "world", { blocked, destination: "renamed" })).exitCode).toBe(1);
      expect(fs.existsSync(path.join(root, "world", "private"))).toBe(true);
    });

    it("produces a standard tar archive from pinned regular files", async () => {
      fs.mkdirSync(path.join(root, "world"));
      fs.writeFileSync(path.join(root, "world", "config.txt"), "hello");
      const archive = await runProcess([
        process.execPath,
        "-e",
        FILE_HELPER_SCRIPT,
        JSON.stringify({
          operation: "download",
          root,
          path: "world",
          blocked: [],
        }),
      ]);
      expect(archive.exitCode).toBe(0);
      const contents = await runProcess(["tar", "-tf", "-"], archive.stdout);
      expect(contents.exitCode).toBe(0);
      expect(decodeText(contents.stdout)).toMatch(/world\/config\.txt/);
    });

    it("preserves long Unicode archive paths using standard PAX records", async () => {
      const name = "\u4e16\u754c".repeat(36) + ".txt";
      fs.mkdirSync(path.join(root, "world"));
      fs.writeFileSync(path.join(root, "world", name), "long path contents");
      const archive = await runProcess([
        process.execPath,
        "-e",
        FILE_HELPER_SCRIPT,
        JSON.stringify({
          operation: "download",
          root,
          path: "world",
          blocked: [],
        }),
      ]);
      expect(archive.exitCode).toBe(0);
      const contents = await runProcess(["tar", "-xOf", "-", `world/${name}`], archive.stdout);
      expect(contents.exitCode, contents.stderr).toBe(0);
      expect(decodeText(contents.stdout)).toBe("long path contents");
    });

    it("represents files larger than eight GiB without octal truncation", async () => {
      fs.mkdirSync(path.join(root, "world"));
      const filename = path.join(root, "world", "large");
      fs.writeFileSync(filename, "");
      const size = 9 * 1024 ** 3;
      fs.truncateSync(filename, size);
      const archive = Bun.spawn(
        [
          process.execPath,
          "-e",
          FILE_HELPER_SCRIPT,
          JSON.stringify({
            operation: "download",
            root,
            path: "world",
            blocked: [],
          }),
        ],
        { stdin: "ignore", stdout: "pipe", stderr: "ignore" },
      );
      let buffer = new Uint8Array(0);
      const reader = archive.stdout.getReader();
      try {
        while (buffer.length < 1024) {
          const { done, value } = await reader.read();
          expect(done, "Archive ended before its PAX size header").toBe(false);
          buffer = concatBytes([buffer, value!]);
        }
        const header = decodeTarHeader(buffer.subarray(512, 1024));
        expect(header.name).toBe("world/large");
        expect(header.size).toBe(size);
      } finally {
        archive.kill("SIGKILL");
        await archive.exited;
        await reader.cancel();
        reader.releaseLock();
      }
    });

    it("pins parents while an external process continuously swaps them for an escape link", async () => {
      const folder = path.join(root, "folder");
      const parked = path.join(root, "parked");
      const outside = path.join(directory, "outside");
      fs.mkdirSync(folder);
      fs.mkdirSync(outside);
      fs.writeFileSync(path.join(folder, "canary"), "inside");
      fs.writeFileSync(path.join(outside, "canary"), "outside-secret");
      const racer = Bun.spawn(
        [
          process.execPath,
          "-e",
          `
      const fs = require("node:fs"); const [folder,parked,outside] = process.argv.slice(1);
      process.stdout.write("ready");
      for (;;) {
        try { fs.renameSync(folder, parked); fs.symlinkSync(outside, folder); fs.unlinkSync(folder); fs.renameSync(parked, folder); } catch {}
      }
    `,
          folder,
          parked,
          outside,
        ],
        { stdin: "ignore", stdout: "pipe", stderr: "ignore" },
      );
      const reader = racer.stdout.getReader();
      try {
        const { done, value } = await reader.read();
        expect(done, "Symlink racer exited before becoming ready").toBe(false);
        expect(decodeText(fixtureBytes(value!))).toBe("ready");
        for (let attempt = 0; attempt < 24; attempt++) {
          const result =
            attempt % 2
              ? await run("upload", "folder/canary", { size: 6 }, "inside")
              : await run("download", "folder/canary");
          expect(result.exitCode === 0 || result.exitCode === 1).toBeTruthy();
          expect(result.stdout).not.toMatch(/outside-secret/);
          expect(fs.readFileSync(path.join(outside, "canary"), "utf8")).toBe("outside-secret");
        }
      } finally {
        racer.kill("SIGKILL");
        await racer.exited;
        await reader.cancel();
        reader.releaseLock();
      }
    }, 60_000);
  },
);
