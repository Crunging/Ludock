import assert from "node:assert/strict";
import {
  mkdtemp,
  rm,
  mkdir,
  symlink,
  writeFile,
  realpath,
} from "node:fs/promises";
import path from "node:path";
import { tmpdir } from "node:os";
import { Duplex, Readable } from "node:stream";
import { pipeline } from "node:stream/promises";
import { afterEach, beforeEach, describe, it } from "node:test";
import * as tar from "tar-stream";
import { createHash, randomUUID } from "node:crypto";
import type Docker from "dockerode";
import {
  archiveEntryMetadata,
  mappedArchiveHeader,
  approvedBackupDirectory,
  archiveValidator,
  backupFilePath,
  extractRootToStage,
  helperExec,
  validateArchive,
  validateArchiveEntry,
} from "../src/backup-storage.js";

const roots = [{ id: "root-0", path: "/data" }];
let directory: string;
let oldRoots: string | undefined;
beforeEach(async () => {
  directory = await realpath(
    await mkdtemp(path.join(tmpdir(), "ludock-backup-test-")),
  );
  oldRoots = process.env.LUDOCK_BACKUP_ROOTS;
  process.env.LUDOCK_BACKUP_ROOTS = directory;
});
afterEach(async () => {
  if (oldRoots === undefined) delete process.env.LUDOCK_BACKUP_ROOTS;
  else process.env.LUDOCK_BACKUP_ROOTS = oldRoots;
  await rm(directory, { recursive: true, force: true });
});
async function archive(
  entries: Array<{ header: tar.Headers; body?: string }>,
): Promise<Buffer> {
  const pack = tar.pack(),
    chunks: Buffer[] = [];
  pack.on("data", (chunk: Buffer) => chunks.push(chunk));
  for (const { header, body } of entries)
    await new Promise<void>((resolve, reject) =>
      pack.entry(header, body || "", (error) =>
        error ? reject(error) : resolve(),
      ),
    );
  const done = new Promise<Buffer>((resolve, reject) => {
    pack.once("end", () => resolve(Buffer.concat(chunks)));
    pack.once("error", reject);
  });
  pack.finalize();
  return done;
}

