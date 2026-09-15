/** @typedef {import("node:fs/promises").FileHandle} FileHandle */
/** @typedef {import("node:fs").Stats} Stats */
import fs from "node:fs";
import fsp from "node:fs/promises";
import { posix as path } from "node:path";
const C = fs.constants;
const request = /** @type {import("./contracts.js").FileHelperRequest} */ (
  JSON.parse(process.argv[1])
);
let uploadAborted = false;
const abortUpload = () => {
  uploadAborted = true;
  process.stdin.destroy(new Error("Upload interrupted"));
};
// The deadline belongs to the command as well as its Docker connection: losing
// the connection must not leave a writer running after the API times out.
const deadline = setTimeout(
  () => (request.operation === "upload" ? abortUpload() : process.exit(1)),
  ["backup", "download", "upload"].includes(request.operation)
    ? 30 * 60_000
    : 55_000,
);
deadline.unref();
/** @type {Set<FileHandle>} */
const opened = new Set();
/** @param {string} message @returns {never} */
function fail(message) {
  throw new Error(message);
}
/** @param {string} name @param {number} flags @param {number} [mode] */
const pin = async (name, flags, mode) => {
  const file = await fsp.open(name, flags, mode);
  opened.add(file);
  return file;
};
/** @param {FileHandle} file */
const close = async (file) => {
  opened.delete(file);
  await file.close();
};
/** @param {FileHandle} directory */
const link = (directory, name = "") =>
  "/proc/self/fd/" + directory.fd + (name ? "/" + name : "");
const directoryFlags = C.O_RDONLY | C.O_DIRECTORY | C.O_NOFOLLOW;
const fileFlags = C.O_RDONLY | C.O_NOFOLLOW | C.O_NONBLOCK;
/** @param {string} value @param {string} root */
const within = (value, root) => value === root || value.startsWith(root + "/");
/** @param {unknown} value @returns {string[]} */
const components = (value) => {
  if (
    typeof value !== "string" ||
    value.includes("\0") ||
    value.split("/").includes("..")
  )
    fail("Invalid path");
  return value.split("/").filter((part) => part && part !== ".");
};
/** @param {string} value */
const blocked = (value, recursive = false) =>
  (request.blocked || []).some(
    (root) => within(value, root) || (recursive && within(root, value)),
  );
