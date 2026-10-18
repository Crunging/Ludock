import { encodeText } from "./bytes.js";
import { createLogger } from "./logger.js";
import fs from "node:fs";
import path from "node:path";
import type { Database } from "bun:sqlite";

const logger = createLogger("identity-key");
const KEY_BYTES = 32;
const KEY_DIRECTORY_MODE = 0o700;
const KEY_FILE_MODE = 0o600;
const KEY_CHECK_SETTING = "identity.key-check";
const KEY_FILENAME = "key";

function keyError(): Error {
  return new Error(
    "Ludock's identity key is missing, unsafe, or does not match this database. Restore the matching .identity-key directory beside the database from your application-data backup; do not generate a replacement key.",
  );
}

/** Says exactly what to fix, so it must not be replaced by the generic error. */
class KeyPermissionError extends Error {}

function ownedByProcess(info: fs.Stats): boolean {
  return !process.getuid || info.uid === process.getuid();
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`;
}

/** Rejects owner or other bits that differ from the expected mode. Extra group
 * bits, which also reflect a POSIX ACL mask, only produce a warning. */
function checkKeyMode(
  target: string,
  info: fs.Stats,
  expected: number,
  warn: boolean,
): void {
  const mode = info.mode & 0o777;
  if (mode === expected) return;
  const kind = info.isDirectory() ? "directory" : "file";
  const octal = (value: number) => value.toString(8).padStart(4, "0");
  // Name the key relative to the database's directory so host paths stay out
  // of logs. Clear ACL entries first: chmod alone leaves a base group entry
  // that setfacl -b would make effective again.
  const name = info.isDirectory()
    ? path.basename(target)
    : path.join(path.basename(path.dirname(target)), path.basename(target));
  const fix = `in the database's directory, run setfacl -b ${shellQuote(name)} && chmod ${expected.toString(8)} ${shellQuote(name)}`;
  if ((mode & 0o707) !== expected) {
    throw new KeyPermissionError(
      `Ludock's identity key ${kind} ${name} has unsafe permissions: expected ${octal(expected)}, found ${octal(mode)}. To fix it, ${fix}`,
    );
  }
  if (warn) {
    logger.warn(
      `Ludock's identity key ${kind} ${name} grants extra access through group bits or a POSIX ACL: expected ${octal(expected)}, found ${octal(mode)}. Starting anyway; to fix it, ${fix}`,
    );
  }
}

function openKeyDirectory(directory: string, warn = true): number {
  const descriptor = fs.openSync(
    directory,
    fs.constants.O_RDONLY |
      fs.constants.O_DIRECTORY |
      fs.constants.O_NOFOLLOW,
  );
  try {
    const info = fs.fstatSync(descriptor);
    if (!info.isDirectory() || !ownedByProcess(info)) throw keyError();
    checkKeyMode(directory, info, KEY_DIRECTORY_MODE, warn);
  } catch (error) {
    fs.closeSync(descriptor);
    throw error;
  }
  return descriptor;
}

function syncParentDirectory(directory: string): void {
  const parent = fs.openSync(
    path.dirname(directory),
    fs.constants.O_RDONLY |
      fs.constants.O_DIRECTORY |
      fs.constants.O_NOFOLLOW,
  );
  try {
    fs.fsyncSync(parent);
  } finally {
    fs.closeSync(parent);
  }
}

function readPublishedKey(directory: string): Uint8Array {
  let directoryDescriptor: number | undefined;
  let keyDescriptor: number | undefined;
  let key: Uint8Array | undefined;
  try {
    directoryDescriptor = openKeyDirectory(directory);
    const keyPath = path.join(directory, KEY_FILENAME);
    keyDescriptor = fs.openSync(
      keyPath,
      fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW,
    );
    const info = fs.fstatSync(keyDescriptor);
    if (
      !info.isFile() ||
      info.nlink !== 1 ||
      info.size !== KEY_BYTES ||
      !ownedByProcess(info)
    ) {
      throw keyError();
    }
    checkKeyMode(keyPath, info, KEY_FILE_MODE, true);
    key = fs.readFileSync(keyDescriptor);
    if (key.length !== KEY_BYTES) throw keyError();

    // A racing creator may have published the complete directory but not yet
    // synced its new parent entry. Make the winner durable before SQLite can
    // commit a database that depends on it.
    fs.fsyncSync(directoryDescriptor);
    syncParentDirectory(directory);
    return key;
  } catch (error) {
    key?.fill(0);
    if (error instanceof KeyPermissionError) throw error;
    throw keyError();
  } finally {
    if (keyDescriptor !== undefined) fs.closeSync(keyDescriptor);
    if (directoryDescriptor !== undefined) fs.closeSync(directoryDescriptor);
  }
}

