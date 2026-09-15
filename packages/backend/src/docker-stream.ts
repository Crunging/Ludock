export class DockerStreamError extends Error {
  constructor(readonly code: "INVALID_STREAM" | "INCOMPLETE_STREAM") {
    super(code === "INVALID_STREAM" ? "Invalid Docker stream frame" : "Incomplete Docker stream frame");
  }
}

/** Retain only the eight-byte header, never a whole Docker output frame. */
class DockerFrameDecoder {
  private header = Buffer.alloc(8);
  private headerBytes = 0;
  private remaining = 0;
  private channel = 0;

  *push(chunk: Uint8Array): Generator<{ channel: number; data: Uint8Array }> {
    let offset = 0;
    while (offset < chunk.length) {
      if (!this.remaining) {
        const count = Math.min(8 - this.headerBytes, chunk.length - offset);
        this.header.set(chunk.subarray(offset, offset + count), this.headerBytes);
        this.headerBytes += count;
        offset += count;
        if (this.headerBytes !== 8) continue;
        this.channel = this.header[0];
        this.remaining = this.header.readUInt32BE(4);
        if (![0, 1, 2].includes(this.channel) || this.header[1] || this.header[2] || this.header[3] || this.remaining > 64 * 1024 * 1024)
          throw new DockerStreamError("INVALID_STREAM");
        this.headerBytes = 0;
        if (!this.remaining) continue;
      }
      const count = Math.min(this.remaining, chunk.length - offset);
      const data = chunk.subarray(offset, offset + count);
      offset += count;
      this.remaining -= count;
      yield { channel: this.channel, data };
    }
  }

  finish(): void {
    if (this.headerBytes || this.remaining) throw new DockerStreamError("INCOMPLETE_STREAM");
  }
}

type Output = (chunk: Uint8Array) => void | Promise<void>;

/** Completion includes consumers' writes; failure cancels the source. */
export async function demuxDockerStream(
  source: ReadableStream<Uint8Array>,
  stdout?: Output,
  stderr?: Output,
): Promise<void> {
  const decoder = new DockerFrameDecoder();
  const reader = source.getReader();
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      for (const { channel, data } of decoder.push(value)) {
        const pending = (channel === 1 ? stdout : channel === 2 ? stderr : undefined)?.(data);
        if (pending) await pending;
      }
    }
    decoder.finish();
  } finally {
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

/** HTTP and tar consumers read stdout directly with Web Stream backpressure. */
export function dockerStdout(source: ReadableStream<Uint8Array>): ReadableStream<Uint8Array> {
  const decoder = new DockerFrameDecoder();
  return source.pipeThrough(new TransformStream<Uint8Array, Uint8Array>({
    transform(chunk, controller) {
      for (const { channel, data } of decoder.push(chunk))
        if (channel === 1) controller.enqueue(data);
    },
    flush() { decoder.finish(); },
  }, new ByteLengthQueuingStrategy({ highWaterMark: 65_536 }),
  new ByteLengthQueuingStrategy({ highWaterMark: 65_536 })));
}
