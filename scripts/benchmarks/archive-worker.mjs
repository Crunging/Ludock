import { mkdtemp, rm } from "node:fs/promises";
// Keep one fixture encoder for both versions. This module is not the subject
// being measured; writeSnapshot is loaded from the requested checkout below.
import { encodeTarHeader, tarPadding, tarEnd } from "../../packages/backend/src/tar.ts";

const [source, scenario] = process.argv.slice(2);
const { writeSnapshot } = await import(source + "/backup-storage.ts");
const destination = await mkdtemp("/tmp/ludock-archive-benchmark-");
process.env.LUDOCK_BACKUP_ROOTS = destination;
const chunk = new Uint8Array(65_536).fill(65);
const count = scenario === "small" ? 10_000 : 1;
const size = scenario === "small" ? 1024 : 128 * 1024 * 1024;
function* tar() {
  yield encodeTarHeader({ name: "data/", type: "directory", mode: 0o755 });
  for (let n = 0; n < count; n++) {
    yield encodeTarHeader({ name: "data/file-" + n, type: "file", size, mode: 0o644 });
    for (let i = 0; i < size; i += chunk.length) yield chunk.subarray(0, Math.min(chunk.length, size - i));
    const pad = tarPadding(size);
    if (pad.length) yield pad;
  }
  yield tarEnd();
}
function* frames() {
  for (const bytes of tar()) {
    const frame = new Uint8Array(bytes.length + 8);
    frame[0] = 1;
    new DataView(frame.buffer).setUint32(4, bytes.length);
    frame.set(bytes, 8);
    yield frame;
  }
}
const context = { observation: { mounts: [{ type: "volume", name: "fixture", source: "/volumes/fixture", destination: "/data", writable: true }] } };
const helper = { roots: [{ id: "root-0", path: "/data" }], container: { exec: async () => ({
  start: async () => ({ readable: ReadableStream.from(frames()), abort() {} }),
  inspect: async () => ({ ExitCode: 0, Running: false }),
}) } };
try {
  const id = crypto.randomUUID();
  Bun.gc(true);
  const start = performance.now();
  const result = await writeSnapshot(context, helper, { destination, reserveBytes: 0 }, id, 512 * 1024 * 1024, async () => {});
  const ms = performance.now() - start;
  // Verify publication and the returned checksum outside the measured interval.
  const archive = Bun.file(destination + "/" + id + ".tar");
  const hash = new Bun.CryptoHasher("sha256");
  for await (const bytes of archive.stream()) hash.update(bytes);
  if (archive.size !== result.size || hash.digest("hex") !== result.checksum) throw new Error("Invalid archive benchmark output");
  console.log(JSON.stringify({ bun: Bun.version, ms, files: count, inputBytes: count * size, archiveBytes: result.size }));
} finally { await rm(destination, { recursive: true, force: true }); }
