/** Keep wire record boundaries and byte limits around Bun's native parser.
 * Incomplete final records and partial results from malformed input must never
 * be mistaken for a successful Docker pull. Event feeds may skip bad records. */
export class JsonLineDecoder {
  private pending: Uint8Array = new Uint8Array(0);
  private pendingBytes = 0;

  constructor(
    private readonly receive: (value: unknown) => void,
    private readonly skipInvalid = false,
    private readonly maxBytes = 1_048_576,
  ) {}

  push(chunk: Uint8Array): void {
    let start = 0;
    let newline: number;
    while ((newline = chunk.indexOf(10, start)) !== -1) {
      const part = chunk.subarray(start, newline);
      if (this.pendingBytes) {
        this.append(part);
        const bytes = this.pending.subarray(0, this.pendingBytes);
        this.pendingBytes = 0;
        this.record(bytes);
      } else this.record(part);
      start = newline + 1;
    }
    if (start < chunk.length) this.append(chunk.subarray(start));
  }

  end(): void {
    this.record(this.pending.subarray(0, this.pendingBytes));
    this.pendingBytes = 0;
  }

  private append(part: Uint8Array): void {
    const size = this.pendingBytes + part.length;
    if (size > this.maxBytes) throw new Error("JSON line exceeded its limit");
    if (size > this.pending.length) {
      // Grow geometrically instead of copying every earlier fragment on each
      // read. Capacity stays bounded by the same per-record byte limit.
      const grown = new Uint8Array(Math.min(this.maxBytes, Math.max(size, this.pending.length * 2, 256)));
      grown.set(this.pending.subarray(0, this.pendingBytes));
      this.pending = grown;
    }
    this.pending.set(part, this.pendingBytes);
    this.pendingBytes = size;
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
