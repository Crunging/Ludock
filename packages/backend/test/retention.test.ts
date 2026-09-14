import assert from "node:assert/strict";
import { afterEach, describe, it } from "bun:test";

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
    getDatabase().prepare("SELECT COUNT(*) AS count FROM audit_log").get() as {
      count: number;
    }
  ).count;
}

function attemptCount(): number {
  return (
    getDatabase()
      .prepare("SELECT COUNT(*) AS count FROM login_attempts")
      .get() as { count: number }
  ).count;
}

describe("audit log retention", () => {
  it("keeps a rolling window of the newest entries", () => {
    assert.doesNotThrow(() => pruneAuditLog(), "empty table must be safe");
    assert.equal(auditCount(), 0);

    for (let index = 0; index < 1200; index += 1) {
      writeAuditLog({ action: `auth.login.failed.${index}` });
    }

    // Periodic pruning can exceed the limit by at most one prune interval.
    assert.ok(
      auditCount() <= 1000 + 500,
      `expected the table to stay bounded, found ${auditCount()}`
    );

    pruneAuditLog();
    assert.equal(auditCount(), 1000);

    assert.equal(listAuditHistory({ limit: 1 }).entries[0]?.action, "auth.login.failed.1199");
    assert.deepEqual(listAuditHistory({ limit: 1, action: "auth.login.failed.0" }).entries, []);
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
      assert.equal(auditCount(), 1501, "retention must not run inside the transaction");
      db.exec("COMMIT");
    } catch (error) {
      db.exec("ROLLBACK");
      throw error;
    }

    pruneAuditLogIfNeeded();
    assert.equal(auditCount(), 1000);
    assert.equal(listAuditHistory({ limit: 1 }).entries[0]?.action, "fixture.transaction");
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
    assert.throws(() => pruneAuditLogIfNeeded(), /fixture pruning failure/);
    assert.equal(auditCount(), 1501);
    db.exec("DROP TRIGGER fail_audit_pruning");

    pruneAuditLogIfNeeded();
    assert.equal(auditCount(), 1000, "a failed maintenance pass must remain due");
  });
});

describe("login throttle retention", () => {
  it("keeps a full failure-counting window when cooldowns are shorter", () => {
    const windowMs = 15 * 60 * 1000;
    const now = 1_000_000_000;
    recordLoginFailure("progressive", now, windowMs, 1, 250);
    const afterCooldown = getLoginThrottle("progressive", now + 251, windowMs);
    assert.equal(afterCooldown.failures, 1);
    assert.equal(afterCooldown.blockedUntil, now + 250);
    assert.equal(
      recordLoginFailure("progressive", now + 251, windowMs, 1, 500).blockedUntil,
      now + 751,
    );
    assert.deepEqual(getLoginThrottle("progressive", now + windowMs, windowMs), {
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
    assert.ok(getLoginThrottle(blocking, now, windowMs).blockedUntil > now);

    recordLoginFailure("stale-key", now - windowMs * 4, windowMs, 5);
    const before = attemptCount();
    assert.ok(before >= 2);

    pruneLoginAttempts(now, windowMs);

    assert.ok(
      attemptCount() < before,
      "the stale row should have been removed"
    );
    assert.ok(
      getLoginThrottle(blocking, now, windowMs).blockedUntil > now,
      "an actively blocking row must survive pruning"
    );
  });
});