function discardStagingDirectory(directory: string): void {
  try {
    fs.unlinkSync(path.join(directory, KEY_FILENAME));
  } catch {
    // A failed write may not have created the staging key.
  }
  try {
    fs.rmdirSync(directory);
  } catch {
    // Crash-only staging directories are ignored on later startups.
  }
}

function publishKeyDirectory(directory: string, key: Uint8Array): void {
  const staging = `${directory}.stage-${process.pid}-${crypto.getRandomValues(new Uint8Array(12)).toHex()}`;
  fs.mkdirSync(staging, { mode: KEY_DIRECTORY_MODE });
  let published = false;
  try {
    const descriptor = fs.openSync(
      path.join(staging, KEY_FILENAME),
      fs.constants.O_WRONLY |
        fs.constants.O_CREAT |
        fs.constants.O_EXCL |
        fs.constants.O_NOFOLLOW,
      KEY_FILE_MODE,
    );
    try {
      fs.writeFileSync(descriptor, key);
      fs.fsyncSync(descriptor);
    } finally {
      fs.closeSync(descriptor);
    }
    // The published directory is checked again and warns under its real name.
    const stagingDescriptor = openKeyDirectory(staging, false);
    try {
      fs.fsyncSync(stagingDescriptor);
    } finally {
      fs.closeSync(stagingDescriptor);
    }
    try {
      // The source is already nonempty, and a legitimate destination is also
      // nonempty, so POSIX rename cannot replace another creator's winner.
      fs.renameSync(staging, directory);
      published = true;
    } catch {
      if (!fs.existsSync(directory)) throw keyError();
    }
    if (published) syncParentDirectory(directory);
  } finally {
    if (!published) discardStagingDirectory(staging);
  }
}

/** The key is separate from SQLite so a database-only disclosure cannot be
 * used to guess low-entropy secrets included in persisted fingerprints. */
export function loadFingerprintKey(dbPath: string, required: boolean): Uint8Array {
  if (dbPath === ":memory:") return crypto.getRandomValues(new Uint8Array(KEY_BYTES));
  // Database aliases share the same installation key.
  const keyDirectory = `${fs.realpathSync(dbPath)}.identity-key`;
  try {
    const info = fs.lstatSync(keyDirectory);
    if (!info.isDirectory()) throw keyError();
    return readPublishedKey(keyDirectory);
  } catch (error) {
    if (error instanceof KeyPermissionError) throw error;
    if ((error as NodeJS.ErrnoException).code !== "ENOENT" || required)
      throw keyError();
  }

  const generated = crypto.getRandomValues(new Uint8Array(KEY_BYTES));
  try {
    publishKeyDirectory(keyDirectory, generated);
  } catch (error) {
    if (error instanceof KeyPermissionError) throw error;
    throw keyError();
  } finally {
    generated.fill(0);
  }
  // Reopen and validate the published key, including after a creator race.
  return loadFingerprintKey(dbPath, true);
}

function protectDigest(
  digest: string,
  key: Uint8Array,
  domain: "binding:v2" | "compose-source:v1",
): string {
  if (!/^[a-f0-9]{64}$/.test(digest))
    throw new Error("Invalid fingerprint digest");
  return `hmac-sha256:${new Bun.CryptoHasher("sha256", key)
    .update(`ludock:${domain}:${digest}`)
    .digest("hex")}`;
}

export function protectBindingFingerprint(
  digest: string,
  key: Uint8Array,
): string {
  return protectDigest(digest, key, "binding:v2");
}

export function protectComposeSourceFingerprint(
  digest: string,
  key: Uint8Array,
): string {
  return protectDigest(digest, key, "compose-source:v1");
}

function keyCheck(key: Uint8Array): string {
  return new Bun.CryptoHasher("sha256", key)
    .update("ludock:identity-key:v2")
    .digest("hex");
}

export function initializeFingerprintKey(
  db: Database,
  key?: Uint8Array,
): void {
  if (!key) throw new Error("Identity key is required to initialize the database");
  db.prepare("INSERT INTO settings(key,value_json) VALUES (?,?)")
    .run(KEY_CHECK_SETTING, JSON.stringify(keyCheck(key)));
}

export function assertFingerprintKey(db: Database, key: Uint8Array): void {
  const row = db.prepare("SELECT value_json FROM settings WHERE key=?")
    .get(KEY_CHECK_SETTING) as { value_json: string } | null;
  const expected = encodeText(JSON.stringify(keyCheck(key)));
  const actual = encodeText(row?.value_json ?? "");
  if (
    actual.length !== expected.length ||
    !crypto.timingSafeEqual(actual, expected)
  ) {
    throw keyError();
  }
}
