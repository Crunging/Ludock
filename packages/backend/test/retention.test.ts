import { expect, afterEach, describe, it } from "bun:test";
import type { SQLQueryBindings } from "bun:sqlite";

process.env.LUDOCK_DB_PATH = ":memory:";
process.env.AUDIT_LOG_MAX_ROWS = "1000";

const {
  closeDatabase,
  getDatabase,
  getLoginThrottle,
  pruneAuditLog,
  pruneAuditLogIfNeeded,
  pruneLoginAttempts,
  recordLoginFailure,
  writeAuditLog,
} = await import("../src/database.js");
const { listAuditHistory } = await import("../src/history.js");

afterEach(() => closeDatabase());

function auditCount(): number {
  return (
    getDatabase().prepare<Record<string, unknown>, SQLQueryBindings[]>("SELECT COUNT(*) AS count FROM audit_log").get() as {
      count: number;
    }
  ).count;
}

function attemptCount(): number {
  return (
    getDatabase()
      .prepare<Record<string, unknown>, SQLQueryBindings[]>("SELECT COUNT(*) AS count FROM login_attempts")
      .get() as { count: number }
  ).count;
}

describe("audit log retention", () => {
  it("keeps a rolling window of the newest entries", () => {
    expect(() => pruneAuditLog(), "empty table must be safe").not.toThrow();
    expect(auditCount()).toBe(0);

    for (let index = 0; index < 1200; index += 1) {
      writeAuditLog({ action: `auth.login.failed.${index}` });
    }

    // Periodic pruning can exceed the limit by at most one prune interval.
    expect(auditCount() <= 1000 + 500, `expected the table to stay bounded, found ${auditCount()}`).toBeTruthy();

    pruneAuditLog();
    expect(auditCount()).toBe(1000);

    expect(listAuditHistory({ limit: 1 }).entries[0]?.action).toBe("auth.login.failed.1199");
    expect(listAuditHistory({ limit: 1, action: "auth.login.failed.0" }).entries).toStrictEqual([]);
  });

  it("defers pruning until the owning transaction has committed", () => {
    const db = getDatabase();
    const seed = db.prepare("INSERT INTO audit_log(action,created_at) VALUES(?,?)");
    for (let index = 0; index < 1001; index += 1) seed.run("fixture", index);

    db.exec("BEGIN IMMEDIATE");
    try {
      for (let index = 0; index < 500; index += 1) {
        writeAuditLog({ action: "fixture.transaction" }, { prune: false });
      }
      expect(auditCount(), "retention must not run inside the transaction").toBe(1501);
      db.exec("COMMIT");
    } catch (error) {
      db.exec("ROLLBACK");
      throw error;
    }

    pruneAuditLogIfNeeded();
    expect(auditCount()).toBe(1000);
    expect(listAuditHistory({ limit: 1 }).entries[0]?.action).toBe("fixture.transaction");
  });

  it("keeps failed deferred pruning eligible for retry", () => {
    const db = getDatabase();
    const seed = db.prepare("INSERT INTO audit_log(action,created_at) VALUES(?,?)");
    for (let index = 0; index < 1001; index += 1) seed.run("fixture", index);
    for (let index = 0; index < 500; index += 1) {
      writeAuditLog({ action: "fixture.committed" }, { prune: false });
    }
    db.exec(`CREATE TEMP TRIGGER fail_audit_pruning BEFORE DELETE ON audit_log
      BEGIN SELECT RAISE(FAIL, 'fixture pruning failure'); END`);
    expect(() => pruneAuditLogIfNeeded()).toThrow(/fixture pruning failure/);
    expect(auditCount()).toBe(1501);
    db.exec("DROP TRIGGER fail_audit_pruning");

    pruneAuditLogIfNeeded();
    expect(auditCount(), "a failed maintenance pass must remain due").toBe(1000);
  });
});

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
