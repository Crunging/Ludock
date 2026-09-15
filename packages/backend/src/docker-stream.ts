import type { Readable, Writable } from "node:stream";

export class DockerStreamError extends Error {
  constructor(readonly code: "INVALID_STREAM" | "INCOMPLETE_STREAM") {
    super(code === "INVALID_STREAM" ? "Invalid Docker stream frame" : "Incomplete Docker stream frame");
  }
}

/** Decode non-TTY Engine output without buffering whole frames. Completion
 * includes writable backpressure, and a truncated frame always fails. */
export async function demuxDockerStream(
  source: Readable,
  stdout: Writable,
  stderr?: Writable,
): Promise<void> {
  let header = Buffer.alloc(0);
  let remaining = 0;
  let channel = 0;
  for await (const value of source) {
    const chunk = value as Buffer;
    let offset = 0;
    while (offset < chunk.length) {
      if (!remaining) {
        const count = Math.min(8 - header.length, chunk.length - offset);
        header = Buffer.concat([header, chunk.subarray(offset, offset + count)]);
        offset += count;
        if (header.length !== 8) continue;
        channel = header[0];
        remaining = header.readUInt32BE(4);
        if (![0, 1, 2].includes(channel) || header[1] || header[2] || header[3] || remaining > 64 * 1024 * 1024)
          throw new DockerStreamError("INVALID_STREAM");
        header = Buffer.alloc(0);
        if (!remaining) continue;
      }
      const count = Math.min(remaining, chunk.length - offset);
      const output = channel === 1 ? stdout : channel === 2 ? stderr : undefined;
      if (output && count) await writeOutput(output, chunk.subarray(offset, offset + count));
      offset += count;
      remaining -= count;
    }
  }
  if (header.length || remaining) throw new DockerStreamError("INCOMPLETE_STREAM");
}

function writeOutput(output: Writable, chunk: Buffer): Promise<void> {
  return new Promise((resolve, reject) => {
    const closed = () => finish(new Error("Docker output consumer closed"));
    const finish = (error?: Error | null) => {
      output.off("error", finish);
      output.off("close", closed);
      if (error) reject(error);
      else resolve();
    };
    if (output.destroyed) { closed(); return; }
    output.once("error", finish);
    output.once("close", closed);
    output.write(chunk, finish);
  });
}
