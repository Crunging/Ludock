import { describe, expect, it } from "bun:test";
import { concatBytes, encodeText } from "../src/bytes.js";
import { parseComposeFile } from "../src/compose.js";
import { encodeTarHeader, tarEnd, tarPadding, walkTar, type TarHeader } from "../src/tar.js";

// Ludock keeps the yaml package and its own streaming tar code because Bun's
// built-in Bun.YAML and Bun.Archive miss the requirements below. When a Bun
// upgrade fails one of these tests, revisit the switch rather than the test.

function aliasBomb(fanOut: number, depth: number): string {
  const levels = [`l0: &l0 [${Array(fanOut).fill("x").join(", ")}]`];
  for (let level = 1; level <= depth; level++)
    levels.push(`l${level}: &l${level} [${Array(fanOut).fill(`*l${level - 1}`).join(", ")}]`);
  return levels.join("\n");
}

describe("Bun.YAML for Compose files", () => {
  it("keeps the last duplicate key, which Compose rejects as an error", () => {
    const text = "services:\n  game:\n    image: a\n    image: b\n";
    expect(Bun.YAML.parse(text)).toStrictEqual({ services: { game: { image: "b" } } });
    expect(() => parseComposeFile(text)).toThrow(/parsed safely/);
  });

  it("drops Compose override tags without a trace, so they cannot be rejected", () => {
    const text = "services:\n  game:\n    ports: !reset []\n    image: !override other\n";
    expect(Bun.YAML.parse(text)).toStrictEqual({ services: { game: { ports: [], image: "other" } } });
    expect(() => parseComposeFile(text)).toThrow(/!reset and !override/);
  });

  it("expands a few hundred bytes of aliases into megabytes", () => {
    const text = aliasBomb(3, 12);
    expect(text.length).toBeLessThan(1000);
    expect(JSON.stringify(Bun.YAML.parse(text)).length).toBeGreaterThan(10_000_000);
    expect(() => parseComposeFile(text)).toThrow(/parsed safely/);
  });
});

function entry(header: TarHeader, body = new Uint8Array()): Uint8Array[] {
  return [encodeTarHeader({ ...header, size: body.length }), body, tarPadding(body.length)];
}

const backup = concatBytes([
  ...entry({ name: "snapshot/", type: "directory", mode: 0o750, uid: 1000, gid: 1000 }),
  ...entry({ name: "snapshot/level.dat", mode: 0o600, uid: 1000, gid: 1000 }, encodeText("level")),
  ...entry({ name: "snapshot/latest", type: "symlink", linkname: "level.dat", uid: 1000, gid: 1000 }),
  tarEnd(),
]);

async function headers(bytes: Uint8Array): Promise<Array<Pick<TarHeader, "name" | "type" | "mode" | "uid">>> {
  const result: Array<Pick<TarHeader, "name" | "type" | "mode" | "uid">> = [];
  await walkTar(new Blob([bytes]).stream(), ({ name, type, mode, uid }) => {
    result.push({ name, type, mode, uid });
  });
  return result;
}

describe("Bun.Archive for backups", () => {
  it("treats a stream as an empty set of files instead of reading it", async () => {
    // Backups arrive as Docker streams that can be larger than memory.
    const archive = new Bun.Archive(new Blob([backup]).stream() as never);
    expect((await new Bun.Archive(await archive.bytes()).files()).size).toBe(0);
    expect((await headers(backup)).map((header) => header.name))
      .toStrictEqual(["snapshot/", "snapshot/level.dat", "snapshot/latest"]);
  });

  it("omits directories, symbolic links, permissions, and ownership when reading", async () => {
    const files = await new Bun.Archive(backup).files();
    expect([...files.keys()]).toStrictEqual(["snapshot/level.dat"]);
    const file = files.get("snapshot/level.dat")!;
    expect(["mode", "uid", "gid"].some((field) => field in file)).toBe(false);
    // Restores need all three entries with their original owners and modes.
    expect(await headers(backup)).toStrictEqual([
      { name: "snapshot/", type: "directory", mode: 0o750, uid: 1000 },
      { name: "snapshot/level.dat", type: "file", mode: 0o600, uid: 1000 },
      { name: "snapshot/latest", type: "symlink", mode: 0o644, uid: 1000 },
    ]);
  });

  it("cannot record permissions or ownership when writing", async () => {
    const created = await new Bun.Archive({ "snapshot/level.dat": "level" }).bytes();
    expect(await headers(created)).toStrictEqual([
      { name: "snapshot/level.dat", type: "file", mode: 0o644, uid: 0 },
    ]);
  });

  it("returns entries from a truncated archive without reporting an error", async () => {
    const truncated = backup.subarray(0, 512 * 2 + 100);
    expect([...(await new Bun.Archive(truncated).files()).keys()]).toStrictEqual(["snapshot/level.dat"]);
    await expect(headers(truncated)).rejects.toThrow(/truncated/);
  });
});
