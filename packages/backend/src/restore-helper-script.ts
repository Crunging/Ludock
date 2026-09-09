/** Fixed program executed inside the isolated data helper. All traversal is
 * relative to pinned directory descriptors; user paths are data, never shell
 * fragments. Complete each filesystem phase durably before recording its next
 * SQLite journal phase in the backend. Linux /proc/self/fd is required. */
export const RESTORE_HELPER_SCRIPT = String.raw`
const fs = require("node:fs");
const fsp = require("node:fs/promises");
const C = fs.constants;
const opened = new Set();
const request = JSON.parse(process.argv[1]);
const fail = () => { throw new Error("Unsafe restore path or changed data"); };
const directoryFlags = C.O_RDONLY | C.O_DIRECTORY | C.O_NOFOLLOW;
const link = (directory, name = "") => "/proc/self/fd/" + directory.fd + (name ? "/" + name : "");
const pin = async (filename, flags = directoryFlags) => {
  const handle = await fsp.open(filename, flags); opened.add(handle); return handle;
};
const close = async (handle) => { opened.delete(handle); await handle.close(); };
const sync = async (...handles) => { for (const handle of new Set(handles)) await handle.sync(); };
const same = (a, b) => a.dev === b.dev && a.ino === b.ino;
async function pinRoot() {
  if (typeof request.root !== "string" || !request.root.startsWith("/") || request.root === "/" || request.root.includes("\0")) fail();
  const parts = request.root.split("/").slice(1);
  if (parts.some((part) => !part || part === "." || part === "..")) fail();
  let current = await pin("/");
  for (const part of parts) { const next = await pin(link(current, part)); await close(current); current = next; }
  return current;
}
async function exists(parent, name) {
  try { return await fsp.lstat(link(parent, name)); }
  catch (error) { if (error.code === "ENOENT") return null; throw error; }
}
async function pinStage(root, optional = false) {
  try { return await pin(link(root, request.stage)); }
  catch (error) { if (optional && error.code === "ENOENT") return null; throw error; }
}
async function unchanged(parent, name, handle) {
  const current = await fsp.lstat(link(parent, name));
  if (!current.isDirectory() || !same(current, await handle.stat())) fail();
}
async function moveContents(source, destination, omitted) {
  const names = await fsp.readdir(link(source));
  if (names.length > 1000000) fail();
  for (const name of names) {
    if (name === omitted) continue;
    if (name.startsWith(".ludock-restore-")) fail();
    const sourceInfo = await fsp.lstat(link(source, name));
    if (!sourceInfo.isDirectory() && (!sourceInfo.isFile() || sourceInfo.nlink !== 1)) fail();
    if (await exists(destination, name)) fail();
    // rename never follows its final component. A racing symlink can be moved,
    // but cannot turn this into a write through that symlink to outside data.
    await fsp.rename(link(source, name), link(destination, name));
    const movedInfo = await fsp.lstat(link(destination, name));
    if (!same(sourceInfo, movedInfo)) fail();
    await sync(source, destination);
  }
}
let removedEntries = 0;
async function removeEntry(parent, name, depth = 0) {
  if (depth > 64 || ++removedEntries > 1000000) fail();
  const before = await fsp.lstat(link(parent, name));
  if (before.isDirectory()) {
    const directory = await pin(link(parent, name));
    if (!same(before, await directory.stat())) fail();
    for (const child of await fsp.readdir(link(directory))) await removeEntry(directory, child, depth + 1);
    await sync(directory);
    // Refuse to unlink a substituted directory after deleting the pinned one.
    await unchanged(parent, name, directory);
    await fsp.rmdir(link(parent, name));
    await close(directory);
  } else {
    // Unlink removes a link itself, never the link target. Special files are
    // rejected so cleanup does not operate on devices or sockets.
    if (!before.isFile() && !before.isSymbolicLink()) fail();
    await fsp.unlink(link(parent, name));
  }
  await sync(parent);
}
async function main() {
  if (!request || typeof request !== "object" || !/^\.ludock-restore-[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(request.stage)) fail();
  const root = await pinRoot();
  if (request.operation === "space") {
    const available = await fsp.statfs(link(root), { bigint: true });
    const bytes = available.bavail * available.bsize;
    if (bytes > BigInt(Number.MAX_SAFE_INTEGER)) fail();
    return { availableBytes: Number(bytes) };
  }
  if (request.operation === "stage") {
    await fsp.mkdir(link(root, request.stage), { mode: 0o700 });
    const stage = await pinStage(root);
    await fsp.mkdir(link(stage, "new"), { mode: 0o700 });
    await fsp.mkdir(link(stage, "old"), { mode: 0o700 });
    const next = await pin(link(stage, "new")); const old = await pin(link(stage, "old"));
    await sync(next, old, stage, root);
    return { ok: true };
  }
  const stage = await pinStage(root, request.operation === "cleanup");
  if (!stage) return { ok: true };
  if (request.operation === "cleanup") {
    await unchanged(root, request.stage, stage);
    for (const name of await fsp.readdir(link(stage))) await removeEntry(stage, name);
    await sync(stage); await unchanged(root, request.stage, stage);
    await fsp.rmdir(link(root, request.stage)); await sync(root);
    return { ok: true };
  }
  if (request.operation === "moveOld") {
    const old = await pin(link(stage, "old"));
    await moveContents(root, old, request.stage);
  } else if (request.operation === "moveNew") {
    const next = await pin(link(stage, "new"));
    await moveContents(next, root);
  } else if (request.operation === "rollbackClean") {
    // Prove the old-data directory exists before deleting failed replacement.
    await pin(link(stage, "old"));
    for (const name of await fsp.readdir(link(root))) {
      if (name === request.stage) continue;
      if (name.startsWith(".ludock-restore-")) fail();
      await removeEntry(root, name);
    }
  } else if (request.operation === "rollbackOld") {
    const old = await pin(link(stage, "old"));
    await moveContents(old, root);
  } else fail();
  await sync(stage, root);
  return { ok: true };
}
main().then((value) => process.stdout.write(JSON.stringify(value))).catch(() => {
  process.stderr.write("Restore data changed, is unsafe, or cannot be accessed. Keep the server stopped and review recovery.");
  process.exitCode = 1;
}).finally(async () => { await Promise.all([...opened].map((handle) => handle.close().catch(() => {}))); });
`;
