/** Executed only inside the isolated Bun helper. Descriptor-relative traversal
 * pins each directory before using it, so swapping a parent for a symlink never
 * redirects a later read/write to another root. Keep this dependency-free. */
export const FILE_HELPER_SCRIPT = String.raw`
const fs = require("node:fs");
const fsp = require("node:fs/promises");
const path = require("node:path").posix;
const C = fs.constants;
const request = JSON.parse(process.argv[1]);
// The deadline belongs to the command as well as its Docker connection: losing
// the connection must not leave a writer running after the API times out.
const deadline = setTimeout(() => process.exit(1), ["backup", "download", "upload"].includes(request.operation) ? 30 * 60_000 : 55_000);
deadline.unref();
const opened = new Set();
const fail = (message) => { throw new Error(message); };
const pin = async (name, flags) => { const file = await fsp.open(name, flags); opened.add(file); return file; };
const close = async (file) => { opened.delete(file); await file.close(); };
const link = (directory, name = "") => "/proc/self/fd/" + directory.fd + (name ? "/" + name : "");
const directoryFlags = C.O_RDONLY | C.O_DIRECTORY | C.O_NOFOLLOW;
const fileFlags = C.O_RDONLY | C.O_NOFOLLOW | C.O_NONBLOCK;
const within = (value, root) => value === root || value.startsWith(root + "/");
const components = (value) => {
  if (typeof value !== "string" || value.includes("\0") || value.split("/").includes("..")) fail("Invalid path");
  return value.split("/").filter((part) => part && part !== ".");
};
const blocked = (value, recursive = false) => (request.blocked || []).some((root) => within(value, root) || (recursive && within(root, value)));
const absolute = (relative) => path.join(request.root, relative || "");
const check = (relative, recursive = false) => { if (blocked(absolute(relative), recursive)) fail("This path overlaps an excluded mount"); };
async function pinRoot() {
  if (!request.root.startsWith("/") || request.root === "/") fail("Invalid root");
  let current = await pin("/", directoryFlags);
  for (const part of components(request.root)) {
    const next = await pin(link(current, part), directoryFlags);
    await close(current); current = next;
  }
  return current;
}
async function pinDirectory(root, relative) {
  let current = root;
  for (const part of components(relative)) current = await pin(link(current, part), directoryFlags);
  return current;
}
async function parentOf(root, relative) {
  const parts = components(relative);
  if (!parts.length) fail("The root cannot be changed or downloaded");
  return { parent: await pinDirectory(root, parts.slice(0, -1).join("/")), name: parts.at(-1) };
}
async function openRegular(parent, name, flags = fileFlags) {
  const file = await pin(link(parent, name), flags);
  const stat = await file.stat();
  if (!stat.isFile() || stat.nlink !== 1) fail("Only regular files without hard links are supported");
  return { file, stat };
}
function json(value) { process.stdout.write(JSON.stringify(value)); }
async function write(value) { await new Promise((resolve, reject) => process.stdout.write(value, (error) => error ? reject(error) : resolve())); }
async function streamFile(file, size) {
  const buffer = Buffer.alloc(64 * 1024); let remaining = size; let position = 0;
  while (remaining > 0) {
    const result = await file.read(buffer, 0, Math.min(buffer.length, remaining), position);
    if (!result.bytesRead) fail("The file changed while it was read");
    await write(buffer.subarray(0, result.bytesRead));
    position += result.bytesRead; remaining -= result.bytesRead;
  }
}
function tarHeader(name, info, directory, type = directory ? 53 : 48) {
  const header = Buffer.alloc(512);
  if (Buffer.byteLength(name) > 4096) fail("An archive path is too long");
  if (Buffer.byteLength(name) > 100) {
    const slash = name.lastIndexOf("/", name.endsWith("/") ? name.length - 2 : undefined);
    if (slash < 1 || Buffer.byteLength(name.slice(0, slash)) > 155 || Buffer.byteLength(name.slice(slash + 1)) > 100) {
      // A standard per-entry PAX path keeps long and Unicode names intact.
      const record = " path=" + name + "\n";
      const bytes = Buffer.byteLength(record); let size = bytes + 1;
      while (String(size).length + bytes !== size) size = String(size).length + bytes;
      const payload = Buffer.from(String(size) + record);
      return Buffer.concat([tarHeader("PaxHeader", { ...info, size: payload.length }, false, 120), payload,
        Buffer.alloc((512 - payload.length % 512) % 512), tarHeader("entry", info, directory, type)]);
    }
    header.write(name.slice(0, slash), 345, 155); name = name.slice(slash + 1);
  }
  header.write(name, 0, 100);
  const octal = (value, offset, length) => {
    const number = Math.max(0, Math.floor(value));
    if (!Number.isSafeInteger(number)) fail("An archive value is too large");
    const encoded = number.toString(8);
    if (encoded.length >= length) {
      // GNU/POSIX readers and tar-stream accept positive base-256 fields.
      let remaining = BigInt(number); header[offset] = 128;
      for (let index = offset + length - 1; index > offset; index--) { header[index] = Number(remaining & 255n); remaining >>= 8n; }
      if (remaining) fail("An archive value is too large");
      return;
    }
    header.write(encoded.padStart(length - 1, "0") + "\0", offset, length);
  };
  octal(info.mode & 0o777, 100, 8); octal(request.operation === "backup" ? info.uid : 0, 108, 8); octal(request.operation === "backup" ? info.gid : 0, 116, 8);
  octal(directory ? 0 : info.size, 124, 12); octal(info.mtimeMs / 1000, 136, 12);
  header.fill(32, 148, 156); header[156] = type;
  header.write("ustar\0", 257, 6); header.write("00", 263, 2);
  const checksum = header.reduce((sum, value) => sum + value, 0);
  header.write(checksum.toString(8).padStart(6, "0") + "\0 ", 148, 8);
  return header;
}
let archiveEntries = 0;
async function archiveEntry(parent, name, relative, depth = 0) {
  if (depth > 64 || ++archiveEntries > 100000) fail("The archive has too many entries or directories");
  const file = await pin(link(parent, name), fileFlags);
  const info = await file.stat();
  if (info.isDirectory()) {
    await write(tarHeader(relative + "/", info, true));
    const names = await fsp.readdir(link(file));
    for (const child of names) await archiveEntry(file, child, relative + "/" + child, depth + 1);
  } else {
    if (!info.isFile() || info.nlink !== 1) fail("Archive cannot include symbolic links, hard links, or special files");
    await write(tarHeader(relative, info, false)); await streamFile(file, info.size);
    if (info.size % 512) await write(Buffer.alloc(512 - info.size % 512));
  }
  await close(file);
}
async function removeEntry(parent, name, depth = 0) {
  if (depth > 64) fail("The directory is too deeply nested");
  const info = await fsp.lstat(link(parent, name));
  if (info.isDirectory()) {
    const directory = await pin(link(parent, name), directoryFlags);
    for (const child of await fsp.readdir(link(directory))) await removeEntry(directory, child, depth + 1);
    await close(directory); await fsp.rmdir(link(parent, name));
  } else await fsp.unlink(link(parent, name));
}
async function main() {
  const root = await pinRoot();
  const relative = components(request.path || "").join("/");
  check(relative, ["download", "delete", "rename"].includes(request.operation));
  if (request.operation === "check") return json({ safe: true });
  if (request.operation === "backup") {
    check(relative, true);
    const info = await root.stat(); await write(tarHeader("source/", info, true));
    for (const name of await fsp.readdir(link(root))) await archiveEntry(root, name, "source/" + name);
    await write(Buffer.alloc(1024)); return;
  }
  if (request.operation === "list") {
    const directory = await pinDirectory(root, relative);
    const entries = [];
    const names = await fsp.readdir(link(directory));
    if (names.length > 10000) fail("This directory has too many entries to list");
    for (const name of names) {
      if (blocked(absolute(path.join(relative, name)))) continue;
      const stat = await fsp.lstat(link(directory, name));
      if (!stat.isDirectory() && !stat.isFile() && !stat.isSymbolicLink()) continue;
      entries.push({ name, type: stat.isSymbolicLink() ? "symlink" : stat.isDirectory() ? "directory" : "file", size: stat.isFile() ? stat.size : 0, modifiedAt: stat.mtimeMs });
    }
    return json(entries);
  }
  const { parent, name } = await parentOf(root, relative);
  if (request.operation === "mkdir") { await fsp.mkdir(link(parent, name)); return json({ ok: true }); }
  if (request.operation === "delete") { await removeEntry(parent, name); return json({ ok: true }); }
  if (request.operation === "rename") {
    const destination = components(request.destination).join("/"); check(destination, true);
    const target = await parentOf(root, destination);
    if ((await fsp.lstat(link(parent, name))).isSymbolicLink()) fail("Symbolic links cannot be renamed");
    try { await fsp.lstat(link(target.parent, target.name)); fail("The destination already exists"); }
    catch (error) { if (error.code !== "ENOENT") throw error; }
    await fsp.rename(link(parent, name), link(target.parent, target.name)); return json({ ok: true });
  }
  if (request.operation === "upload") {
    if (!Number.isSafeInteger(request.size) || request.size < 0) fail("Invalid upload size");
    const { file } = await openRegular(parent, name, C.O_WRONLY | C.O_CREAT | C.O_NOFOLLOW | C.O_NONBLOCK);
    await file.truncate(0); let received = 0;
    const idle = setTimeout(() => process.exit(1), 60_000); idle.unref();
    try { for await (const chunk of process.stdin) {
      idle.refresh(); received += chunk.length; if (received > request.size) fail("Upload exceeded its declared size");
      let offset = 0;
      while (offset < chunk.length) { const result = await file.write(chunk, offset, chunk.length - offset); offset += result.bytesWritten; }
    } } finally { clearTimeout(idle); }
    if (received !== request.size) fail("Upload did not match its declared size");
    await file.sync(); return json({ ok: true });
  }
  const file = await pin(link(parent, name), fileFlags);
  const info = await file.stat();
  if (!info.isDirectory() && (!info.isFile() || info.nlink !== 1)) fail("Only regular files and directories can be downloaded");
  if (request.operation === "stat") return json({ type: info.isDirectory() ? "directory" : "file", size: info.isFile() ? info.size : 0 });
  if (request.operation === "download") {
    if (info.isDirectory()) { await close(file); await archiveEntry(parent, name, name); await write(Buffer.alloc(1024)); }
    else await streamFile(file, info.size);
    return;
  }
  fail("Unknown file operation");
}
main().catch(() => {
  // Paths and raw filesystem errors can contain private host information.
  process.stderr.write("The file operation failed: the path changed, is unsafe, or cannot be accessed.");
  process.exitCode = 1;
}).finally(async () => { clearTimeout(deadline); await Promise.all([...opened].map((file) => file.close().catch(() => {}))); });
`;
