import { concatBytes, encodeText } from "./bytes.js";

/** Uncompressed USTAR/PAX, consumed one entry at a time. Game backups can be
 * larger than memory; Bun.Archive.files() cannot provide this streaming contract
 * or the ownership metadata needed by restore. No paths are extracted here. */
export interface TarHeader {
  name: string;
  type?: string;
  size?: number;
  mode?: number;
  uid?: number;
  gid?: number;
  mtime?: Date;
  linkname?: string;
  pax?: Record<string, string> | null;
}

const blockSize = 512;
const maxMetadataBytes = 16_384;
const decoder = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });
const invalid = () => new Error("Invalid or truncated backup archive");
const padding = (size: number) => (blockSize - size % blockSize) % blockSize;
const types: Record<string, string> = {
  "0": "file", "1": "link", "2": "symlink", "3": "character-device",
  "4": "block-device", "5": "directory", "6": "fifo", "7": "contiguous-file",
  x: "pax-header", g: "pax-global-header",
};

function textField(bytes: Uint8Array): string {
  const end = bytes.indexOf(0);
  return decoder.decode(end < 0 ? bytes : bytes.subarray(0, end));
}

function integer(bytes: Uint8Array): number {
  let value: number;
  if (bytes[0] & 128) {
    // The helper uses positive base-256 for large sizes and Linux owner IDs.
    if (bytes[0] !== 128) throw invalid();
    let wide = 0n;
    for (const byte of bytes.subarray(1)) wide = (wide << 8n) | BigInt(byte);
    value = Number(wide);
  } else {
    const encoded = textField(bytes).trim();
    if (encoded && !/^[0-7]+$/.test(encoded)) throw invalid();
    value = encoded ? Number.parseInt(encoded, 8) : 0;
  }
  if (!Number.isSafeInteger(value) || value < 0) throw invalid();
  return value;
}

export function decodeTarHeader(block: Uint8Array, pathOverride?: string): TarHeader {
  if (block.length !== blockSize) throw invalid();
  const checksum = integer(block.subarray(148, 156));
  let actual = 8 * 32;
  for (let i = 0; i < block.length; i++) if (i < 148 || i >= 156) actual += block[i];
  if (checksum !== actual || textField(block.subarray(257, 263)).trim() !== "ustar") throw invalid();
  const flag = block[156] === 0 ? "0" : String.fromCharCode(block[156]);
  if (!types[flag]) throw invalid();
  // PAX producers may truncate the diagnostic USTAR name within a UTF-8 code
  // point. Only the complete PAX path is meaningful for such an entry.
  const overridden = pathOverride ?? (flag === "x" || flag === "g" ? "PaxHeader" : undefined);
  const prefix = overridden === undefined ? textField(block.subarray(345, 500)) : "";
  const name = overridden ?? textField(block.subarray(0, 100));
  return {
    name: prefix ? `${prefix}/${name}` : name,
    type: types[flag],
    mode: integer(block.subarray(100, 108)),
    uid: integer(block.subarray(108, 116)),
    gid: integer(block.subarray(116, 124)),
    size: integer(block.subarray(124, 136)),
    mtime: new Date(integer(block.subarray(136, 148)) * 1000),
    linkname: textField(block.subarray(157, 257)),
  };
}

function parsePax(bytes: Uint8Array): Record<string, string> {
  const result: Record<string, string> = Object.create(null) as Record<string, string>;
  let offset = 0;
  while (offset < bytes.length) {
    const space = bytes.indexOf(32, offset);
    if (space < offset || space - offset > 8) throw invalid();
    const encoded = decoder.decode(bytes.subarray(offset, space));
    if (!/^[1-9]\d*$/.test(encoded)) throw invalid();
    const end = offset + Number(encoded);
    if (end > bytes.length || end <= space + 3 || bytes[end - 1] !== 10) throw invalid();
    const record = decoder.decode(bytes.subarray(space + 1, end - 1));
    const equals = record.indexOf("=");
    if (equals < 1 || record.includes("\0")) throw invalid();
    const key = record.slice(0, equals);
    if (Object.hasOwn(result, key) || key.startsWith("GNU.sparse") || key === "SCHILY.filetype") throw invalid();
    result[key] = record.slice(equals + 1);
    offset = end;
  }
  return result;
}

