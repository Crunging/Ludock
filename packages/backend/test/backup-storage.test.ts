import { fixtureBytes } from "./fixtures/bytes.js";
import { concatBytes, decodeText } from "../src/bytes.js";
import { rejectedBy } from "./fixtures/errors.js";
import { StreamFixture } from "./fixtures/web-streams.js";
import type { ArchiveHeader } from "../src/backup-storage.js";
import {
  mkdtemp,
  rm,
  mkdir,
  symlink,
  writeFile,
  readFile,
  realpath,
  readdir,
} from "node:fs/promises";
import path from "node:path";
import { tmpdir } from "node:os";
import { expect, afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";
import { walkTar, encodeTarHeader, tarPadding, tarEnd } from "../src/tar.js";
import type * as Docker from "../src/docker-client.js";
import {
  archiveEntryMetadata,
  mappedArchiveHeader,
  approvedBackupDirectory,
  validateArchiveStream,
  backupFilePath,
  createDataHelper,
  extractRootToStage,
  helperExec,
  validateArchive,
  writeSnapshot,
  validateArchiveEntry,
  removeArchive,
  availableBackupDestinationBytes,
} from "../src/backup-storage.js";
import { getDockerInstance } from "../src/docker.js";
import { DEFAULT_HELPER_IMAGE } from "../src/runtime-images.js";
import type { ServerContext } from "../src/servers.js";
import { AppError } from "../src/errors.js";

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
): Promise<Uint8Array> {
  const chunks: Uint8Array[] = [];
  for (const { header, body } of entries) {
    const bytes = fixtureBytes(body || "");
    chunks.push(encodeTarHeader({ ...header, size: header.size ?? bytes.length }), bytes, tarPadding(bytes.length));
  }
  return concatBytes([...chunks, tarEnd()]);
}

async function snapshotFixture(body: string, options: { keepOpen?: boolean; onStart?: () => void } = {}) {
  const input = await archive([
    { header: { name: "data/", type: "directory", mode: 0o755 } },
    { header: { name: "data/file", type: "file", mode: 0o640 }, body },
  ]);
  const frame = new Uint8Array(input.length + 8);
  frame[0] = 1;
  new DataView(frame.buffer).setUint32(4, input.length);
  frame.set(input, 8);
  const stream = new StreamFixture();
  stream.enqueue(frame);
  if (!options.keepOpen) stream.close();
  const helper = {
    roots,
    container: {
      exec: async () => ({
        start: async () => { options.onStart?.(); return stream.connection; },
        inspect: async () => ({ ExitCode: 0, Running: false }),
      }),
    } as unknown as Docker.Container,
    cleanup: async () => {},
  };
  const context = { observation: { mounts: [{ destination: "/data", writable: true }] } } as ServerContext;
  const settings = { destination: directory, retentionCount: 1, maxBytes: 1_000_000, reserveBytes: 0 };
  const id = crypto.randomUUID();
  return {
    id, stream,
    write: (assertAccess?: () => void) => writeSnapshot(context, helper, settings, id, settings.maxBytes, async () => {}, assertAccess),
  };
}