/** @param {string} relative */
const absolute = (relative) => path.join(request.root, relative || "");
/** @param {string} relative */
const check = (relative, recursive = false) => {
  if (blocked(absolute(relative), recursive))
    fail("This path overlaps an excluded mount");
};
async function pinRoot() {
  if (!request.root.startsWith("/") || request.root === "/")
    fail("Invalid root");
  let current = await pin("/", directoryFlags);
  for (const part of components(request.root)) {
    const next = await pin(link(current, part), directoryFlags);
    await close(current);
    current = next;
  }
  return current;
}
/** @param {FileHandle} root @param {string} relative */
async function pinDirectory(root, relative) {
  let current = root;
  for (const part of components(relative))
    current = await pin(link(current, part), directoryFlags);
  return current;
}
/** @param {FileHandle} root @param {string} relative */
async function parentOf(root, relative) {
  const parts = components(relative);
  if (!parts.length) fail("The root cannot be changed or downloaded");
  return {
    parent: await pinDirectory(root, parts.slice(0, -1).join("/")),
    name: parts[parts.length - 1],
  };
}
/** @param {unknown} id */
function uploadName(id) {
  if (
    typeof id !== "string" ||
    !/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(
      id,
    )
  )
    fail("Invalid upload identifier");
  return ".ludock-upload-" + id + ".tmp";
}
/** @param {FileHandle} parent @param {string} name */
async function uploadTarget(parent, name) {
  try {
    const info = await fsp.lstat(link(parent, name));
    if (!info.isFile() || info.nlink !== 1)
      fail("Only regular files without hard links are supported");
    return info;
  } catch (error) {
    if (/** @type {NodeJS.ErrnoException} */ (error).code !== "ENOENT")
      throw error;
    return undefined;
  }
}
/** @param {unknown} value */
function json(value) {
  process.stdout.write(JSON.stringify(value));
}
/** @param {string | Uint8Array} value @returns {Promise<void>} */
async function write(value) {
  await new Promise((resolve, reject) =>
    process.stdout.write(value, (error) =>
      error ? reject(error) : resolve(undefined),
    ),
  );
}
/** @param {FileHandle} file @param {number} size */
async function streamFile(file, size) {
  const buffer = Buffer.alloc(64 * 1024);
  let remaining = size;
  let position = 0;
  while (remaining > 0) {
    const result = await file.read(
      buffer,
      0,
      Math.min(buffer.length, remaining),
      position,
    );
    if (!result.bytesRead) fail("The file changed while it was read");
    await write(buffer.subarray(0, result.bytesRead));
    position += result.bytesRead;
    remaining -= result.bytesRead;
  }
}
/** @param {string} name @param {Pick<Stats, 'size' | 'uid' | 'gid' | 'mode' | 'mtimeMs'>} info @param {boolean} directory @returns {Buffer} */
function tarHeader(name, info, directory, type = directory ? 53 : 48) {
  const header = Buffer.alloc(512);
  if (Buffer.byteLength(name) > 4096) fail("An archive path is too long");
  if (Buffer.byteLength(name) > 100) {
    const slash = name.lastIndexOf(
      "/",
      name.endsWith("/") ? name.length - 2 : undefined,
    );
    if (
      slash < 1 ||
      Buffer.byteLength(name.slice(0, slash)) > 155 ||
      Buffer.byteLength(name.slice(slash + 1)) > 100
    ) {
      // A standard per-entry PAX path keeps long and Unicode names intact.
      const record = " path=" + name + "\n";
      const bytes = Buffer.byteLength(record);
      let size = bytes + 1;
      while (String(size).length + bytes !== size)
        size = String(size).length + bytes;
      const payload = Buffer.from(String(size) + record);
      return Buffer.concat([
        tarHeader("PaxHeader", { ...info, size: payload.length }, false, 120),
        payload,
        Buffer.alloc((512 - (payload.length % 512)) % 512),
        tarHeader("entry", info, directory, type),
      ]);
    }
    header.write(name.slice(0, slash), 345, 155);
    name = name.slice(slash + 1);
  }
  header.write(name, 0, 100);
  /** @param {number} value @param {number} offset @param {number} length */
  const octal = (value, offset, length) => {
    const number = Math.max(0, Math.floor(value));
    if (!Number.isSafeInteger(number)) fail("An archive value is too large");
    const encoded = number.toString(8);
    if (encoded.length >= length) {
      // GNU/POSIX readers and tar-stream accept positive base-256 fields.
      let remaining = BigInt(number);
      header[offset] = 128;
      for (let index = offset + length - 1; index > offset; index--) {
        header[index] = Number(remaining & 255n);
        remaining >>= 8n;
      }
      if (remaining) fail("An archive value is too large");
      return;
    }
    header.write(encoded.padStart(length - 1, "0") + "\0", offset, length);
  };
  octal(info.mode & 0o777, 100, 8);
  octal(request.operation === "backup" ? info.uid : 0, 108, 8);
  octal(request.operation === "backup" ? info.gid : 0, 116, 8);
  octal(directory ? 0 : info.size, 124, 12);
  octal(info.mtimeMs / 1000, 136, 12);
  header.fill(32, 148, 156);
  header[156] = type;
  header.write("ustar\0", 257, 6);
  header.write("00", 263, 2);
  const checksum = header.reduce((sum, value) => sum + value, 0);
  header.write(checksum.toString(8).padStart(6, "0") + "\0 ", 148, 8);
  return header;
}
let archiveEntries = 0;
/** @param {FileHandle} parent @param {string} name @param {string} relative @returns {Promise<void>} */
async function archiveEntry(parent, name, relative, depth = 0) {
  if (depth > 64 || ++archiveEntries > 100000)
    fail("The archive has too many entries or directories");
  const file = await pin(link(parent, name), fileFlags);
  const info = await file.stat();
  if (info.isDirectory()) {
    await write(tarHeader(relative + "/", info, true));
    const names = await fsp.readdir(link(file));
    for (const child of names)
      await archiveEntry(file, child, relative + "/" + child, depth + 1);
  } else {
    if (!info.isFile() || info.nlink !== 1)
      fail(
        "Archive cannot include symbolic links, hard links, or special files",
      );
    await write(tarHeader(relative, info, false));
    await streamFile(file, info.size);
    if (info.size % 512) await write(Buffer.alloc(512 - (info.size % 512)));
  }
  await close(file);
}
/** @param {FileHandle} parent @param {string} name @returns {Promise<void>} */
async function removeEntry(parent, name, depth = 0) {
  if (depth > 64) fail("The directory is too deeply nested");
  const info = await fsp.lstat(link(parent, name));
  if (info.isDirectory()) {
    const directory = await pin(link(parent, name), directoryFlags);
    for (const child of await fsp.readdir(link(directory)))
      await removeEntry(directory, child, depth + 1);
    await close(directory);
    await fsp.rmdir(link(parent, name));
  } else await fsp.unlink(link(parent, name));
}
async function main() {
  const root = await pinRoot();
  const relative = components(request.path || "").join("/");
  check(relative, ["download", "delete", "rename"].includes(request.operation));
  if (request.operation === "check") return json({ safe: true });
  if (request.operation === "backup") {
    check(relative, true);
    const info = await root.stat();
    await write(tarHeader("source/", info, true));
    for (const name of await fsp.readdir(link(root)))
      await archiveEntry(root, name, "source/" + name);
    await write(Buffer.alloc(1024));
    return;
  }
  if (request.operation === "list") {
    const directory = await pinDirectory(root, relative);
    const entries = [];
    const names = await fsp.readdir(link(directory));
    if (names.length > 10000)
      fail("This directory has too many entries to list");
    for (const name of names) {
      if (blocked(absolute(path.join(relative, name)))) continue;
      const stat = await fsp.lstat(link(directory, name));
      if (!stat.isDirectory() && !stat.isFile() && !stat.isSymbolicLink())
        continue;
      entries.push({
        name,
        type: stat.isSymbolicLink()
          ? "symlink"
          : stat.isDirectory()
            ? "directory"
            : "file",
        size: stat.isFile() ? stat.size : 0,
        modifiedAt: stat.mtimeMs,
      });
    }
    return json(entries);
  }
  const { parent, name } = await parentOf(root, relative);
  if (request.operation === "mkdir") {
    await fsp.mkdir(link(parent, name));
    return json({ ok: true });
  }
  if (request.operation === "delete") {
    await removeEntry(parent, name);
    return json({ ok: true });
  }
  if (request.operation === "rename") {
    const destination = components(request.destination).join("/");
    check(destination, true);
    const target = await parentOf(root, destination);
    if ((await fsp.lstat(link(parent, name))).isSymbolicLink())
      fail("Symbolic links cannot be renamed");
    try {
      await fsp.lstat(link(target.parent, target.name));
      fail("The destination already exists");
    } catch (error) {
      if (/** @type {NodeJS.ErrnoException} */ (error).code !== "ENOENT")
        throw error;
    }
    await fsp.rename(link(parent, name), link(target.parent, target.name));
    return json({ ok: true });
  }
  if (request.operation === "upload") {
    if (
      typeof request.size !== "number" ||
      !Number.isSafeInteger(request.size) ||
      request.size < 0
    )
      fail("Invalid upload size");
    const temporary = uploadName(request.uploadId || crypto.randomUUID());
    const trailer = request.uploadId
      ? Buffer.from(request.uploadId)
      : Buffer.alloc(0);
    const original = await uploadTarget(parent, name);
    const file = await pin(
      link(parent, temporary),
      C.O_WRONLY | C.O_CREAT | C.O_EXCL | C.O_NOFOLLOW | C.O_NONBLOCK,
      0o600,
    );
    const idle = setTimeout(abortUpload, 60_000);
    idle.unref();
    let received = 0;
    let committed = 0;
    try {
      for await (const chunk of process.stdin) {
        idle.refresh();
        const payload = Math.min(chunk.length, request.size - received);
        let offset = 0;
        while (offset < payload) {
          const result = await file.write(chunk, offset, payload - offset);
          if (!result.bytesWritten) fail("Upload write made no progress");
          offset += result.bytesWritten;
        }
        received += payload;
        const tail = chunk.subarray(payload);
        if (
          committed + tail.length > trailer.length ||
          !tail.equals(trailer.subarray(committed, committed + tail.length))
        )
          fail("Upload exceeded its declared size or was interrupted");
        committed += tail.length;
      }
      if (
        uploadAborted ||
        received !== request.size ||
        committed !== trailer.length
      )
        fail("Upload did not match its declared size or was interrupted");
      // Set permissions while the helper still owns the new inode; CHOWN is
      // sufficient to restore game ownership without granting FOWNER.
      await file.chmod(
        original ? original.mode & 0o777 : 0o666 & ~process.umask(),
      );
      if (original) await file.chown(original.uid, original.gid);
      await file.sync();
      const current = await uploadTarget(parent, name);
      /** @type {(keyof Stats)[]} */
      const compared = [
        "dev",
        "ino",
        "mode",
        "uid",
        "gid",
        "size",
        "mtimeMs",
        "ctimeMs",
      ];
      if (
        Boolean(current) !== Boolean(original) ||
        (original &&
          current &&
          compared.some((field) => original[field] !== current[field]))
      )
        fail("The upload target changed");
      if (uploadAborted) fail("Upload interrupted");
      if (original)
        await fsp.rename(link(parent, temporary), link(parent, name));
      else {
        await fsp.link(link(parent, temporary), link(parent, name));
        await fsp.unlink(link(parent, temporary));
      }
      await parent.sync();
      return json({ ok: true });
    } finally {
      clearTimeout(idle);
      try {
        await close(file);
      } finally {
        await fsp.unlink(link(parent, temporary)).catch((error) => {
          if (/** @type {NodeJS.ErrnoException} */ (error).code !== "ENOENT")
            throw error;
        });
      }
    }
  }
  if (request.operation === "upload-cleanup") {
    await fsp
      .unlink(link(parent, uploadName(request.uploadId)))
      .catch((error) => {
        if (/** @type {NodeJS.ErrnoException} */ (error).code !== "ENOENT")
          throw error;
      });
    return json({ ok: true });
  }
  const file = await pin(link(parent, name), fileFlags);
  const info = await file.stat();
  if (!info.isDirectory() && (!info.isFile() || info.nlink !== 1))
    fail("Only regular files and directories can be downloaded");
  if (request.operation === "stat")
    return json({
      type: info.isDirectory() ? "directory" : "file",
      size: info.isFile() ? info.size : 0,
    });
  if (request.operation === "download") {
    if (info.isDirectory()) {
      await close(file);
      await archiveEntry(parent, name, name);
      await write(Buffer.alloc(1024));
    } else await streamFile(file, info.size);
    return;
  }
  fail("Unknown file operation");
}
main()
  .catch(() => {
    // Paths and raw filesystem errors can contain private host information.
    process.stderr.write(
      "The file operation failed: the path changed, is unsafe, or cannot be accessed.",
    );
    process.exitCode = 1;
  })
  .finally(async () => {
    clearTimeout(deadline);
    await Promise.all([...opened].map((file) => file.close().catch(() => {})));
  });
