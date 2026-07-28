import assert from "node:assert/strict";
import { describe, it } from "node:test";

process.env.PANEL_DB_PATH = ":memory:";
process.env.AUDIT_LOG_MAX_ROWS = "1000";

const {
  getDatabase,
  getLoginThrottle,
  listAuditLog,
  pruneAuditLog,
  pruneLoginAttempts,
  recordLoginFailure,
  writeAuditLog,
} = await import("../src/database.js");

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

    // Pruning runs every 500 writes, so growth is bounded by the retention
    // window plus one prune interval rather than capped exactly.
    assert.ok(
      auditCount() <= 1000 + 500,
      `expected the table to stay bounded, found ${auditCount()}`
    );

    // An explicit prune trims to the configured window exactly.
    pruneAuditLog();
    assert.equal(auditCount(), 1000);

    // The most recent entry must survive and the oldest must be gone.
    assert.equal(listAuditLog(1)[0]?.action, "auth.login.failed.1199");
    const actions = new Set(listAuditLog(1000).map((entry) => entry.action));
    assert.equal(actions.has("auth.login.failed.1199"), true);
    assert.equal(actions.has("auth.login.failed.0"), false);
  });
});

describe("login throttle retention", () => {
  it("drops stale rows but keeps rows that still block", () => {
    const windowMs = 15 * 60 * 1000;
    const now = 1_000_000_000;

    // A key that is actively blocking.
    const blocking = "blocking-key";
    for (let attempt = 0; attempt < 5; attempt += 1) {
      recordLoginFailure(blocking, now, windowMs, 5);
    }
    assert.ok(getLoginThrottle(blocking, now, windowMs).blockedUntil > now);

    // A key whose window closed long ago.
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
