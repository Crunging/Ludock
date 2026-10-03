import { describe, expect, it } from "bun:test";
import { concatBytes, decodeText, encodeText } from "../src/bytes.js";
import { encodeTarHeader, tarEnd, tarPadding, walkTar } from "../src/tar.js";
import { streamFrom } from "./fixtures/web-streams.js";

const stream = (bytes: Uint8Array, width = 65_536) => streamFrom((function* () {
  for (let offset = 0; offset < bytes.length; offset += width) yield bytes.subarray(offset, offset + width);
})());
function recheck(header: Uint8Array) {
  header.fill(32, 148, 156);
  const sum = header.reduce((total, byte) => total + byte, 0);
  header.set(encodeText(sum.toString(8).padStart(6, "0") + "\0 "), 148);
  return header;
}

describe("streaming backup tar", () => {
  it("drains unselected entries and hashes every byte including terminal padding", async () => {
    const bytes = await new Bun.Archive({ "ignore": "123456789", "keep": "value" }).bytes();
    const consumed: Uint8Array[] = [];
    let selected = "";
    await walkTar(stream(bytes, 7), async (header, body) => {
      if (header.name !== "keep") return;
      const chunks: Uint8Array[] = [];
      for await (const chunk of body) chunks.push(chunk);
      selected = decodeText(concatBytes(chunks));
    }, (chunk) => { consumed.push(chunk); });
    expect(selected).toBe("value");
    expect(concatBytes(consumed)).toStrictEqual(bytes);
  });

  it("cancels the source with the destination failure and stops reading ahead", async () => {
    const failure = new Error("destination full");
    const header = encodeTarHeader({ name: "world", size: 1024 ** 3 });
    let pulls = 0, cancelled: unknown;
    const source = new ReadableStream<Uint8Array>({
      pull(controller) { controller.enqueue(pulls++ === 0 ? header : new Uint8Array(65_536)); },
      cancel(reason) { cancelled = reason; },
    }, { highWaterMark: 0 });
    await expect(walkTar(source, async (_header, body) => {
      for await (const _chunk of body) { void _chunk; throw failure; }
    })).rejects.toThrow("destination full");
    expect(pulls).toBe(2);
    expect(cancelled).toBe(failure);
    expect(source.locked).toBe(false);
  });

  it("rejects truncated bodies, headers, footer blocks, and nonzero trailing data", async () => {
    const header = encodeTarHeader({ name: "world", size: 4 });
    const body = encodeText("data");
    const complete = concatBytes([header, body, tarPadding(4), tarEnd()]);
    for (const bytes of [
      new Uint8Array(), complete.subarray(0, 511), complete.subarray(0, 514),
      complete.subarray(0, -512), complete.subarray(0, -1),
      concatBytes([complete, new Uint8Array(512).fill(1)]),
    ]) await expect(walkTar(stream(bytes), () => {})).rejects.toThrow(/archive/);
  });

  it("rejects corrupt checksums, unsupported formats, and special-entry bodies", async () => {
    for (const mutate of [
      (header: Uint8Array) => { header[0] ^= 1; },
      (header: Uint8Array) => { header[257] = 0; recheck(header); },
      (header: Uint8Array) => { header[156] = 83; recheck(header); }, // GNU sparse
      (header: Uint8Array) => { header[156] = 53; recheck(header); }, // directory with bytes
      (header: Uint8Array) => { header[124] = 255; recheck(header); }, // negative base-256 size
    ]) {
      const header = encodeTarHeader({ name: "world", size: 4 });
      mutate(header);
      await expect(walkTar(stream(concatBytes([header, encodeText("data"), tarPadding(4), tarEnd()])), () => {})).rejects.toThrow(/archive/);
    }
  });

  it("bounds PAX metadata and rejects malformed lengths and dangling overrides", async () => {
    const metadata = (body: string) => concatBytes([
      encodeTarHeader({ name: "PaxHeader", type: "pax-header", size: encodeText(body).length }),
      encodeText(body), tarPadding(encodeText(body).length),
      encodeTarHeader({ name: "world" }), tarEnd(),
    ]);
    for (const body of ["0 path=world\n", "999 path=world\n", "14 path=world!", "14 size=-100\n"])
      await expect(walkTar(stream(metadata(body)), () => {})).rejects.toThrow(/archive/);
    await expect(walkTar(stream(concatBytes([
      encodeTarHeader({ name: "PaxHeader", type: "pax-header", size: 16_385 }),
    ])), () => {})).rejects.toThrow(/archive/);
    const dangling = encodeTarHeader({ name: "world", pax: { path: "changed" } });
    await expect(walkTar(stream(concatBytes([dangling.subarray(0, -512), tarEnd()])), () => {})).rejects.toThrow(/archive/);
  });
});
