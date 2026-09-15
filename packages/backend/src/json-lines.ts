import { concatBytes } from "./bytes.js";
/** Keep wire record boundaries and byte limits around Bun's native parser.
 * Incomplete final records and partial results from malformed input must never
 * be mistaken for a successful Docker pull. Event feeds may skip bad records. */
export class JsonLineDecoder {
  private pending: Uint8Array = new Uint8Array(0);

  constructor(
    private readonly receive: (value: unknown) => void,
    private readonly skipInvalid = false,
    private readonly maxBytes = 1_048_576,
  ) {}

  push(chunk: Uint8Array): void {
    if (this.pending.length) chunk = concatBytes([this.pending, chunk]);
    let start = 0;
    let newline: number;
    while ((newline = chunk.indexOf(10, start)) !== -1) {
      this.record(chunk.subarray(start, newline));
      start = newline + 1;
    }
    if (chunk.length - start > this.maxBytes) throw new Error("JSON line exceeded its limit");
    this.pending = Uint8Array.from(chunk.subarray(start));
  }

  end(): void {
    this.record(this.pending);
    this.pending = new Uint8Array(0);
  }

  private record(bytes: Uint8Array): void {
    if (bytes.length > this.maxBytes) throw new Error("JSON line exceeded its limit");
    if (!bytes.length) return;
    const result = Bun.JSONL.parseChunk(bytes);
    if (result.error || !result.done || result.values.length > 1) {
      if (this.skipInvalid) return;
      throw new Error("Invalid JSON line");
    }
    if (result.values.length) this.receive(result.values[0]);
  }
}
