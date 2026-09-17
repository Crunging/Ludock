const encoder = new TextEncoder();
const decoder = new TextDecoder("utf-8", { ignoreBOM: true });
/** @typedef {import("node:fs/promises").FileHandle} FileHandle */
/** @typedef {import("node:fs").Stats} Stats */
/** @typedef {import("./contracts.js").RestoreRecord} RestoreRecord */
import fs from "node:fs/promises";
import { constants as C } from "node:fs";
const request = /** @type {{root: string, stage: string, maxBytes: number}} */ (
  JSON.parse(process.argv[1])
);
const flags = C.O_RDONLY | C.O_DIRECTORY | C.O_NOFOLLOW;
/** @type {Set<FileHandle>} */
const owned = new Set();
/** @param {string} name @param {number} options */
const pin = async (name, options) => {
  const fd = await fs.open(name, options);
  owned.add(fd);
  return fd;
};
/** @param {FileHandle} fd */
const close = async (fd) => {
  owned.delete(fd);
  await fd.close();
};
/** @param {FileHandle} fd @param {string} [name] */
const link = (fd, name) => "/proc/self/fd/" + fd.fd + (name ? "/" + name : "");
/** @param {unknown} name @returns {string[]} */
const parts = (name) => {
  if (
    typeof name !== "string" ||
    name.includes("\0") ||
    name.includes("\\") ||
    name.split("/").some((p) => p === ".." || p === "." || p === "") ||
    encoder.encode(name).byteLength > 4096
  )
    throw new Error();
  return name.split("/");
};
(async () => {
  if (
    !request.root.startsWith("/") ||
    !/^\.ludock-restore-[0-9a-f-]{36}$/.test(request.stage)
  )
    throw new Error();
  let root = await pin("/", flags);
  for (const part of parts(request.root.slice(1))) {
    const next = await pin(link(root, part), flags);
    await close(root);
    root = next;
  }
  const stage = await pin(link(root, request.stage), flags);
  const destination = await pin(link(stage, "new"), flags);
  /** @param {string[]} names */
  const parent = async (names) => {
    let current = destination;
    for (const name of names) {
      try {
        await fs.mkdir(link(current, name), { mode: 0o700 });
      } catch (e) {
        if (/** @type {NodeJS.ErrnoException} */ (e).code !== "EEXIST") throw e;
      }
      const next = await pin(link(current, name), flags);
      await current.sync();
      if (current !== destination) await close(current);
      current = next;
    }
    return current;
  };
  /** @param {FileHandle} fd @param {RestoreRecord} record */
  const metadata = async (fd, record) => {
    await fd.chown(record.uid, record.gid);
    await fd.chmod(record.mode & 0o777);
    if (Number.isFinite(record.mtime))
      await fd.utimes(record.mtime, record.mtime);
  };
  /** @type {RestoreRecord[]} */
  const directories = [];
  let buffer = new Uint8Array(0),
    count = 0,
    total = 0;
  /** @type {{file: FileHandle, record: RestoreRecord, remaining: number} | null} */
  let current = null;
  const finish = async () => {
    if (!current) return;
    await metadata(current.file, current.record);
    await current.file.sync();
    await close(current.file);
    current = null;
  };
  for await (const chunk of Bun.stdin.stream()) {
    buffer = new Uint8Array(Bun.concatArrayBuffers([buffer, chunk]));
    while (buffer.length) {
      if (current) {
        const take = Math.min(current.remaining, buffer.length);
        let offset = 0;
        while (offset < take) {
          const result = await current.file.write(
            buffer,
            offset,
            take - offset,
          );
          offset += result.bytesWritten;
        }
        buffer = buffer.subarray(take);
        current.remaining -= take;
        if (current.remaining === 0) await finish();
        continue;
      }
      const end = buffer.indexOf(10);
      if (end === -1) {
        if (buffer.length > 16384) throw new Error();
        break;
      }
      if (end > 16384) throw new Error();
      const record = /** @type {RestoreRecord} */ (
        JSON.parse(decoder.decode(buffer.subarray(0, end)))
      );
      buffer = buffer.subarray(end + 1);
      if (
        ++count > 100000 ||
        !["file", "directory"].includes(record.type) ||
        !Number.isSafeInteger(record.size) ||
        record.size < 0 ||
        !Number.isSafeInteger(record.uid) ||
        record.uid < 0 ||
        record.uid > 4294967294 ||
        !Number.isSafeInteger(record.gid) ||
        record.gid < 0 ||
        record.gid > 4294967294 ||
        !Number.isSafeInteger(record.mode)
      )
        throw new Error();
      total += record.size;
      if (total > request.maxBytes) throw new Error();
      const names = parts(record.name);
      if (
        names.length > 64 ||
        names.some((name) => name.startsWith(".ludock-restore-"))
      )
        throw new Error();
      const owner = await parent(names.slice(0, -1));
      const name = names[names.length - 1];
      if (record.type === "directory") {
        if (record.size !== 0) throw new Error();
        try {
          await fs.mkdir(link(owner, name), { mode: 0o700 });
        } catch (e) {
          if (/** @type {NodeJS.ErrnoException} */ (e).code !== "EEXIST")
            throw e;
        }
        const directory = await pin(link(owner, name), flags);
        await close(directory);
        directories.push(record);
      } else {
        const file = await pin(
          link(owner, name),
          C.O_WRONLY | C.O_CREAT | C.O_EXCL | C.O_NOFOLLOW,
        );
        current = { file, record, remaining: record.size };
        if (record.size === 0) await finish();
      }
      await owner.sync();
      if (owner !== destination) await close(owner);
    }
  }
  if (current || buffer.length) throw new Error();
  for (const record of directories.reverse()) {
    const names = parts(record.name),
      owner = await parent(names.slice(0, -1)),
      directory = await pin(link(owner, names[names.length - 1]), flags);
    await metadata(directory, record);
    await directory.sync();
    await close(directory);
    if (owner !== destination) await close(owner);
  }
  await destination.sync();
  await Bun.write(Bun.stdout, "ok");
})()
  .catch(async () => {
    await Bun.write(Bun.stderr,
      "Restore extraction failed: the archive or destination changed or is unsafe.",
    );
    process.exitCode = 1;
  })
  .finally(async () => {
    await Promise.all([...owned].map((file) => file.close().catch(() => {})));
  });
