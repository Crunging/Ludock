import { Duplex, Readable, Writable } from "node:stream";
import type { DockerConnection } from "../../src/docker-transport.js";

/** Existing helper-process fakes retain their controlled Node stream events;
 * expose the same Web Stream contract as the separately tested real transport. */
export function webConnection(stream: Readable): DockerConnection {
  return {
    readable: Readable.toWeb(stream),
    writable: stream instanceof Duplex ? Writable.toWeb(stream) : new WritableStream<Uint8Array>(),
    abort(reason) { stream.destroy(reason instanceof Error ? reason : undefined); },
  };
}

export function bytesStream(chunks: Iterable<Uint8Array>): ReadableStream<Uint8Array> {
  return ReadableStream.from(chunks);
}
