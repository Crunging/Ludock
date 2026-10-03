import { describe, expect, it } from "bun:test";
import { fixtureBytes } from "./fixtures/bytes.js";
import { byteView, concatBytes } from "../src/bytes.js";
import { ConsoleOutputRedactor } from "../src/console-redaction.js";
import { DockerLogDecoder } from "../src/container-logs.js";

describe("Docker log decoder", () => {
  it("keeps UTF-8 decoding separate for interleaved channels and empty frames", () => {
    const output = { stdout: "", stderr: "" };
    const decoder = new DockerLogDecoder((type, value) => { output[type] += value; });
    const stdout = fixtureBytes("🔑"), stderr = fixtureBytes("é");
    const bytes = concatBytes([
      frame(1, stdout.subarray(0, 2)), frame(2, stderr.subarray(0, 1)),
      frame(1, ""), frame(1, stdout.subarray(2)), frame(2, stderr.subarray(1)),
    ]);
    for (const byte of bytes) decoder.push(fixtureBytes([byte]));
    expect(decoder.end()).toBe(true);
    expect(output).toStrictEqual({ stdout: "🔑", stderr: "é" });
  });

  for (const framed of [false, true]) {
    it(`preserves and redacts Unicode credentials split across ${framed ? "Docker frames" : "TTY chunks"}`, () => {
      const output: string[] = [];
      const secret = "päss🔑word";
      const redactor = new ConsoleOutputRedactor([secret], (value) => output.push(value));
      const decoder = new DockerLogDecoder((_type, value) => redactor.push(value));
      for (const byte of fixtureBytes(`before ${secret} after`)) {
        const payload = fixtureBytes([byte]);
        decoder.push(framed ? frame(1, payload) : payload);
      }
      expect(decoder.end()).toBe(true);
      redactor.end();
      expect(output.join("")).toBe("before [redacted] after");
    });
  }
});

function frame(type: 1 | 2, value: string | Uint8Array): Uint8Array {
  const payload = fixtureBytes(value);
  const result = new Uint8Array(8 + payload.length);
  result[0] = type;
  byteView(result).setUint32(4, payload.length);
  result.set(payload, 8);
  return result;
}
