import { fixtureBytes } from "./fixtures/bytes.js";
import { describe, expect, it } from "bun:test";
import { JsonLineDecoder } from "../src/json-lines.js";

describe("bounded native JSON records", () => {
  it("preserves bytewise UTF-8 and checks the final record without a newline", () => {
    const values: unknown[] = [];
    const decoder = new JsonLineDecoder(value => values.push(value));
    const input = fixtureBytes('{"name":"café 🌍"}\r\n \n{"status":"done"}');
    for (const byte of input) decoder.push(Uint8Array.of(byte));
    decoder.end();
    expect(values).toEqual([{ name: "café 🌍" }, { status: "done" }]);
  });

  for (const input of ['{"ok":true}\n{"status":', '{"ok":true} {"other":false}\n', '{"ok":true}garbage\n']) {
    it("rejects incomplete or extra data instead of accepting native partial results: " + input, () => {
      const decoder = new JsonLineDecoder(() => {});
      expect(() => { decoder.push(fixtureBytes(input)); decoder.end(); }).toThrow("Invalid JSON line");
    });
  }

  it("limits each record by bytes while accepting a larger chunk of small records", () => {
    let records = 0;
    const decoder = new JsonLineDecoder(() => { records++; }, false, 16);
    decoder.push(fixtureBytes('{"ok":true}\n'.repeat(100)));
    expect(records).toBe(100);
    expect(() => decoder.push(fixtureBytes('{"v":"🌍🌍🌍"}\n'))).toThrow("exceeded");
  });

  it("skips malformed events without losing subsequent valid records", () => {
    const values: unknown[] = [];
    const decoder = new JsonLineDecoder(value => values.push(value), true);
    decoder.push(fixtureBytes('broken\n{"Action":"start"}\n{"bad":\n{"Action":"stop"}\n'));
    expect(values).toEqual([{ Action: "start" }, { Action: "stop" }]);
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

  it("accepts a fragmented record exactly at its byte limit before more complete records", () => {
    const input = fixtureBytes('{"v":"🌍🌍"}');
    expect(input.length).toBe(16);
    const values: unknown[] = [];
    const decoder = new JsonLineDecoder(value => values.push(value), false, input.length);
    decoder.push(input.subarray(0, 5));
    decoder.push(input.subarray(5));
    decoder.push(fixtureBytes('\n{"ok":true}\n'.repeat(10)));
    decoder.end();
    expect(values).toEqual([{ v: "🌍🌍" }, ...Array.from({ length: 10 }, () => ({ ok: true }))]);
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

  it("resets fragmented malformed events before parsing the next record", () => {
    const values: unknown[] = [];
    const decoder = new JsonLineDecoder(value => values.push(value), true);
    for (const byte of fixtureBytes('broken\n{"Action":"start"}\n{"bad":\n{"Action":"stop"}'))
      decoder.push(Uint8Array.of(byte));
    decoder.end();
    expect(values).toEqual([{ Action: "start" }, { Action: "stop" }]);
  });
});
