import path from "node:path";
import { AppError } from "./errors.js";

const active = new Set<string>();
const drained = new Set<() => void>();
const normalizeKey = (key: string) =>
  key.startsWith("path:") ? `path:${path.posix.resolve(key.slice(5))}` : key;
const overlaps = (left: string, right: string) =>
  left === right ||
  (left.startsWith("path:") &&
    right.startsWith("path:") &&
    (left === "path:/" ||
      right === "path:/" ||
      left.startsWith(`${right}/`) ||
      right.startsWith(`${left}/`)));

export function acquireLocks(keys: readonly string[]): () => void {
  const ordered = [...new Set(keys.map(normalizeKey))].sort();
  if (ordered.some((key) => [...active].some((held) => overlaps(key, held)))) {
    throw new AppError(
      "OPERATION_CONFLICT",
      409,
      "A conflicting operation is already running",
    );
  }
  ordered.forEach((key) => active.add(key));
  let released = false;
  return () => {
    if (!released) {
      released = true;
      ordered.forEach((key) => active.delete(key));
      if (active.size === 0) {
        for (const resolve of drained) resolve();
        drained.clear();
      }
    }
  };
}
export async function withLocks<T>(
  keys: readonly string[],
  action: () => Promise<T> | T,
): Promise<T> {
  const release = acquireLocks(keys);
  try {
    return await action();
  } finally {
    release();
  }
}
export function isServerBusy(id: string): boolean {
  return active.has(`server:${id}`);
}

/** Call after stopping new requests and jobs so remaining stream cleanup and
 * console commands can finish before the database is closed. */
export function waitForLocksReleased(): Promise<void> {
  if (active.size === 0) return Promise.resolve();
  return new Promise((resolve) => drained.add(resolve));
}
