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
});