function applyPax(header: TarHeader, pax: Record<string, string>): TarHeader {
  const result = { ...header, pax };
  if (pax.path !== undefined) result.name = pax.path;
  if (pax.linkpath !== undefined) result.linkname = pax.linkpath;
  for (const key of ["size", "uid", "gid"] as const) {
    if (pax[key] === undefined) continue;
    if (!/^\d+$/.test(pax[key])) throw invalid();
    const value = Number(pax[key]);
    if (!Number.isSafeInteger(value)) throw invalid();
    result[key] = value;
  }
  if (pax.mtime !== undefined) {
    if (!/^-?\d+(?:\.\d+)?$/.test(pax.mtime)) throw invalid();
    result.mtime = new Date(Number(pax.mtime) * 1000);
  }
  if (!Number.isFinite(result.mtime!.getTime())) throw invalid();
  return result;
}

/** The visitor must finish using body before returning. Unread bytes are drained
 * before the next header. Awaiting each visitor bounds memory and propagates
 * destination failures back to Docker/file stream cancellation. */
export async function walkTar(
  source: ReadableStream<Uint8Array>,
  visit: (header: TarHeader, body: AsyncIterable<Uint8Array>) => void | Promise<void>,
  onChunk?: (chunk: Uint8Array) => void,
): Promise<void> {
  const reader = source.getReader();
  let pending = new Uint8Array(0) as Uint8Array;
  let eof = false;
  let failure: unknown;
  const take = async (limit: number): Promise<Uint8Array> => {
    while (!pending.length && !eof) {
      const next = await reader.read();
      eof = Boolean(next.done);
      if (!next.done) { onChunk?.(next.value); pending = next.value; }
    }
    const bytes = pending.subarray(0, limit);
    pending = pending.subarray(bytes.length);
    return bytes;
  };
  const exact = async (size: number): Promise<Uint8Array> => {
    const bytes = new Uint8Array(size);
    let offset = 0;
    while (offset < size) {
      const part = await take(size - offset);
      if (!part.length) throw invalid();
      bytes.set(part, offset);
      offset += part.length;
    }
    return bytes;
  };
  let globalPax: Record<string, string> = {};
  let localPax: Record<string, string> | undefined;
  let metadataCount = 0;
  try {
    while (true) {
      const block = await exact(blockSize);
      if (block.every((byte) => byte === 0)) {
        if (localPax || !(await exact(blockSize)).every((byte) => byte === 0)) throw invalid();
        // Include all terminal padding in the caller's checksum/byte budget.
        let trailing = 0;
        while (!eof) {
          const bytes = await take(65_536);
          trailing += bytes.length;
          if (bytes.some((byte) => byte !== 0)) throw invalid();
        }
        if (trailing % blockSize) throw invalid();
        return;
      }
      let header = decodeTarHeader(block, localPax?.path ?? globalPax.path);
      if (header.type === "pax-header" || header.type === "pax-global-header") {
        const size = header.size!;
        if (size > maxMetadataBytes || ++metadataCount > 100_000 || localPax) throw invalid();
        const pax = parsePax(await exact(size));
        if ((await exact(padding(size))).some((byte) => byte !== 0)) throw invalid();
        if (header.type === "pax-global-header") {
          globalPax = { ...globalPax, ...pax };
          if (encodeText(JSON.stringify(globalPax)).length > maxMetadataBytes) throw invalid();
        }
        else localPax = pax;
        continue;
      }
      header = applyPax(header, { ...globalPax, ...localPax });
      localPax = undefined;
      if (header.type !== "file" && header.size !== 0) throw invalid();
      let remaining = header.size!;
      let active = true;
      const body: AsyncIterable<Uint8Array> = {
        async *[Symbol.asyncIterator]() {
          while (remaining) {
            if (!active) throw new Error("Archive entry is no longer active");
            const part = await take(Math.min(remaining, 65_536));
            if (!part.length) throw invalid();
            remaining -= part.length;
            yield part;
          }
        },
      };
      await visit(header, body);
      for await (const part of body) { void part; /* Drain an unselected entry. */ }
      active = false;
      if ((await exact(padding(header.size!))).some((byte) => byte !== 0)) throw invalid();
    }
  } catch (error) {
    failure = error;
    throw error;
  } finally {
    try { if (!eof) await reader.cancel(failure).catch(() => {}); }
    finally { reader.releaseLock(); }
  }
}

