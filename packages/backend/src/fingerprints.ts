import fs from "node:fs";
import path from "node:path";
import { createHmac, randomBytes, timingSafeEqual } from "node:crypto";
import type { Database } from "bun:sqlite";

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

function ownedByProcess(info: fs.Stats): boolean {
  return !process.getuid || info.uid === process.getuid();
}

function openKeyDirectory(directory: string): number {
  const descriptor = fs.openSync(
    directory,
    fs.constants.O_RDONLY |
      fs.constants.O_DIRECTORY |
      fs.constants.O_NOFOLLOW,
  );
  const info = fs.fstatSync(descriptor);
  if (
    !info.isDirectory() ||
    (info.mode & 0o777) !== KEY_DIRECTORY_MODE ||
    !ownedByProcess(info)
  ) {
    fs.closeSync(descriptor);
    throw keyError();
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

function readPublishedKey(directory: string): Buffer {
  let directoryDescriptor: number | undefined;
  let keyDescriptor: number | undefined;
  let key: Buffer | undefined;
  try {
    directoryDescriptor = openKeyDirectory(directory);
    keyDescriptor = fs.openSync(
      path.join(directory, KEY_FILENAME),
      fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW,
    );
    const info = fs.fstatSync(keyDescriptor);
    if (
      !info.isFile() ||
      info.nlink !== 1 ||
      info.size !== KEY_BYTES ||
      (info.mode & 0o777) !== KEY_FILE_MODE ||
      !ownedByProcess(info)
    ) {
      throw keyError();
    }
    key = fs.readFileSync(keyDescriptor);
    if (key.length !== KEY_BYTES) throw keyError();

    // A racing creator may have published the complete directory but not yet
    // synced its new parent entry. Make the winner durable before SQLite can
    // commit a database that depends on it.
    fs.fsyncSync(directoryDescriptor);
    syncParentDirectory(directory);
    return key;
  } catch {
    key?.fill(0);
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
  const staging = `${directory}.stage-${process.pid}-${randomBytes(12).toString("hex")}`;
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
    const stagingDescriptor = openKeyDirectory(staging);
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
export function loadFingerprintKey(dbPath: string, required: boolean): Buffer {
  if (dbPath === ":memory:") return randomBytes(KEY_BYTES);
  // Database aliases share the same installation key.
  const keyDirectory = `${fs.realpathSync(dbPath)}.identity-key`;
  try {
    const info = fs.lstatSync(keyDirectory);
    if (!info.isDirectory()) throw keyError();
    return readPublishedKey(keyDirectory);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT" || required)
      throw keyError();
  }

  const generated = randomBytes(KEY_BYTES);
  try {
    publishKeyDirectory(keyDirectory, generated);
  } catch {
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
  return `hmac-sha256:${createHmac("sha256", key)
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
  return createHmac("sha256", key)
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
  const expected = Buffer.from(JSON.stringify(keyCheck(key)));
  const actual = Buffer.from(row?.value_json ?? "");
  if (
    actual.length !== expected.length ||
    !timingSafeEqual(actual, expected)
  ) {
    throw keyError();
  }
}
