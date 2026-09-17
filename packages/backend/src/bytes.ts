const encoder = new TextEncoder();
// Protocol text includes an initial BOM as data, matching incremental decoders.
const decoder = new TextDecoder("utf-8", { ignoreBOM: true });

export function encodeText(value: string): Uint8Array<ArrayBuffer> {
  return encoder.encode(value);
}

export function decodeText(value: Uint8Array | ArrayBuffer): string {
  return decoder.decode(value);
}

export function concatBytes(chunks: readonly Uint8Array[]): Uint8Array<ArrayBuffer> {
  return new Uint8Array(Bun.concatArrayBuffers([...chunks]));
}

/** Respect views into socket chunks as well as standalone allocations. */
export function byteView(bytes: Uint8Array): DataView {
  return new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
}
