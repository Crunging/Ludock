/** Construct independent wire fixtures with standard typed arrays. */
export function fixtureBytes(value: string | Uint8Array | readonly number[] | ArrayBuffer): Uint8Array<ArrayBuffer> {
  if (typeof value === "string") return new TextEncoder().encode(value);
  if (value instanceof ArrayBuffer) return new Uint8Array(value.slice(0));
  return Uint8Array.from(value);
}

export function repeatedBytes(length: number, pattern: string): Uint8Array<ArrayBuffer> {
  const bytes = fixtureBytes(pattern);
  if (!bytes.length) throw new Error("A fixture byte pattern cannot be empty");
  const result = new Uint8Array(length);
  for (let offset = 0; offset < length; offset += bytes.length)
    result.set(bytes.subarray(0, Math.min(bytes.length, length - offset)), offset);
  return result;
}
