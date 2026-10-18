import path from "node:path";
import { AppError } from "./errors.js";

/** Held keys. Jobs wait for background holders instead of failing. */
const active = new Map<string, { background: boolean }>();
const drained = new Set<() => void>();
let releasedWaiters: { promise: Promise<void>; resolve: () => void } | undefined;
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

/** A conflict only with background work, such as a countdown warning. */
export class BackgroundLockConflict extends AppError {
  constructor() {
    super("OPERATION_CONFLICT", 409, "A conflicting operation is already running");
  }
}

export function acquireLocks(keys: readonly string[], options?: { background?: boolean }): () => void {
  const ordered = [...new Set(keys.map(normalizeKey))].sort();
  const conflicts = [...active].filter(([held]) => ordered.some((key) => overlaps(key, held)));
  if (conflicts.length) {
    if (conflicts.every(([, holder]) => holder.background)) throw new BackgroundLockConflict();
    throw new AppError(
      "OPERATION_CONFLICT",
      409,
      "A conflicting operation is already running",
    );
  }
  ordered.forEach((key) => active.set(key, { background: options?.background === true }));
  let released = false;
  return () => {
    if (!released) {
      released = true;
      ordered.forEach((key) => active.delete(key));
      releasedWaiters?.resolve();
      releasedWaiters = undefined;
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
  options?: { background?: boolean },
): Promise<T> {
  const release = acquireLocks(keys, options);
  try {
    return await action();
  } finally {
    release();
  }
}
/** Resolves at the next release of any held keys. */
export function nextLockRelease(): Promise<void> {
  releasedWaiters ??= Promise.withResolvers<void>();
  return releasedWaiters.promise;
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
