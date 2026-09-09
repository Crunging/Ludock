import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";
import { FILE_HELPER_SCRIPT } from "../src/file-helper-script.js";
import { extract } from "tar-stream";

// Run directly with Node24 in the Linux container as part of Docker acceptance.
// macOS has no /proc/self/fd and must not substitute weaker path semantics.
describe(
  "Linux file helper descriptor protections",
  { skip: process.platform !== "linux" },
  () => {
    let directory: string;
    let root: string;
    beforeEach(() => {
      directory = fs.mkdtempSync(path.join(os.tmpdir(), "ludock-file-helper-"));
      root = path.join(directory, "safe");
      fs.mkdirSync(root);
    });
    afterEach(() => fs.rmSync(directory, { recursive: true, force: true }));

    function run(
      operation: string,
      relative = "",
      extra: Record<string, unknown> = {},
      input?: string,
    ) {
      return spawnSync(
        process.execPath,
        [
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
        { input, encoding: "utf8", timeout: 10_000 },
      );
    }

    it("lists, reads, writes, renames, and deletes files without a shell or archive mutation", () => {
      assert.equal(run("mkdir", "world").status, 0);
      assert.equal(
        run("upload", "world/config.txt", { size: 12 }, "hello world!").status,
        0,
      );
      assert.equal(run("download", "world/config.txt").stdout, "hello world!");
      const listing = JSON.parse(run("list", "world").stdout) as Array<{
        name: string;
        type: string;
      }>;
      assert.deepEqual(
        listing.map(({ name, type }) => ({ name, type })),
        [{ name: "config.txt", type: "file" }],
      );
      assert.equal(
        run("rename", "world/config.txt", { destination: "world/renamed.txt" })
          .status,
        0,
      );
      assert.equal(run("download", "world/config.txt").status, 1);
      assert.equal(run("delete", "world").status, 0);
      assert.equal(fs.existsSync(path.join(root, "world")), false);
    });

    it("rejects root and intermediate symbolic links for both reads and writes", () => {
      const outside = path.join(directory, "outside");
      fs.mkdirSync(outside);
      fs.writeFileSync(path.join(outside, "canary"), "outside-secret");
      fs.symlinkSync(outside, path.join(root, "escape"));
      assert.equal(run("download", "escape/canary").status, 1);
      assert.equal(
        run("upload", "escape/canary", { size: 7 }, "changed").status,
        1,
      );
      assert.equal(run("list", "escape").status, 1);
      assert.equal(
        run("check", "", { root: path.join(root, "escape") }).status,
        1,
      );
      assert.equal(
        fs.readFileSync(path.join(outside, "canary"), "utf8"),
        "outside-secret",
      );
      assert.equal(
        run("delete", "escape").status,
        0,
        "deleting the link itself remains allowed",
      );
      assert.equal(fs.existsSync(path.join(outside, "canary")), true);
    });

    it("rejects downloads of hard links, symlinks, and directories containing special files", () => {
      fs.mkdirSync(path.join(root, "world"));
      fs.writeFileSync(path.join(root, "world", "file"), "contents");
      fs.linkSync(
        path.join(root, "world", "file"),
        path.join(root, "hardlink"),
      );
      assert.equal(run("download", "hardlink").status, 1);
      assert.equal(run("upload", "hardlink", { size: 1 }, "x").status, 1);
      assert.equal(
        fs.readFileSync(path.join(root, "world", "file"), "utf8"),
        "contents",
      );
      fs.unlinkSync(path.join(root, "hardlink"));
      assert.equal(
        spawnSync("mkfifo", [path.join(root, "world", "pipe")]).status,
        0,
      );
      assert.equal(run("download", "world").status, 1);
    });

    it("never crosses a blocked nested mount, including ancestor archive and mutations", () => {
      fs.mkdirSync(path.join(root, "world"));
      fs.mkdirSync(path.join(root, "world", "private"));
      const blocked = [path.join(root, "world", "private")];
      assert.deepEqual(
        JSON.parse(run("list", "world", { blocked }).stdout),
        [],
      );
      assert.equal(run("list", "world/private", { blocked }).status, 1);
      assert.equal(run("download", "world", { blocked }).status, 1);
      assert.equal(run("delete", "world", { blocked }).status, 1);
      assert.equal(
        run("rename", "world", { blocked, destination: "renamed" }).status,
        1,
      );
      assert.equal(fs.existsSync(path.join(root, "world", "private")), true);
    });

    it("produces a standard tar archive from pinned regular files", () => {
      fs.mkdirSync(path.join(root, "world"));
      fs.writeFileSync(path.join(root, "world", "config.txt"), "hello");
      const archive = spawnSync(process.execPath, [
        "-e",
        FILE_HELPER_SCRIPT,
        JSON.stringify({
          operation: "download",
          root,
          path: "world",
          blocked: [],
        }),
      ]);
      assert.equal(archive.status, 0);
      const contents = spawnSync("tar", ["-tf", "-"], {
        input: archive.stdout,
        encoding: "utf8",
      });
      assert.equal(contents.status, 0);
      assert.match(contents.stdout, /world\/config\.txt/);
    });

    it("preserves long Unicode archive paths using standard PAX records", () => {
      const name = "\u4e16\u754c".repeat(36) + ".txt";
      fs.mkdirSync(path.join(root, "world"));
      fs.writeFileSync(path.join(root, "world", name), "long path contents");
      const archive = spawnSync(process.execPath, [
        "-e",
        FILE_HELPER_SCRIPT,
        JSON.stringify({
          operation: "download",
          root,
          path: "world",
          blocked: [],
        }),
      ]);
      assert.equal(archive.status, 0);
      const contents = spawnSync("tar", ["-xOf", "-", `world/${name}`], {
        input: archive.stdout,
        encoding: "utf8",
      });
      assert.equal(contents.status, 0, contents.stderr);
      assert.equal(contents.stdout, "long path contents");
    });

    it("represents files larger than eight GiB without octal truncation", async () => {
      fs.mkdirSync(path.join(root, "world"));
      const filename = path.join(root, "world", "large");
      fs.writeFileSync(filename, "");
      const size = 9 * 1024 ** 3;
      fs.truncateSync(filename, size);
      const archive = spawn(
        process.execPath,
        [
          "-e",
          FILE_HELPER_SCRIPT,
          JSON.stringify({
            operation: "download",
            root,
            path: "world",
            blocked: [],
          }),
        ],
        { stdio: ["ignore", "pipe", "ignore"] },
      );
      let buffer = Buffer.alloc(0);
      const stopped = new Promise<void>((resolve) =>
        archive.once("exit", () => resolve()),
      );
      try {
        await new Promise<void>((resolve, reject) => {
          archive.once("error", reject);
          archive.stdout.on("data", (chunk: Buffer) => {
            buffer = Buffer.concat([buffer, chunk]);
            if (buffer.length >= 1024) {
              archive.stdout.pause();
              resolve();
            }
          });
        });
        const parser = extract();
        const parsed = new Promise<number>((resolve, reject) => {
          parser.once("error", reject);
          parser.on("entry", (header, stream, next) => {
            if (header.name === "world/large") resolve(header.size || 0);
            stream.resume();
            next();
          });
        });
        parser.write(buffer.subarray(0, 1024));
        try {
          assert.equal(await parsed, size);
        } finally {
          parser.destroy();
        }
      } finally {
        archive.kill("SIGKILL");
        await stopped;
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
      const racer = spawn(
        process.execPath,
        [
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
        { stdio: ["ignore", "pipe", "ignore"] },
      );
      await new Promise<void>((resolve) =>
        racer.stdout.once("data", () => resolve()),
      );
      try {
        for (let attempt = 0; attempt < 24; attempt++) {
          const result =
            attempt % 2
              ? run("upload", "folder/canary", { size: 6 }, "inside")
              : run("download", "folder/canary");
          assert.ok(result.status === 0 || result.status === 1);
          assert.doesNotMatch(result.stdout, /outside-secret/);
          assert.equal(
            fs.readFileSync(path.join(outside, "canary"), "utf8"),
            "outside-secret",
          );
        }
      } finally {
        const exited = new Promise<void>((resolve) =>
          racer.once("exit", () => resolve()),
        );
        racer.kill("SIGKILL");
        await exited;
      }
    });
  },
);