describe("backup storage boundaries", () => {
  it.skipIf(process.platform !== "linux")("publishes the final buffered bytes with the complete archive checksum", async () => {
    const body = "buffered data".repeat(11);
    const fixture = await snapshotFixture(body);
    const result = await fixture.write();
    const bytes = new Uint8Array(await Bun.file(await backupFilePath(directory, fixture.id)).arrayBuffer());
    expect(result).toStrictEqual({
      size: bytes.length,
      checksum: new Bun.CryptoHasher("sha256").update(bytes).digest("hex"),
    });
    const entries: Array<{ name: string; body: string }> = [];
    await walkTar(ReadableStream.from([bytes]), async (header, chunks) => {
      const content: Uint8Array[] = [];
      for await (const chunk of chunks) content.push(chunk);
      entries.push({ name: header.name, body: decodeText(concatBytes(content)) });
    });
    expect(entries).toStrictEqual([
      { name: "snapshot/", body: "" },
      { name: "snapshot/root-0/", body: "" },
      { name: "snapshot/root-0/file", body },
    ]);
    expect(await readdir(directory)).toStrictEqual([`${fixture.id}.tar`]);
  });

  for (const failureAt of ["write", "flush", "end"] as const) {
    it.skipIf(process.platform !== "linux")(`cleans up a buffered archive when ${failureAt} fails`, async () => {
      const probePath = path.join(directory, "writer-probe");
      const probe = Bun.file(probePath).writer();
      const prototype = Object.getPrototypeOf(probe) as Bun.FileSink;
      await probe.end();
      await rm(probePath);
      // Keep the source open for write/flush failures to prove cancellation
      // does not depend on reaching EOF or consuming the rest of the archive.
      const fixture = await snapshotFixture(failureAt === "end" ? "small" : "x".repeat(131_072), {
        keepOpen: failureAt !== "end",
      });
      const failure = new Error("Fixture destination full");
      if (failureAt === "write") {
        const write = prototype.write;
        spyOn(prototype, "write")
          .mockImplementationOnce(function (this: Bun.FileSink, chunk) { return write.call(this, chunk); })
          .mockImplementationOnce(async () => { throw failure; });
      } else {
        spyOn(prototype, failureAt).mockImplementationOnce(async () => { throw failure; });
      }
      await expect(fixture.write()).rejects.toThrow(failure.message);
      expect(fixture.stream.cancelled).toBe(true);
      expect(await readdir(directory)).toStrictEqual([]);
    });
  }

  it.skipIf(process.platform !== "linux")("discards buffered output when access is revoked after the source starts", async () => {
    let allowed = true;
    const fixture = await snapshotFixture("small", { keepOpen: true, onStart: () => { allowed = false; } });
    await expect(fixture.write(() => {
      if (!allowed) throw new Error("Access revoked");
    })).rejects.toThrow(/Access revoked/);
    expect(fixture.stream.cancelled).toBe(true);
    expect(await readdir(directory)).toStrictEqual([]);
  });

  it.skipIf(process.platform !== "linux")("cleans up failed writes without removing a partial archive it did not create", async () => {
    const id = crypto.randomUUID();
    const filename = await backupFilePath(directory, id, true);
    const settings = { destination: directory, retentionCount: 1, maxBytes: 10_000, reserveBytes: 0 };
    const helper = { roots, container: {} as Docker.Container, cleanup: async () => {} };
    await Bun.write(filename, "existing partial");
    await expect(writeSnapshot({} as ServerContext, helper, settings, id, 1, async () => {})).rejects.toThrow();
    expect(await Bun.file(filename).text()).toBe("existing partial");
    await Bun.file(filename).delete();
    await expect(writeSnapshot({} as ServerContext, helper, settings, id, 1, async () => {})).rejects.toThrow(/byte limit/);
    expect(await Bun.file(filename).exists()).toBe(false);
    expect(await Bun.file(await backupFilePath(directory, id)).exists()).toBe(false);
  });
  it("accepts whitespace around configured roots and still confines the destination", async () => {
    process.env.LUDOCK_BACKUP_ROOTS = `  ${directory}  ${path.delimiter} `;
    expect(await approvedBackupDirectory(directory)).toBe(directory);
    await expect(approvedBackupDirectory(path.dirname(directory))).rejects.toThrow(/inside/);
  });

  it("explains missing roots and folders without exposing filesystem errors or creating directories", async () => {
    for (const missingRoot of [false, true]) {
      const missing = path.join(directory, "missing-private-fixture");
      process.env.LUDOCK_BACKUP_ROOTS = missingRoot ? missing : directory;
      await expect(await rejectedBy(approvedBackupDirectory(missingRoot ? directory : missing))).toSatisfy((error) => {
        expect(error instanceof AppError).toBeTruthy();
        expect(error.code).toBe("BACKUP_DESTINATION");
        expect(error.message).toMatch(/Create the folder and check its mount and permissions/);
        expect(error.message).not.toMatch(/ENOENT|missing-private-fixture/);
        return true;
      });
      expect(await readdir(directory)).toStrictEqual([]);
    }
    process.env.LUDOCK_BACKUP_ROOTS = "   ";
    await expect(approvedBackupDirectory(directory)).rejects.toThrow(/Set LUDOCK_BACKUP_ROOTS/);
  });

  it.skipIf(process.platform !== "linux")("measures free space through an approved destination without creating files", async () => {
    const available = await availableBackupDestinationBytes(directory);
    expect(Number.isSafeInteger(available)).toBe(true);
    expect(available >= 0).toBe(true);
    expect(await readdir(directory)).toStrictEqual([]);
    const linked = `${directory}-linked`;
    await symlink(directory, linked);
    try {
      await expect(availableBackupDestinationBytes(linked)).rejects.toThrow(/inside|symbolic|symlink/);
    } finally {
      await rm(linked, { force: true });
    }
  });

  it.skipIf(process.platform !== "linux")("rejects FIFO archives without waiting for a writer", async () => {
    const id = crypto.randomUUID();
    const filename = await backupFilePath(directory, id);
    const fifo = Bun.spawn(["mkfifo", filename], { stdin: "ignore", stdout: "ignore", stderr: "pipe" });
    expect(await fifo.exited, await new Response(fifo.stderr).text()).toBe(0);
    // Isolate a blocking-open regression so the test deadline can clean up its
    // process even if the runtime is waiting for a FIFO writer in native code.
    const child = Bun.spawn([process.execPath, "-e", `
      import { archiveReadStream } from ${JSON.stringify(new URL("../src/backup-storage.ts", import.meta.url).href)};
      try {
        const stream = await archiveReadStream(${JSON.stringify(directory)}, ${JSON.stringify(id)});
        await stream.cancel(); process.exitCode = 2;
      } catch (error) {
        if (error.code !== "INVALID_BACKUP") { console.error(error); process.exitCode = 3; }
      }
    `], {
      env: { ...process.env, LUDOCK_BACKUP_ROOTS: directory },
      stdin: "ignore", stdout: "ignore", stderr: "pipe",
    });
    const deadline = setTimeout(() => child.kill("SIGKILL"), 2000);
    try { expect(await child.exited, await new Response(child.stderr).text()).toBe(0); }
    finally { clearTimeout(deadline); child.kill("SIGKILL"); await child.exited; }
  }, 4000);

  it.skipIf(process.platform !== "linux")("keeps an archive when access is revoked before unlink dispatch", async () => {
    const id = crypto.randomUUID();
    const filename = await backupFilePath(directory, id);
    await writeFile(filename, "keep this backup");
    await expect(removeArchive(directory, id, () => { throw new Error("Access revoked"); })).rejects.toThrow(/Access revoked/);
    expect(await readFile(filename, "utf8")).toBe("keep this backup");
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
          start: async () => { const stream = new StreamFixture(); stream.close(); return stream.connection; },
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
      } as ServerContext, true, crypto.randomUUID());
      try {
        const options = create.mock.calls[0][0];
        expect(options.Image).toBe(helperImage || DEFAULT_HELPER_IMAGE);
        expect(options.HostConfig?.Mounts).toStrictEqual([{
          Type: "volume", Source: "game-data", Target: "/mounts/root-0", ReadOnly: true,
        }]);
        expect(options.HostConfig?.NetworkMode).toBe("none");
      } finally {
        await access.cleanup();
      }
      expect(removed).toBe(true);
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
    await expect(helperExec(helper, ["bun", "restore-helper"], {}, assertAccess)).rejects.toThrow(/Access revoked/);
    allowed = true;
    await expect(extractRootToStage(
        directory, crypto.randomUUID(), helper, roots[0], "/data",
        `.ludock-restore-${crypto.randomUUID()}`, roots, 10000, "checksum",
        assertAccess,
      )).rejects.toThrow(/Access revoked/);
    expect(starts).toBe(0);
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
      const id = crypto.randomUUID();
      await writeFile(await backupFilePath(directory, id), bytes);
      let allowed = true;
      const writes: Uint8Array[] = [];
      const socket = new StreamFixture();
      socket.onInput = chunk => { writes.push(fixtureBytes(chunk)); allowed = false; };
      socket.onInputEnd = () => socket.close();
      const helper = {
        exec: async () => ({
          start: async () => socket.connection,
          inspect: async () => ({ ExitCode: 0 }),
        }),
      } as unknown as Docker.Container;
      await expect(extractRootToStage(
          directory, id, helper, roots[0], "/data",
          `.ludock-restore-${crypto.randomUUID()}`, roots, 10000,
          new Bun.CryptoHasher("sha256").update(bytes).digest("hex"),
          () => {
            if (!allowed) throw new Error("Access revoked during streaming");
          },
        )).rejects.toThrow(/Access revoked/);
      expect(writes.length).toBe(1);
      expect(decodeText(writes[0])).toMatch(/world\.txt/);
      expect(socket.closed).toBe(true);
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
    let found = false;
    await walkTar(ReadableStream.from([bytes]), (header) => {
      expect(header.name).toBe("snapshot/root-0/world");
      expect(archiveEntryMetadata(header)).toStrictEqual({
        uid: 1_000_000, gid: 2_000_000, mtime: source.mtime.getTime() / 1000,
      });
      found = true;
    });
    expect(found).toBe(true);
    for (const metadata of [
      { uid: "4294967295" },
      { gid: "-1" },
      { mtime: "Infinity" },
    ])
      expect(() => archiveEntryMetadata({ ...source, pax: metadata })).toThrow();
  });

  it("uses only existing approved directories and rejects symlink traversal", async () => {
    await mkdir(path.join(directory, "archives"));
    expect(await approvedBackupDirectory(path.join(directory, "archives"))).toBe(path.join(directory, "archives"));
    await symlink(
      path.join(directory, "archives"),
      path.join(directory, "shortcut"),
    );
    await expect(approvedBackupDirectory(path.join(directory, "shortcut"))).rejects.toThrow(/symlink|symbolic/);
    await expect(approvedBackupDirectory(path.dirname(directory))).rejects.toThrow(/inside/);
    await expect(approvedBackupDirectory("relative/path")).rejects.toThrow(/absolute/);
    await expect(backupFilePath(directory, "../../data")).rejects.toThrow(/identifier/);
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
      expect(() => validateArchiveEntry(header, roots)).toThrow();
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
    await validateArchiveStream(ReadableStream.from([valid]), roots, 10000);
    const duplicate = await archive([...base, ...base]);
    await expect(validateArchiveStream(ReadableStream.from([duplicate]), roots, 10000)).rejects.toThrow(/duplicate/);
    const missing = await archive([
      { header: { name: "snapshot", type: "directory" } },
    ]);
    await expect(validateArchiveStream(ReadableStream.from([missing]), roots, 10000)).rejects.toThrow(/missing/);
    await expect(validateArchiveStream(ReadableStream.from([valid]), roots, 4)).rejects.toThrow(/size/);
  });
  it.skipIf(Boolean(process.platform !== "linux"))(
    "verifies archive checksums and refuses symlink archive files",
    async () => {
      const bytes = await archive([
        { header: { name: "snapshot", type: "directory" } },
        { header: { name: "snapshot/root-0", type: "directory" } },
      ]);
      const id = crypto.randomUUID();
      const filename = await backupFilePath(directory, id);
      await writeFile(filename, bytes);
      await validateArchive(
        directory,
        id,
        roots,
        10000,
        new Bun.CryptoHasher("sha256").update(bytes).digest("hex"),
      );
      await expect(validateArchive(directory, id, roots, 10000, "bad-checksum")).rejects.toThrow(/checksum/);
      const linked = crypto.randomUUID();
      await symlink(filename, await backupFilePath(directory, linked));
      await expect(validateArchive(directory, linked, roots, 10000, "bad-checksum")).rejects.toThrow();
    },
  );
});
