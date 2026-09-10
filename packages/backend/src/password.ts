import { password as bunPassword } from "bun";
import { scrypt, timingSafeEqual } from "node:crypto";

const ARGON_MEMORY_KIB = 65536;
const ARGON_TIME_COST = 2;
const SCRYPT_MAX_MEMORY = 64 * 1024 * 1024;
const MAX_ACTIVE_PASSWORD_WORK = 4;
const MAX_ACTIVE_PASSWORD_WORK_PER_SOURCE = 2;
const MAX_ACTIVE_PASSWORD_WORK_PER_SUBJECT = 1;

let activePasswordWork = 0;
const activePasswordWorkBySource = new Map<string, number>();
const activePasswordWorkBySubject = new Map<string, number>();

export class PasswordWorkBusyError extends Error {
  constructor() {
    super("Password verification is busy");
    this.name = "PasswordWorkBusyError";
  }
}

function releasePasswordWork(counter: Map<string, number>, key: string): void {
  const remaining = (counter.get(key) ?? 1) - 1;
  if (remaining > 0) counter.set(key, remaining);
  else counter.delete(key);
}

/**
 * Atomically reserve bounded password work before starting a memory-hard KDF.
 * Requests are rejected instead of queued so an attacker cannot build an
 * unbounded backlog. Callers provide opaque, non-secret source and credential
 * keys; the limits are deliberately independent of persistent login throttles.
 */
export async function withPasswordWork<T>(
  sourceKey: string,
  subjectKey: string,
  work: () => Promise<T>,
): Promise<T> {
  const sourceCount = activePasswordWorkBySource.get(sourceKey) ?? 0;
  const subjectCount = activePasswordWorkBySubject.get(subjectKey) ?? 0;
  if (
    activePasswordWork >= MAX_ACTIVE_PASSWORD_WORK ||
    sourceCount >= MAX_ACTIVE_PASSWORD_WORK_PER_SOURCE ||
    subjectCount >= MAX_ACTIVE_PASSWORD_WORK_PER_SUBJECT
  ) {
    throw new PasswordWorkBusyError();
  }

  activePasswordWork += 1;
  activePasswordWorkBySource.set(sourceKey, sourceCount + 1);
  activePasswordWorkBySubject.set(subjectKey, subjectCount + 1);
  try {
    return await work();
  } finally {
    activePasswordWork -= 1;
    releasePasswordWork(activePasswordWorkBySource, sourceKey);
    releasePasswordWork(activePasswordWorkBySubject, subjectKey);
  }
}

export function hashPassword(password: string): Promise<string> {
  return bunPassword.hash(password, {
    algorithm: "argon2id",
    memoryCost: ARGON_MEMORY_KIB,
    timeCost: ARGON_TIME_COST,
  });
}

function argonParameters(encoded: string): { memory: number; time: number; parallelism: number } | null {
  if (encoded.length > 256) return null;
  const match = /^\$argon2id\$v=19\$m=([1-9]\d*),t=([1-9]\d*),p=([1-9]\d*)\$([A-Za-z0-9+/]+)\$([A-Za-z0-9+/]+)$/.exec(encoded);
  if (!match) return null;
  const [, memory, time, parallelism, salt, key] = match;
  const parameters = { memory: Number(memory), time: Number(time), parallelism: Number(parallelism) };
  // Stored parameters must not turn a login into unbounded password work.
  if (parameters.memory < 8 * parameters.parallelism || parameters.memory > 262144 ||
    parameters.time > 10 || parameters.parallelism > 16 ||
    Buffer.from(salt, "base64").length < 16 || Buffer.from(salt, "base64").length > 64 ||
    Buffer.from(key, "base64").length !== 32 ||
    Buffer.from(salt, "base64").toString("base64").replace(/=+$/, "") !== salt ||
    Buffer.from(key, "base64").toString("base64").replace(/=+$/, "") !== key)
    return null;
  return parameters;
}

export async function verifyPassword(password: string, encoded: string): Promise<boolean> {
  if (encoded.startsWith("$argon2id$")) {
    if (!argonParameters(encoded)) return false;
    try {
      return await bunPassword.verify(password, encoded, "argon2id");
    } catch {
      return false;
    }
  }

  // Existing accounts retain their validated scrypt encoding until a successful
  // login replaces it with Argon2id. Never reinterpret a malformed stored hash.
  const [algorithm, n, r, p, saltValue, keyValue, extra] = encoded.split("$");
  if (algorithm !== "scrypt" || !n || !r || !p || !saltValue || !keyValue || extra !== undefined)
    return false;
  const expected = Buffer.from(keyValue, "base64url");
  const salt = Buffer.from(saltValue, "base64url");
  if (expected.length !== 64 || salt.length !== 16 ||
    expected.toString("base64url") !== keyValue || salt.toString("base64url") !== saltValue)
    return false;
  const options = { N: Number(n), r: Number(r), p: Number(p) };
  if (!Number.isInteger(options.N) || !Number.isInteger(options.r) || !Number.isInteger(options.p) ||
    options.N < 2 || options.N > 131072 || (options.N & (options.N - 1)) !== 0 ||
    options.r < 1 || options.r > 16 || options.p < 1 || options.p > 10)
    return false;
  const actual = await new Promise<Buffer>((resolve, reject) => {
    scrypt(password, salt, expected.length, {
      ...options,
      maxmem: Math.max(SCRYPT_MAX_MEMORY, 128 * options.N * options.r + 16 * 1024 * 1024),
    }, (error, derived) => error ? reject(error) : resolve(derived));
  });
  return actual.length === expected.length && timingSafeEqual(actual, expected);
}

export function passwordHashNeedsUpgrade(encoded: string): boolean {
  const parameters = argonParameters(encoded);
  return !parameters || parameters.memory < ARGON_MEMORY_KIB || parameters.time < ARGON_TIME_COST;
}
