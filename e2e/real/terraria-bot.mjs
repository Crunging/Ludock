// A headless Terraria player for the real rig: it joins the server on
// 127.0.0.1:7777, spawns, holds its slot, and prints what it hears, one
// timestamped line each. It speaks just enough of the game's protocol to
// join; the server needs no account or password.
import { connect } from "node:net";
import { randomUUID } from "node:crypto";

const [name = "RigBot", releaseArg] = process.argv.slice(2);
const log = (...parts) => console.log(new Date().toISOString(), name, ...parts);

// .NET's BinaryWriter strings: a 7-bit encoded length, then UTF-8.
function string(text) {
  const bytes = Buffer.from(text, "utf8");
  const length = [];
  for (let n = bytes.length; ; n >>= 7) {
    length.push((n & 0x7f) | (n > 0x7f ? 0x80 : 0));
    if (n <= 0x7f) break;
  }
  return Buffer.concat([Buffer.from(length), bytes]);
}

function readString(buffer, offset) {
  let length = 0;
  let shift = 0;
  for (;;) {
    const byte = buffer[offset++];
    length |= (byte & 0x7f) << shift;
    shift += 7;
    if (!(byte & 0x80)) break;
  }
  return [buffer.toString("utf8", offset, offset + length), offset + length];
}

// NetworkText: a mode byte, then the text; formatted text adds substitutions.
function networkText(buffer, offset) {
  const mode = buffer[offset++];
  let text;
  [text, offset] = readString(buffer, offset);
  if (mode !== 0) {
    const count = buffer[offset++];
    const parts = [];
    for (let index = 0; index < count; index++) {
      let part;
      [part, offset] = networkText(buffer, offset);
      parts.push(part);
    }
    if (parts.length) text += ` (${parts.join(", ")})`;
  }
  return [text, offset];
}

const u8 = (...values) => Buffer.from(values);
const i16 = (value) => { const b = Buffer.alloc(2); b.writeInt16LE(value); return b; };
const i32 = (value) => { const b = Buffer.alloc(4); b.writeInt32LE(value); return b; };

function join(release) {
  return new Promise((resolve) => {
    const socket = connect(7777, "127.0.0.1");
    let pending = Buffer.alloc(0);
    let slot = -1;
    let spawned = false;
    const send = (type, ...parts) => {
      const payload = Buffer.concat(parts);
      const header = Buffer.alloc(3);
      header.writeUInt16LE(payload.length + 3);
      header[2] = type;
      socket.write(Buffer.concat([header, payload]));
    };
    socket.on("connect", () => send(1, string(`Terraria${release}`)));
    socket.on("data", (chunk) => {
      pending = Buffer.concat([pending, chunk]);
      while (pending.length >= 3 && pending.length >= pending.readUInt16LE(0)) {
        const length = pending.readUInt16LE(0);
        const type = pending[2];
        const body = pending.subarray(3, length);
        pending = pending.subarray(length);
        if (type === 2) {
          const [reason] = networkText(body, 0);
          socket.destroy();
          resolve({ refused: reason });
          return;
        }
        if (type === 3 && slot < 0) {
          slot = body[0];
          log(`connected as Terraria${release}, slot ${slot}`);
          const color = u8(128, 128, 128);
          // Player info for release 326 (1.4.5): slot, seven bytes of looks
          // before the name, then hair dye, hidden accessories, seven
          // colors, and flags, all left at 0 or gray.
          send(4, u8(slot, 0, 0, 0, 0, 0, 0, 0), string(name), u8(0, 0, 0, 0), color, color, color, color, color, color, color, u8(0, 0, 0));
          send(68, string(randomUUID()));
          send(16, u8(slot), i16(100), i16(100));
          send(42, u8(slot), i16(20), i16(20));
          send(50, u8(slot), Buffer.alloc(44 * 2));
          send(6);
        }
        if (type === 7 && !spawned) send(8, i32(-1), i32(-1));
        if (type === 49 && !spawned) {
          spawned = true;
          send(12, u8(slot), i16(-1), i16(-1), i32(0), i16(0), i16(0), u8(0));
          log("joined");
          resolve({ socket });
        }
        // Chat and server messages, such as a warning before a restart.
        if (type === 107) {
          const [text] = networkText(body, 3);
          log("[message]", text);
        }
        if (type === 82) {
          // Net module 1 is chat: module ID, author slot, NetworkText, color.
          if (body.readUInt16LE(0) === 1) {
            const [text] = networkText(body, 3);
            log("[chat]", text);
          }
        }
      }
    });
    socket.on("close", () => {
      if (spawned) {
        log("disconnected");
        process.exit(0);
      }
      resolve({ refused: "connection closed" });
    });
    socket.on("error", (error) => log("error:", error.message));
  });
}

// Without a release number, try them until the server takes one.
// Terraria 1.4.5.8 is release 326.
const releases = releaseArg ? [Number(releaseArg)] : [326, ...Array.from({ length: 200 }, (_, index) => 420 - index)];
for (const release of releases) {
  const result = await join(release);
  if (result.socket) {
    const quit = () => { result.socket.end(); setTimeout(() => process.exit(0), 500); };
    process.on("SIGTERM", quit);
    process.on("SIGINT", quit);
    break;
  }
  // LegacyMultiplayer.4 is "You are not using the same version as this server."
  if (!/version|LegacyMultiplayer\.4/i.test(result.refused) || releaseArg) {
    log("refused:", result.refused);
    process.exit(1);
  }
}