function paxRecord(key: string, value: string): Uint8Array {
  const record = ` ${key}=${value}\n`;
  const bytes = encodeText(record).length;
  let size = bytes + 1;
  while (String(size).length + bytes !== size) size = String(size).length + bytes;
  return encodeText(String(size) + record);
}

function rawHeader(header: TarHeader, flag: string): Uint8Array {
  const bytes = new Uint8Array(blockSize);
  const text = (value: string, offset: number, length: number) => {
    const encoded = encodeText(value);
    if (encoded.length > length || value.includes("\0")) throw invalid();
    bytes.set(encoded, offset);
  };
  const number = (value: number, offset: number, length: number) => {
    if (!Number.isSafeInteger(value) || value < 0) throw invalid();
    const octal = value.toString(8);
    if (octal.length < length) text(octal.padStart(length - 1, "0"), offset, length);
    else {
      let wide = BigInt(value);
      for (let i = offset + length - 1; i > offset; i--) { bytes[i] = Number(wide & 255n); wide >>= 8n; }
      if (wide) throw invalid();
      bytes[offset] = 128;
    }
  };
  text(header.name, 0, 100);
  number(header.mode ?? 0o644, 100, 8);
  number(header.uid ?? 0, 108, 8);
  number(header.gid ?? 0, 116, 8);
  number(header.size ?? 0, 124, 12);
  number(Math.max(0, Math.floor((header.mtime?.getTime() ?? 0) / 1000)), 136, 12);
  bytes.fill(32, 148, 156);
  text(flag, 156, 1);
  text(header.linkname ?? "", 157, 100);
  text("ustar", 257, 6);
  text("00", 263, 2);
  const sum = bytes.reduce((total, byte) => total + byte, 0);
  text(sum.toString(8).padStart(6, "0"), 148, 6);
  bytes[154] = 0;
  return bytes;
}

export function encodeTarHeader(header: TarHeader): Uint8Array {
  const flag = Object.entries(types).find(([, type]) => type === (header.type ?? "file"))?.[0];
  if (!flag || encodeText(header.name).length > 4096) throw invalid();
  const pax = { ...header.pax };
  if (encodeText(header.name).length > 100) pax.path ??= header.name;
  const mtime = (header.mtime?.getTime() ?? 0) / 1000;
  if (mtime < 0 || !Number.isInteger(mtime)) pax.mtime ??= String(mtime);
  const prefix: Uint8Array[] = [];
  if (Object.keys(pax).length) {
    const records = concatBytes(Object.entries(pax).map(([key, value]) => paxRecord(key, value)));
    if (records.length > maxMetadataBytes) throw invalid();
    prefix.push(rawHeader({ name: "PaxHeader", size: records.length }, "x"), records, new Uint8Array(padding(records.length)));
  }
  return concatBytes([...prefix, rawHeader({ ...header, name: pax.path ? "entry" : header.name }, flag)]);
}

export const tarPadding = (size: number): Uint8Array => new Uint8Array(padding(size));
export const tarEnd = (): Uint8Array => new Uint8Array(2 * blockSize);
