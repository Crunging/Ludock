import { expect, afterEach, describe, it } from "bun:test";
import type { SQLQueryBindings } from "bun:sqlite";

process.env.LUDOCK_DB_PATH = ":memory:";

const {
  closeDatabase,
  getDatabase,
  getLoginThrottle,
  pruneLoginAttempts,
  recordLoginFailure,
} = await import("../src/database.js");

afterEach(() => closeDatabase());

function attemptCount(): number {
  return (
    getDatabase()
      .prepare<Record<string, unknown>, SQLQueryBindings[]>("SELECT COUNT(*) AS count FROM login_attempts")
      .get() as { count: number }
  ).count;
}

describe("login throttle retention", () => {
  it("keeps a full failure-counting window when cooldowns are shorter", () => {
    const windowMs = 15 * 60 * 1000;
    const now = 1_000_000_000;
    recordLoginFailure("progressive", now, windowMs, 1, 250);
    const afterCooldown = getLoginThrottle("progressive", now + 251, windowMs);
    expect(afterCooldown.failures).toBe(1);
    expect(afterCooldown.blockedUntil).toBe(now + 250);
    expect(recordLoginFailure("progressive", now + 251, windowMs, 1, 500).blockedUntil).toBe(now + 751);
    expect(getLoginThrottle("progressive", now + windowMs, windowMs)).toStrictEqual({
      failures: 0,
      blockedUntil: 0,
    });
  });

  it("drops stale rows but keeps rows that still block", () => {
    const windowMs = 15 * 60 * 1000;
    const now = 1_000_000_000;

    const blocking = "blocking-key";
    for (let attempt = 0; attempt < 5; attempt += 1) {
      recordLoginFailure(blocking, now, windowMs, 5);
    }
    expect(getLoginThrottle(blocking, now, windowMs).blockedUntil > now).toBeTruthy();

    recordLoginFailure("stale-key", now - windowMs * 4, windowMs, 5);
    const before = attemptCount();
    expect(before >= 2).toBeTruthy();

    pruneLoginAttempts(now, windowMs);

    expect(attemptCount() < before, "the stale row should have been removed").toBeTruthy();
    expect(getLoginThrottle(blocking, now, windowMs).blockedUntil > now, "an actively blocking row must survive pruning").toBeTruthy();
  });
});
