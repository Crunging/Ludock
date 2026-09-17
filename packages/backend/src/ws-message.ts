import { decodeText } from "./bytes.js";
import type { SocketMessage } from "./socket-channel.js";

/** Bun supplies text messages as strings and binary messages as byte buffers. */
export function rawDataToString(raw: SocketMessage): string {
  if (typeof raw === "string") return raw;
  return decodeText(raw);
}
