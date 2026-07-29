import type { RawData } from "ws";

/**
 * Decode a WebSocket payload as UTF-8 text.
 *
 * `RawData` is `Buffer | ArrayBuffer | Buffer[]`, and calling `toString()` on
 * it directly only behaves for the `Buffer` case: a fragmented message arrives
 * as `Buffer[]` and stringifies to comma-joined garbage, while an `ArrayBuffer`
 * yields "[object ArrayBuffer]".
 */
export function rawDataToString(raw: RawData): string {
  if (Array.isArray(raw)) return Buffer.concat(raw).toString("utf8");
  if (Buffer.isBuffer(raw)) return raw.toString("utf8");
  return Buffer.from(raw).toString("utf8");
}
