import path from "node:path";
import { AppError } from "./errors.js";

/** Held keys. Jobs cancel background holders and wait instead of failing. */
const active = new Map<string, { background?: AbortController }>();
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
  constructor(private readonly holders: readonly AbortController[]) {
    super("OPERATION_CONFLICT", 409, "A conflicting operation is already running");
  }

  /** The holders keep their keys until their cleanup finishes. */
  cancel(): void {
    for (const holder of this.holders) holder.abort();
  }
}

export function acquireLocks(keys: readonly string[], options?: { background?: AbortController }): () => void {
  const ordered = [...new Set(keys.map(normalizeKey))].sort();
  const conflicts = [...active]
    .filter(([held]) => ordered.some((key) => overlaps(key, held)))
    .map(([, holder]) => holder.background);
  if (conflicts.length) {
    if (conflicts.every((holder) => holder !== undefined)) throw new BackgroundLockConflict(conflicts);
    throw new AppError(
      "OPERATION_CONFLICT",
      409,
      "A conflicting operation is already running",
    );
  }
  ordered.forEach((key) => active.set(key, { background: options?.background }));
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
  options?: { background?: AbortController },
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