describe("backup storage boundaries", () => {
  it("rechecks access after helper exec preparation before starting a restore", async () => {
    let allowed = true;
    let starts = 0;
    const helper = {
      exec: async () => {
        allowed = false;
        return { start: async () => { starts++; } };
      },
    } as unknown as Docker.Container;
    const assertAccess = () => {
      if (!allowed) throw new Error("Access revoked during preparation");
    };
    await assert.rejects(
      helperExec(helper, ["node", "restore-helper"], {}, assertAccess),
      /Access revoked/,
    );
    allowed = true;
    await assert.rejects(
      extractRootToStage(
        directory, randomUUID(), helper, roots[0], "/data",
        `.ludock-restore-${randomUUID()}`, roots, 10000, "checksum",
        assertAccess,
      ),
      /Access revoked/,
    );
    assert.equal(starts, 0);
  });
  it(
    "stops streaming restore data when access changes after extraction starts",
    { skip: process.platform !== "linux" },
    async () => {
      const bytes = await archive([
        {
          header: { name: "snapshot/root-0/world.txt", type: "file", size: 5 },
          body: "hello",
        },
      ]);
      const id = randomUUID();
      await writeFile(await backupFilePath(directory, id), bytes);
      let allowed = true;
      const writes: Buffer[] = [];
      const socket = new Duplex({
        read() {},
        write(chunk: Buffer, _encoding, callback) {
          writes.push(Buffer.from(chunk));
          allowed = false;
          callback();
        },
        final(callback) {
          this.push(null);
          callback();
        },
      });
      const helper = {
        exec: async () => ({
          start: async () => socket,
          inspect: async () => ({ ExitCode: 0 }),
        }),
      } as unknown as Docker.Container;
      await assert.rejects(
        extractRootToStage(
          directory, id, helper, roots[0], "/data",
          `.ludock-restore-${randomUUID()}`, roots, 10000,
          createHash("sha256").update(bytes).digest("hex"),
          () => {
            if (!allowed) throw new Error("Access revoked during streaming");
          },
        ),
        /Access revoked/,
      );
      assert.equal(writes.length, 1);
      assert.match(writes[0].toString(), /world\.txt/);
      assert.equal(socket.destroyed, true);
    },
  );
  it("preserves large Linux IDs and future timestamps through tar repacking without source path overrides", async () => {
    const source = {
      name: "source/world",
      type: "file" as const,
      size: 5,
      uid: 1_000_000,
      gid: 2_000_000,
      mtime: new Date("2040-01-01T00:00:00Z"),
      pax: { path: "source/elsewhere" },
    };
    const mapped = mappedArchiveHeader(source, "snapshot/root-0/world");
    const bytes = await archive([{ header: mapped, body: "hello" }]);
    const extract = tar.extract();
    let found = false;
    extract.on("entry", (header, stream, next) => {
      assert.equal(header.name, "snapshot/root-0/world");
      assert.deepEqual(archiveEntryMetadata(header), {
        uid: 1_000_000,
        gid: 2_000_000,
        mtime: source.mtime.getTime() / 1000,
      });
      found = true;
      stream.resume();
      stream.once("end", next);
    });
    await pipeline(Readable.from(bytes), extract);
    assert.equal(found, true);
    for (const metadata of [
      { uid: "4294967295" },
      { gid: "-1" },
      { mtime: "Infinity" },
    ])
      assert.throws(() => archiveEntryMetadata({ ...source, pax: metadata }));
  });

  it("uses only existing approved directories and rejects symlink traversal", async () => {
    await mkdir(path.join(directory, "archives"));
    assert.equal(
      await approvedBackupDirectory(path.join(directory, "archives")),
      path.join(directory, "archives"),
    );
    await symlink(
      path.join(directory, "archives"),
      path.join(directory, "shortcut"),
    );
    await assert.rejects(
      approvedBackupDirectory(path.join(directory, "shortcut")),
      /symlink|symbolic/,
    );
    await assert.rejects(
      approvedBackupDirectory(path.dirname(directory)),
      /inside/,
    );
    await assert.rejects(approvedBackupDirectory("relative/path"), /absolute/);
    await assert.rejects(backupFilePath(directory, "../../data"), /identifier/);
  });
  it("rejects unsafe links, devices, traversal and restore staging data", () => {
    for (const header of [
      { name: "snapshot/root-0/../secret", type: "file" },
      { name: "/snapshot/root-0/data", type: "file" },
      {
        name: "snapshot/root-0/data",
        type: "symlink",
        linkname: "../../secret",
      },
      {
        name: "snapshot/root-0/data",
        type: "link",
        linkname: "snapshot/root-0/world",
      },
      { name: "snapshot/root-0/data", type: "fifo" },
      { name: "snapshot/root-0/data", type: "character-device" },
      { name: "snapshot/root-0/.ludock-restore-secret/data", type: "file" },
      { name: "snapshot/root-unknown/data", type: "file" },
    ] as tar.Headers[])
      assert.throws(() => validateArchiveEntry(header, roots));
  });
  it("validates recorded roots and rejects duplicate paths and oversized archives", async () => {
    const base: Array<{ header: tar.Headers; body?: string }> = [
      { header: { name: "snapshot", type: "directory" } },
      { header: { name: "snapshot/root-0", type: "directory" } },
    ];
    const valid = await archive([
      ...base,
      {
        header: { name: "snapshot/root-0/world.txt", type: "file", size: 5 },
        body: "hello",
      },
    ]);
    await pipeline(Readable.from(valid), archiveValidator(roots, 10000));
    const duplicate = await archive([...base, ...base]);
    await assert.rejects(
      pipeline(Readable.from(duplicate), archiveValidator(roots, 10000)),
      /duplicate/,
    );
    const missing = await archive([
      { header: { name: "snapshot", type: "directory" } },
    ]);
    await assert.rejects(
      pipeline(Readable.from(missing), archiveValidator(roots, 10000)),
      /missing/,
    );
    await assert.rejects(
      pipeline(Readable.from(valid), archiveValidator(roots, 4)),
      /size/,
    );
  });
  it(
    "verifies archive checksums and refuses symlink archive files",
    { skip: process.platform !== "linux" },
    async () => {
      const bytes = await archive([
        { header: { name: "snapshot", type: "directory" } },
        { header: { name: "snapshot/root-0", type: "directory" } },
      ]);
      const id = randomUUID();
      const filename = await backupFilePath(directory, id);
      await writeFile(filename, bytes);
      await validateArchive(
        directory,
        id,
        roots,
        10000,
        createHash("sha256").update(bytes).digest("hex"),
      );
      await assert.rejects(
        validateArchive(directory, id, roots, 10000, "bad-checksum"),
        /checksum/,
      );
      const linked = randomUUID();
      await symlink(filename, await backupFilePath(directory, linked));
      await assert.rejects(
        validateArchive(directory, linked, roots, 10000, "bad-checksum"),
      );
    },
  );
});
