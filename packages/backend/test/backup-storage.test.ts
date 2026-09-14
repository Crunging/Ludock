import type { ArchiveHeader } from "../src/backup-storage.js";
import assert from "node:assert/strict";
import {
  mkdtemp,
  rm,
  mkdir,
  symlink,
  writeFile,
  readFile,
  realpath,
} from "node:fs/promises";
import path from "node:path";
import { tmpdir } from "node:os";
import { Duplex, Readable } from "node:stream";
import { pipeline } from "node:stream/promises";
import { afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";
import * as tar from "tar-stream";
import { createHash, randomUUID } from "node:crypto";
import type Docker from "dockerode";
import {
  archiveEntryMetadata,
  mappedArchiveHeader,
  approvedBackupDirectory,
  archiveValidator,
  backupFilePath,
  createDataHelper,
  extractRootToStage,
  helperExec,
  validateArchive,
  validateArchiveEntry,
  removeArchive,
} from "../src/backup-storage.js";
import { getDockerInstance } from "../src/docker.js";
import { DEFAULT_HELPER_IMAGE } from "../src/runtime-images.js";
import type { ServerContext } from "../src/servers.js";

const roots = [{ id: "root-0", path: "/data" }];
let directory: string;
let oldRoots: string | undefined;
let oldHelperImage: string | undefined;
beforeEach(async () => {
  directory = await realpath(
    await mkdtemp(path.join(tmpdir(), "ludock-backup-test-")),
  );
  oldRoots = process.env.LUDOCK_BACKUP_ROOTS;
  oldHelperImage = process.env.FILE_HELPER_IMAGE;
  process.env.LUDOCK_BACKUP_ROOTS = directory;
});
afterEach(async () => {
  mock.restore();
  if (oldRoots === undefined) delete process.env.LUDOCK_BACKUP_ROOTS;
  else process.env.LUDOCK_BACKUP_ROOTS = oldRoots;
  if (oldHelperImage === undefined) delete process.env.FILE_HELPER_IMAGE;
  else process.env.FILE_HELPER_IMAGE = oldHelperImage;
  await rm(directory, { recursive: true, force: true });
});
async function archive(
  entries: Array<{ header: ArchiveHeader; body?: string }>,
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
  it.skipIf(process.platform !== "linux")("rejects FIFO archives without waiting for a writer", async () => {
    const id = randomUUID();
    const filename = await backupFilePath(directory, id);
    const fifo = Bun.spawn(["mkfifo", filename], { stdin: "ignore", stdout: "ignore", stderr: "pipe" });
    assert.equal(await fifo.exited, 0, await new Response(fifo.stderr).text());
    // Isolate a blocking-open regression so the test deadline can clean up its
    // process even if the runtime is waiting for a FIFO writer in native code.
    const child = Bun.spawn([process.execPath, "-e", `
      import { archiveReadStream } from ${JSON.stringify(new URL("../src/backup-storage.ts", import.meta.url).href)};
      try {
        const stream = await archiveReadStream(${JSON.stringify(directory)}, ${JSON.stringify(id)});
        stream.destroy(); process.exitCode = 2;
      } catch (error) {
        if (error.code !== "INVALID_BACKUP") { console.error(error); process.exitCode = 3; }
      }
    `], {
      env: { ...process.env, LUDOCK_BACKUP_ROOTS: directory },
      stdin: "ignore", stdout: "ignore", stderr: "pipe",
    });
    const deadline = setTimeout(() => child.kill("SIGKILL"), 2000);
    try { assert.equal(await child.exited, 0, await new Response(child.stderr).text()); }
    finally { clearTimeout(deadline); child.kill("SIGKILL"); await child.exited; }
  }, 4000);

  it.skipIf(process.platform !== "linux")("keeps an archive when access is revoked before unlink dispatch", async () => {
    const id = randomUUID();
    const filename = await backupFilePath(directory, id);
    await writeFile(filename, "keep this backup");
    await assert.rejects(removeArchive(directory, id, () => { throw new Error("Access revoked"); }), /Access revoked/);
    assert.equal(await readFile(filename, "utf8"), "keep this backup");
  });

  for (const helperImage of [undefined, `example/custom-bun-helper:test@sha256:${"a".repeat(64)}`]) {
    it(`uses the ${helperImage ? "configured" : "pinned default"} image for a scoped backup helper`, async () => {
      if (helperImage === undefined) delete process.env.FILE_HELPER_IMAGE;
      else process.env.FILE_HELPER_IMAGE = helperImage;
      const docker = getDockerInstance();
      let removed = false;
      const helper = {
        start: async () => {},
        remove: async () => { removed = true; },
        exec: async () => ({
          start: async () => Readable.from([]),
          inspect: async () => ({ ExitCode: 0 }),
        }),
      } as unknown as Docker.Container;
      spyOn(docker, "getVolume").mockReturnValue({
        inspect: async () => ({ Driver: "local", Options: {} }),
      } as unknown as Docker.Volume);
      const create = spyOn(docker, "createContainer").mockResolvedValue(helper);
      const access = await createDataHelper({
        container: { fileRoots: roots },
        observation: {
          mounts: [{
            type: "volume", name: "game-data",
            source: "/var/lib/docker/volumes/game-data/_data",
            destination: "/data", writable: true,
          }],
        },
      } as ServerContext, true, randomUUID());
      try {
        const options = create.mock.calls[0][0];
        assert.equal(options.Image, helperImage || DEFAULT_HELPER_IMAGE);
        assert.deepEqual(options.HostConfig?.Mounts, [{
          Type: "volume", Source: "game-data", Target: "/mounts/root-0", ReadOnly: true,
        }]);
        assert.equal(options.HostConfig?.NetworkMode, "none");
      } finally {
        await access.cleanup();
      }
      assert.equal(removed, true);
    });
  }
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
      helperExec(helper, ["bun", "restore-helper"], {}, assertAccess),
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
  it.skipIf(Boolean(process.platform !== "linux"))(
    "stops streaming restore data when access changes after extraction starts",
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
    ] as ArchiveHeader[])
      assert.throws(() => validateArchiveEntry(header, roots));
  });
  it("validates recorded roots and rejects duplicate paths and oversized archives", async () => {
    const base: Array<{ header: ArchiveHeader; body?: string }> = [
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
  it.skipIf(Boolean(process.platform !== "linux"))(
    "verifies archive checksums and refuses symlink archive files",
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
