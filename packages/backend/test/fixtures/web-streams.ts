import { encodeText } from "../../src/bytes.js";
import type { DockerConnection } from "../../src/docker-transport.js";

/** A controlled peer: stdout and stdin are separate native streams. */
export class StreamFixture {
  private controller!: ReadableStreamDefaultController<Uint8Array>;
  closed = false;
  cancelled = false;
  stdinClosed = false;
  readonly received: Uint8Array[] = [];
  onInput?: (chunk: Uint8Array) => void;
  onInputEnd?: () => void;
  readonly readable = new ReadableStream<Uint8Array>({
    start: controller => { this.controller = controller; },
    cancel: () => { this.cancelled = true; this.closed = true; },
  }, new ByteLengthQueuingStrategy({ highWaterMark: 65_536 }));
  readonly connection: DockerConnection = {
    readable: this.readable,
    writable: new WritableStream<Uint8Array>({
      write: chunk => {
        if (this.cancelled) throw new Error("Fixture connection cancelled");
        const bytes = Uint8Array.from(chunk);
        this.received.push(bytes);
        this.onInput?.(bytes);
      },
      close: () => { this.stdinClosed = true; this.onInputEnd?.(); },
      abort: reason => { this.cancel(reason); },
    }),
    abort: reason => { this.cancel(reason); },
  };

  enqueue(value: string | Uint8Array): void {
    if (!this.closed) this.controller.enqueue(typeof value === "string" ? encodeText(value) : value);
  }

  close(value?: string | Uint8Array): void {
    if (this.closed) return;
    if (value !== undefined) this.enqueue(value);
    this.closed = true;
    this.controller.close();
  }

  fail(reason: unknown): void {
    if (this.closed) return;
    this.closed = true;
    this.controller.error(reason);
  }

  cancel(reason?: unknown): void {
    this.cancelled = true;
    this.fail(reason ?? new Error("Fixture connection cancelled"));
  }
}

export function bytesStream(chunks: Iterable<Uint8Array>): ReadableStream<Uint8Array> {
  return streamFrom(chunks);
}

/** Bun implements ReadableStream.from, but its bundled types do not declare it. */
export function streamFrom<T>(source: Iterable<T> | AsyncIterable<T>): ReadableStream<T> {
  return (ReadableStream as unknown as {
    from(source: Iterable<T> | AsyncIterable<T>): ReadableStream<T>;
  }).from(source);
}
