import { fixtureBytes } from "./fixtures/bytes.js";
import { describe, expect, it } from "bun:test";
import { JsonLineDecoder } from "../src/json-lines.js";

describe("bounded native JSON records", () => {
  it("limits each record by bytes while accepting a larger chunk of small records", () => {
    let records = 0;
    const decoder = new JsonLineDecoder(() => { records++; }, false, 16);
    decoder.push(fixtureBytes('{"ok":true}\n'.repeat(100)));
    expect(records).toBe(100);
    expect(() => decoder.push(fixtureBytes('{"v":"🌍🌍🌍"}\n'))).toThrow("exceeded");
  });

  it("owns fragmented bytes when the source reuses its buffer", () => {
    const expected = [{ message: "é🌍".repeat(10_000) }, { message: "next" }];
    const input = fixtureBytes(expected.map(value => JSON.stringify(value)).join("\n"));
    const values: unknown[] = [];
    const decoder = new JsonLineDecoder(value => values.push(value));
    const reused = new Uint8Array(257);
    for (let offset = 0; offset < input.length; offset += reused.length) {
      const part = input.subarray(offset, offset + reused.length);
      reused.set(part);
      decoder.push(reused.subarray(0, part.length));
      reused.fill(0);
    }
    decoder.end();
    decoder.push(fixtureBytes('{"message":"after end"}\n'));
    decoder.end();
    expect(values).toEqual([...expected, { message: "after end" }]);
  });

  for (const suffix of ["", "\n{\"ok\":true}\n"]) {
    it(`rejects oversized fragmented records even when malformed events may be skipped (${suffix ? "terminated" : "unfinished"})`, () => {
      const values: unknown[] = [];
      const decoder = new JsonLineDecoder(value => values.push(value), true, 16);
      decoder.push(fixtureBytes('{"v":"'));
      decoder.push(fixtureBytes("🌍🌍"));
      expect(() => decoder.push(fixtureBytes(`x"}${suffix}`))).toThrow("exceeded");
      expect(values).toEqual([]);
    });
  }
});
