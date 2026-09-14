import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";
import { listOperationHistory } from "../src/history.js";
import type { ScheduleInput } from "@ludock/shared";
import * as database from "../src/database.js";
import * as identity from "../src/identity.js";
import {
  closeDatabase,
  createUser,
  deleteUser,
  getDatabase,
  updateUserAccess,
  type SessionUser,
} from "../src/database.js";
import {
  MAX_SCHEDULE_PAYLOAD_BYTES,
  MAX_SCHEDULES_PER_SERVER,
  MAX_SCHEDULES_TOTAL,
  createSchedule,
  deleteSchedule,
  listSchedules,
  runSchedules,
  scheduleSlot,
  setScheduleEnabled,
  updateSchedule,
} from "../src/schedules.js";
import { AppError } from "../src/errors.js";
import { setServerGrant } from "../src/authorization.js";
import { reconcileServers, reviewServerBinding } from "../src/identity.js";
import {
  getOperation,
  registerJobHandler,
  startOperationRunner,
  stopOperationRunner,
} from "../src/operations.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const admin: SessionUser = { id: "admin", username: "owner", role: "admin" };
const friend: SessionUser = {
  id: "friend",
  username: "friend",
  role: "operator",
};
const other: SessionUser = { id: "other", username: "other", role: "operator" };
const observation = {
  containerId: "original",
  name: "world",
  displayName: "World",
  gameType: "minecraft",
  mounts: [],
};
const input: ScheduleInput = {
  action: "start",
  enabled: true,
  time: "08:00",
  days: [0, 1, 2, 3, 4, 5, 6],
  timezone: "America/Los_Angeles",
};
const due = Date.parse("2026-09-09T15:00:00Z");
let serverId: string;
const serverOperations = () => listOperationHistory(admin, { serverId, limit: 50 }).operations;

function insertScheduleRows(
  count: number,
  options: {
    serverId?: string;
    ownerId?: string;
    data?: ScheduleInput;
    inputJson?: string;
  } = {},
): string[] {
  const statement = getDatabase().prepare(
    "INSERT INTO schedules(id,server_id,owner_id,input_json,binding_revision,created_at) VALUES(?,?,?,?,?,?)",
  );
  const ids: string[] = [];
  for (let index = 0; index < count; index += 1) {
    const id = crypto.randomUUID();
    ids.push(id);
    statement.run(
      id,
      options.serverId ?? serverId,
      options.ownerId ?? friend.id,
      options.inputJson ?? JSON.stringify(options.data ?? input),
      1,
      index,
    );
  }
  return ids;
}

beforeEach(async () => {
  await stopOperationRunner();
  closeDatabase();
  for (const actor of [admin, friend, other])
    createUser({
      ...actor,
      passwordHash: "fake",
      disabled: false,
      createdAt: 0,
    });
  serverId = reconcileServers([observation])[0].id;
  for (const actor of [friend, other])
    setServerGrant(
      actor.id,
      serverId,
      ["server.view", "server.start", "schedules.manage"],
      admin,
    );
});
afterEach(async () => {
  mock.restore();
  await stopOperationRunner();
  closeDatabase();
});

describe("schedule clock semantics", () => {
  it("uses local days/time and never catches up missed destructive work", () => {
    assert.ok(scheduleSlot(input, due));
    assert.equal(scheduleSlot(input, due + 60_000), null);
    assert.equal(scheduleSlot({ ...input, days: [0] }, due), null);
  });
  it("deduplicates the repeated fall-back hour and skips the spring-forward gap", () => {
    const fall = { ...input, time: "01:30" };
    assert.equal(
      scheduleSlot(fall, Date.parse("2026-11-01T08:30:00Z")),
      scheduleSlot(fall, Date.parse("2026-11-01T09:30:00Z")),
    );
    const spring = { ...input, time: "02:30" };
    assert.equal(
      scheduleSlot(spring, Date.parse("2026-03-08T09:30:00Z")),
      null,
    );
    assert.equal(
      scheduleSlot(spring, Date.parse("2026-03-08T10:30:00Z")),
      null,
    );
  });
});

describe("schedule creation audit transaction", () => {
  it("commits creation and its audit before attempting retention cleanup", () => {
    const db = getDatabase();
    let cleanupInTransaction: boolean | undefined;
    const warn = spyOn(console, "warn").mockImplementation(() => {});
    const cleanup = spyOn(database, "pruneAuditLogIfNeeded").mockImplementation(() => {
      cleanupInTransaction = db.inTransaction;
      throw new Error("fixture-retention-private-detail");
    });

    const schedule = createSchedule(friend, serverId, input);

    assert.equal(cleanup.mock.calls.length, 1);
    assert.equal(cleanupInTransaction, false);
    assert.equal(listSchedules(friend, serverId)[0].id, schedule.id);
    const audit = db.prepare(
      "SELECT details_json FROM audit_log WHERE action='schedule.created'",
    ).get() as { details_json: string };
    assert.equal(JSON.parse(audit.details_json).scheduleId, schedule.id);
    assert.equal(warn.mock.calls.length, 1);
    assert.match(String(warn.mock.calls[0][0]), /retention cleanup failed/);
    assert.doesNotMatch(String(warn.mock.calls[0][0]), /fixture-retention-private-detail/);
  });

  it("rolls back creation when its audit cannot be written", () => {
    const db = getDatabase();
    db.exec(`CREATE TRIGGER fail_schedule_audit BEFORE INSERT ON audit_log
      WHEN NEW.action='schedule.created'
      BEGIN SELECT RAISE(ABORT, 'fixture schedule audit failure'); END`);

    assert.throws(
      () => createSchedule(friend, serverId, input),
      /fixture schedule audit failure/,
    );
    assert.equal(listSchedules(friend, serverId).length, 0);
    assert.equal(db.inTransaction, false);
  });

  it("preserves the original failure when SQLite has already rolled back", () => {
    const db = getDatabase();
    db.exec(`CREATE TRIGGER rollback_schedule_audit BEFORE INSERT ON audit_log
      WHEN NEW.action='schedule.created'
      BEGIN SELECT RAISE(ROLLBACK, 'fixture original audit failure'); END`);

    assert.throws(
      () => createSchedule(friend, serverId, input),
      /fixture original audit failure/,
    );
    assert.equal(listSchedules(friend, serverId).length, 0);
    assert.equal(db.inTransaction, false);
  });
});

describe("schedule authority", () => {
  it("requires an independently granted action, in addition to schedule management", () => {
    assert.throws(
      () => createSchedule(friend, serverId, { ...input, action: "backup" }),
      /permission/i,
    );
    assert.throws(
      () => createSchedule(friend, serverId, { ...input, action: "restart" }),
      /permission/i,
    );
    assert.equal(listSchedules(friend, serverId).length, 0);
  });
  it("hides other owners' schedules and prevents changing their authority", () => {
    const schedule = createSchedule(admin, serverId, input);
    assert.equal(listSchedules(friend, serverId).length, 0);
    assert.throws(
      () => deleteSchedule(friend, serverId, schedule.id),
      /not found/,
    );
    assert.equal(listSchedules(admin, serverId).length, 1);
  });
  it("uses current account roles when enforcing schedule ownership", () => {
    const schedule = createSchedule(admin, serverId, input);
    const staleAdministrator = { ...friend, role: "admin" as const };
    assert.equal(listSchedules(staleAdministrator, serverId).length, 0);
    assert.throws(
      () => deleteSchedule(staleAdministrator, serverId, schedule.id),
      /not found/,
    );
  });
  it("does not run twice in a slot and does not catch up after a missed slot", () => {
    createSchedule(friend, serverId, input);
    runSchedules(due);
    runSchedules(due + 30_000);
    assert.equal(serverOperations().length, 1);
    getDatabase().prepare("UPDATE operations SET status='succeeded'").run();
    runSchedules(due + 86400_000 + 60_000);
    assert.equal(serverOperations().length, 1);
  });
  it("blocks revoked actions and records an actionable suspension", () => {
    createSchedule(friend, serverId, input);
    assert.ok(listSchedules(friend, serverId)[0].nextRunAt);
    setServerGrant(
      friend.id,
      serverId,
      ["server.view", "schedules.manage"],
      admin,
    );
    runSchedules(due);
    assert.equal(serverOperations().length, 0);
    assert.match(
      listSchedules(admin, serverId)[0].lastResult!,
      /Suspended: required access/,
    );
  });
  it("blocks disabled owners and removes schedules with deleted owners", () => {
    createSchedule(friend, serverId, input);
    assert.ok(listSchedules(friend, serverId)[0].nextRunAt);
    updateUserAccess(friend.id, "operator", true);
    runSchedules(due);
    assert.equal(serverOperations().length, 0);
    assert.match(listSchedules(admin, serverId)[0].lastResult!, /Suspended/);
    deleteUser(friend.id);
    assert.equal(listSchedules(admin, serverId).length, 0);
  });
  it("follows validated ordinary recreation using its new binding revision", () => {
    createSchedule(friend, serverId, input);
    const replacement = reconcileServers([
      { ...observation, containerId: "replacement" },
    ])[0];
    runSchedules(due);
    const operations = serverOperations();
    assert.equal(operations.length, 1);
    assert.equal(
      getOperation(operations[0].id)?.bindingRevision,
      replacement.bindingRevision,
    );
  });
  it("keeps schedules suspended after material data changes until explicitly recreated", () => {
    createSchedule(friend, serverId, input);
    assert.ok(listSchedules(friend, serverId)[0].nextRunAt);
    const pending = reconcileServers([
      { ...observation, gameType: "factorio" },
    ])[0];
    reviewServerBinding(serverId, pending.pendingFingerprint!);
    runSchedules(due);
    assert.equal(serverOperations().length, 0);
    assert.match(
      listSchedules(admin, serverId)[0].lastResult!,
      /server configuration changed/,
    );
  });
});

describe("schedule editing and suspension", () => {
  it("edits in place while preserving owner, binding baseline, history, and consumed slots", () => {
    const created = createSchedule(friend, serverId, input);
    runSchedules(due);
    const before = getDatabase().prepare("SELECT * FROM schedules WHERE id=?")
      .get(created.id) as Record<string, unknown>;
    const edited = updateSchedule(admin, serverId, created.id, {
      ...input, time: "09:00", revision: created.revision,
    });
    assert.equal(edited.id, created.id);
    assert.equal(edited.ownerId, friend.id);
    assert.equal(edited.revision, 2);
    assert.equal(edited.time, "09:00");
    const after = getDatabase().prepare("SELECT * FROM schedules WHERE id=?")
      .get(created.id) as Record<string, unknown>;
    for (const key of ["owner_id", "binding_revision", "last_slot", "last_result", "created_at"])
      assert.equal(after[key], before[key]);
    updateSchedule(friend, serverId, created.id, { ...input, revision: 2 });
    getDatabase().prepare("UPDATE operations SET status='succeeded'").run();
    runSchedules(due + 30_000);
    assert.equal(serverOperations().length, 1);
  });

  it("rejects stale edits and toggles without losing newer input", () => {
    const schedule = createSchedule(friend, serverId, input);
    setScheduleEnabled(friend, serverId, schedule.id, { enabled: false, revision: 1 });
    for (const mutate of [
      () => updateSchedule(friend, serverId, schedule.id, { ...input, time: "09:00", revision: 1 }),
      () => setScheduleEnabled(friend, serverId, schedule.id, { enabled: true, revision: 1 }),
    ]) assert.throws(mutate, (error: unknown) =>
      error instanceof AppError && error.code === "SCHEDULE_CHANGED" && error.statusCode === 409);
    const current = listSchedules(friend, serverId)[0];
    assert.equal(current.revision, 2);
    assert.equal(current.enabled, false);
    assert.equal(current.time, input.time);
  });

  it("does not change revision or audit repeated identical updates", () => {
    const schedule = createSchedule(friend, serverId, input);
    const edited = updateSchedule(friend, serverId, schedule.id, {
      ...input, days: [...input.days].reverse(), revision: 1,
    });
    assert.equal(edited.revision, 1);
    assert.equal(setScheduleEnabled(friend, serverId, schedule.id, {
      enabled: true, revision: 1,
    }).revision, 1);
    assert.equal(getDatabase().prepare(
      "SELECT COUNT(*) AS count FROM audit_log WHERE action IN ('schedule.updated','schedule.resumed')",
    ).get()?.count, 0);
  });

  it("recognizes newly selected weekdays even when older input repeats a day", () => {
    const schedule = createSchedule(friend, serverId, { ...input, days: [1, 1] });
    const edited = updateSchedule(friend, serverId, schedule.id, {
      ...input, days: [1, 2], revision: 1,
    });
    assert.equal(edited.revision, 2);
    assert.deepEqual(edited.days, [1, 2]);
  });

  it("uses current ownership authority for editing and toggling", () => {
    const schedule = createSchedule(admin, serverId, input);
    const staleAdministrator = { ...friend, role: "admin" as const };
    assert.throws(() => updateSchedule(staleAdministrator, serverId, schedule.id, {
      ...input, time: "09:00", revision: 1,
    }), /not found/);
    assert.throws(() => setScheduleEnabled(staleAdministrator, serverId, schedule.id, {
      enabled: false, revision: 1,
    }), /not found/);
  });

  it("requires action grants for both requester and original owner", () => {
    const schedule = createSchedule(friend, serverId, input);
    assert.throws(() => updateSchedule(friend, serverId, schedule.id, {
      ...input, action: "backup", revision: 1,
    }), /permission/);
    assert.throws(() => updateSchedule(admin, serverId, schedule.id, {
      ...input, action: "backup", revision: 1,
    }), /required access/);
    assert.equal(listSchedules(admin, serverId)[0].revision, 1);
  });

  it("allows pausing after action revocation but prevents edits and resume under either actor", () => {
    const schedule = createSchedule(friend, serverId, input);
    setServerGrant(friend.id, serverId, ["server.view", "schedules.manage"], admin);
    assert.equal(listSchedules(admin, serverId)[0].nextRunAt, null);
    const paused = setScheduleEnabled(friend, serverId, schedule.id, { enabled: false, revision: 1 });
    assert.equal(paused.enabled, false);
    assert.equal(paused.nextRunAt, null);
    for (const actor of [friend, admin]) {
      assert.throws(() => updateSchedule(actor, serverId, schedule.id, {
        ...input, enabled: false, time: "09:00", revision: 2,
      }), /permission|required access/);
      assert.throws(() => setScheduleEnabled(actor, serverId, schedule.id, {
        enabled: true, revision: 2,
      }), /permission|required access/);
    }
    assert.equal(listSchedules(admin, serverId)[0].revision, 2);
  });

  it("permits administrators to pause disabled owners without transferring their schedules", () => {
    const schedule = createSchedule(friend, serverId, input);
    updateUserAccess(friend.id, "operator", true);
    const paused = setScheduleEnabled(admin, serverId, schedule.id, { enabled: false, revision: 1 });
    assert.equal(paused.ownerId, friend.id);
    assert.throws(() => setScheduleEnabled(admin, serverId, schedule.id, {
      enabled: true, revision: 2,
    }), /account is disabled/);
  });

  it("keeps materially changed bindings suspended after pause, edit, or resume attempts", () => {
    const schedule = createSchedule(friend, serverId, input);
    const pending = reconcileServers([{ ...observation, gameType: "factorio" }])[0];
    reviewServerBinding(serverId, pending.pendingFingerprint!);
    assert.equal(listSchedules(admin, serverId)[0].nextRunAt, null);
    const paused = setScheduleEnabled(friend, serverId, schedule.id, { enabled: false, revision: 1 });
    assert.equal(paused.enabled, false);
    assert.throws(() => updateSchedule(admin, serverId, schedule.id, {
      ...input, time: "09:00", revision: 2,
    }), /server configuration changed/);
    assert.throws(() => setScheduleEnabled(admin, serverId, schedule.id, {
      enabled: true, revision: 2,
    }), /server configuration changed/);
    assert.equal(getDatabase().prepare("SELECT binding_revision FROM schedules WHERE id=?")
      .get(schedule.id)?.binding_revision, 1);
  });

  it("previews enabled authorized schedules and does not consume slots while paused", () => {
    spyOn(Date, "now").mockReturnValue(due - 60_000);
    const schedule = createSchedule(friend, serverId, input);
    assert.equal(schedule.nextRunAt, due);
    setScheduleEnabled(friend, serverId, schedule.id, { enabled: false, revision: 1 });
    runSchedules(due);
    assert.equal(serverOperations().length, 0);
    const resumed = setScheduleEnabled(friend, serverId, schedule.id, { enabled: true, revision: 2 });
    assert.equal(resumed.nextRunAt, due);
    assert.equal(resumed.revision, 3);
    runSchedules(due);
    assert.equal(getOperation(serverOperations()[0].id)?.input.scheduleRevision, 3);
    assert.equal(listSchedules(friend, serverId)[0].nextRunAt, due + 86_400_000);
  });

  for (const action of ["schedule.updated", "schedule.paused", "schedule.resumed"])
    it(`rolls back ${action} together with a failed audit`, () => {
      const enabled = action !== "schedule.resumed";
      const schedule = createSchedule(friend, serverId, { ...input, enabled });
      const db = getDatabase();
      db.exec(`CREATE TRIGGER fail_schedule_mutation_audit BEFORE INSERT ON audit_log
        WHEN NEW.action='${action}'
        BEGIN SELECT RAISE(ABORT, 'fixture audit failed'); END`);
      assert.throws(() => action === "schedule.updated"
        ? updateSchedule(friend, serverId, schedule.id, { ...input, time: "09:00", revision: 1 })
        : setScheduleEnabled(friend, serverId, schedule.id, { enabled: !enabled, revision: 1 }),
      /fixture audit failed/);
      const current = listSchedules(friend, serverId)[0];
      assert.equal(current.revision, 1);
      assert.equal(current.enabled, enabled);
      assert.equal(current.time, input.time);
      assert.equal(db.inTransaction, false);
    });

  it("commits mutation before retention cleanup and hides cleanup failures", () => {
    const schedule = createSchedule(friend, serverId, input);
    const db = getDatabase();
    const warn = spyOn(console, "warn").mockImplementation(() => {});
    const cleanup = spyOn(database, "pruneAuditLogIfNeeded").mockImplementation(() => {
      assert.equal(db.inTransaction, false);
      throw new Error("fixture-private-cleanup-error");
    });
    assert.equal(setScheduleEnabled(friend, serverId, schedule.id, {
      enabled: false, revision: 1,
    }).revision, 2);
    assert.equal(cleanup.mock.calls.length, 1);
    assert.doesNotMatch(JSON.stringify(warn.mock.calls), /fixture-private-cleanup-error/);
  });
});

describe("schedule outcomes", () => {
  it("creates paused schedules without consuming a slot or fabricating a run", () => {
    const schedule = createSchedule(friend, serverId, { ...input, enabled: false });
    runSchedules(due);
    assert.equal(serverOperations().length, 0);
    assert.deepEqual(listSchedules(friend, serverId)[0], schedule);
    assert.equal(schedule.lastOperation, null);
    assert.equal(schedule.lastRunAt, null);
    assert.equal(schedule.nextRunAt, null);
    assert.equal(schedule.nextRunUnavailableReason, null);
  });

  it("projects live queued, running, and completed outcomes without exposing job state", async () => {
    createSchedule(friend, serverId, input);
    runSchedules(due + 12_345);
    const queued = listSchedules(friend, serverId)[0];
    assert.equal(queued.lastRunAt, due + 12_345);
    assert.equal(queued.lastOperation?.status, "queued");
    let enter!: () => void;
    let finish!: () => void;
    const entered = new Promise<void>((resolve) => { enter = resolve; });
    const finished = new Promise<void>((resolve) => { finish = resolve; });
    registerJobHandler("start", {
      run: async (context) => {
        context.progress("starting", { privateFixtureDetail: "fixture-private-recovery" });
        enter();
        await finished;
        return { action: "start" };
      },
    });
    await startOperationRunner();
    await entered;
    try {
      const running = listSchedules(friend, serverId)[0];
      assert.equal(running.lastOperation?.status, "running");
      assert.equal(running.lastOperation?.phase, "starting");
      assert.equal(running.lastRunAt, queued.lastRunAt);
      assert.equal(running.lastOperation?.id, queued.lastOperation?.id);
      assert.doesNotMatch(JSON.stringify(running), /fixture-private-recovery|actorId|recovery|scheduleRevision/);
    } finally {
      finish();
      await stopOperationRunner();
    }
    const completed = listSchedules(friend, serverId)[0];
    assert.equal(completed.lastOperation?.status, "succeeded");
    assert.deepEqual(completed.lastOperation?.result, { action: "start" });
    assert.equal(completed.lastRunAt, queued.lastRunAt);
  });

  it("shows the operation's actual failure after dispatch", async () => {
    createSchedule(friend, serverId, input);
    runSchedules(due);
    registerJobHandler("start", {
      run: () => { throw new AppError("FIXTURE_START_FAILED", 409, "The fixture could not start."); },
    });
    await startOperationRunner();
    for (let attempt = 0; attempt < 100; attempt++) {
      if (listSchedules(friend, serverId)[0].lastOperation?.status === "failed") break;
      await new Promise((resolve) => setTimeout(resolve, 5));
    }
    const schedule = listSchedules(friend, serverId)[0];
    assert.equal(schedule.lastOperation?.status, "failed");
    assert.equal(schedule.lastOperation?.error, "The fixture could not start.");
    assert.equal(schedule.lastRunAt, due);
  });

  it("follows interrupted operation recovery through to its actual outcome", async () => {
    createSchedule(friend, serverId, input);
    runSchedules(due);
    getDatabase().prepare("UPDATE operations SET status='running',phase='stopping'").run();
    let enter!: () => void;
    let finish!: () => void;
    const entered = new Promise<void>((resolve) => { enter = resolve; });
    const finished = new Promise<void>((resolve) => { finish = resolve; });
    registerJobHandler("start", {
      run: async () => {},
      recover: async (context) => {
        context.progress("restarting");
        enter();
        await finished;
      },
    });
    const starting = startOperationRunner();
    await entered;
    try {
      assert.equal(listSchedules(friend, serverId)[0].lastOperation?.phase, "restarting");
    } finally {
      finish();
      await starting;
      await stopOperationRunner();
    }
    const schedule = listSchedules(friend, serverId)[0];
    assert.equal(schedule.lastOperation?.status, "interrupted");
    assert.match(schedule.lastOperation?.error ?? "", /Ludock restarted/);
    assert.equal(schedule.lastRunAt, due);
  });

  it("keeps a newer skipped attempt after an older queued operation finishes", () => {
    createSchedule(friend, serverId, input);
    runSchedules(due);
    const older = listSchedules(friend, serverId)[0].lastOperation!;
    runSchedules(due + 86_400_000);
    getDatabase().prepare("UPDATE operations SET status='succeeded',phase='succeeded' WHERE id=?").run(older.id);
    const latest = listSchedules(friend, serverId)[0];
    assert.equal(latest.lastOperation, null);
    assert.equal(latest.lastRunAt, due + 86_400_000);
    assert.match(latest.lastResult!, /Skipped:/);
    runSchedules(due + 86_400_000 + 30_000);
    assert.equal(listSchedules(friend, serverId)[0].lastRunAt, latest.lastRunAt);
  });

  it("never projects an unrelated or malformed operation reference", () => {
    const schedule = createSchedule(friend, serverId, input);
    runSchedules(due);
    const operation = listSchedules(friend, serverId)[0].lastOperation!;
    const another = reconcileServers([observation, { ...observation, name: "another", containerId: "another" }])
      .find((server) => server.id !== serverId)!;
    const db = getDatabase();
    for (const [targetServer, owner, operationInput] of [
      [another.id, friend.id, JSON.stringify({ scheduleId: schedule.id })],
      [serverId, other.id, JSON.stringify({ scheduleId: schedule.id })],
      [serverId, friend.id, JSON.stringify({ scheduleId: "another-schedule", credential: "fixture-secret" })],
      [serverId, friend.id, "malformed-fixture-secret"],
      [serverId, friend.id, "[]"],
    ]) {
      db.prepare("UPDATE operations SET server_id=?,actor_id=?,input_json=? WHERE id=?")
        .run(targetServer, owner, operationInput, operation.id);
      const current = listSchedules(friend, serverId)[0];
      assert.equal(current.lastOperation, null);
      assert.doesNotMatch(JSON.stringify(current), /fixture-secret/);
    }
  });

  it("isolates invalid public fields on associated historical operations", () => {
    createSchedule(friend, serverId, input);
    runSchedules(due);
    const operation = listSchedules(friend, serverId)[0].lastOperation!;
    const db = getDatabase();
    for (const [status, createdAt, updatedAt] of [
      ["cancelled", 1, 1], ["succeeded", -1, 1], ["succeeded", 1, -1],
    ]) {
      db.prepare("UPDATE operations SET status=?,created_at=?,updated_at=? WHERE id=?")
        .run(status, createdAt, updatedAt, operation.id);
      const schedule = listSchedules(friend, serverId)[0];
      assert.equal(schedule.lastOperation, null);
      assert.equal(schedule.lastRunAt, due);
    }
  });

  it("rolls back queued work if associating the attempt fails and still consumes the skipped slot", () => {
    const schedule = createSchedule(friend, serverId, input);
    const db = getDatabase();
    db.exec(`CREATE TRIGGER fail_operation_link BEFORE UPDATE ON schedules
      WHEN NEW.last_operation_id IS NOT NULL
      BEGIN SELECT RAISE(ABORT, 'fixture association failed'); END`);
    runSchedules(due);
    assert.equal(serverOperations().length, 0);
    assert.equal(db.prepare("SELECT COUNT(*) AS count FROM audit_log WHERE action='server.start.queued'").get()?.count, 0);
    const skipped = listSchedules(friend, serverId)[0];
    assert.equal(skipped.id, schedule.id);
    assert.equal(skipped.lastOperation, null);
    assert.equal(skipped.lastRunAt, due);
    assert.match(skipped.lastResult!, /Skipped/);
    db.exec("DROP TRIGGER fail_operation_link");
    runSchedules(due + 30_000);
    assert.equal(serverOperations().length, 0);
    assert.equal(db.inTransaction, false);
  });

  it("prunes audit retention after committing the associated operation", () => {
    createSchedule(friend, serverId, input);
    const db = getDatabase();
    const warn = spyOn(console, "warn").mockImplementation(() => {});
    const cleanup = spyOn(database, "pruneAuditLogIfNeeded").mockImplementation(() => {
      assert.equal(db.inTransaction, false);
      assert.equal(listSchedules(friend, serverId)[0].lastOperation?.status, "queued");
      throw new Error("fixture-private-cleanup-error");
    });
    runSchedules(due);
    assert.equal(cleanup.mock.calls.length, 1);
    assert.equal(listSchedules(friend, serverId)[0].lastOperation?.status, "queued");
    assert.doesNotMatch(JSON.stringify(warn.mock.calls), /fixture-private-cleanup-error/);
  });
});

describe("schedule preview availability reasons", () => {
  it("bounds repeated authority reads to distinct owners, actions, and binding baselines", () => {
    insertScheduleRows(1);
    const owners = spyOn(database, "findUserById");
    const bindings = spyOn(identity, "resolveServerBinding");
    assert.ok(listSchedules(admin, serverId)[0].nextRunAt);
    const singleOwnerReads = owners.mock.calls.filter(([id]) => id === friend.id).length;
    const singleBindingReads = bindings.mock.calls.length;
    assert.ok(singleOwnerReads > 0);
    assert.ok(singleBindingReads > 0);

    insertScheduleRows(MAX_SCHEDULES_PER_SERVER - 1);
    owners.mockClear();
    bindings.mockClear();
    const schedules = listSchedules(admin, serverId);
    assert.equal(schedules.length, MAX_SCHEDULES_PER_SERVER);
    assert.ok(schedules.every((schedule) => schedule.nextRunAt !== null));
    assert.equal(owners.mock.calls.filter(([id]) => id === friend.id).length, singleOwnerReads);
    assert.equal(bindings.mock.calls.length, singleBindingReads);
  });

  it("keeps different owners, actions, and binding baselines independent within a list", () => {
    const [available] = insertScheduleRows(1);
    const [revokedOwner] = insertScheduleRows(1, { ownerId: other.id });
    const [revokedAction] = insertScheduleRows(1, { data: { ...input, action: "stop" } });
    const [changedBinding] = insertScheduleRows(1);
    setServerGrant(other.id, serverId, ["server.view", "schedules.manage"], admin);
    getDatabase().prepare("UPDATE schedules SET binding_revision=999 WHERE id=?").run(changedBinding);

    const schedules = new Map(listSchedules(admin, serverId).map((schedule) => [schedule.id, schedule]));
    assert.ok(schedules.get(available)!.nextRunAt);
    assert.equal(schedules.get(available)!.nextRunUnavailableReason, null);
    assert.equal(schedules.get(revokedOwner)!.nextRunUnavailableReason, "action_access_removed");
    assert.equal(schedules.get(revokedAction)!.nextRunUnavailableReason, "action_access_removed");
    assert.equal(schedules.get(changedBinding)!.nextRunUnavailableReason, "binding_changed");
  });

  it("reuses unavailable outcomes only within the current list request", () => {
    insertScheduleRows(MAX_SCHEDULES_PER_SERVER);
    updateUserAccess(friend.id, "operator", true);
    const owners = spyOn(database, "findUserById");
    assert.ok(listSchedules(admin, serverId)
      .every((schedule) => schedule.nextRunUnavailableReason === "owner_disabled"));
    assert.equal(owners.mock.calls.filter(([id]) => id === friend.id).length, 1);

    updateUserAccess(friend.id, "operator", false);
    assert.ok(listSchedules(admin, serverId).every((schedule) => schedule.nextRunAt !== null));
    setServerGrant(friend.id, serverId, ["server.view", "schedules.manage"], admin);
    assert.ok(listSchedules(admin, serverId)
      .every((schedule) => schedule.nextRunUnavailableReason === "action_access_removed"));
  });

  it("distinguishes missing schedule-management grants, action grants, and disabled owners", () => {
    createSchedule(friend, serverId, input);
    setServerGrant(friend.id, serverId, ["server.view", "schedules.manage"], admin);
    assert.equal(listSchedules(admin, serverId)[0].nextRunUnavailableReason, "action_access_removed");
    setServerGrant(friend.id, serverId, ["server.view", "server.start"], admin);
    assert.equal(listSchedules(admin, serverId)[0].nextRunUnavailableReason, "owner_access_removed");
    updateUserAccess(friend.id, "viewer", false);
    assert.equal(listSchedules(admin, serverId)[0].nextRunUnavailableReason, "owner_access_removed");
    updateUserAccess(friend.id, "operator", true);
    assert.equal(listSchedules(admin, serverId)[0].nextRunUnavailableReason, "owner_disabled");
  });

  it("reports a missing owner without revealing private account details", () => {
    createSchedule(friend, serverId, input);
    const original = database.findUserById;
    spyOn(database, "findUserById").mockImplementation((id) => id === friend.id ? null : original(id));
    const schedule = listSchedules(admin, serverId)[0];
    assert.equal(schedule.nextRunUnavailableReason, "owner_missing");
    assert.equal(schedule.nextRunAt, null);
  });

  it("distinguishes unavailable bindings from accepted material baseline changes", () => {
    createSchedule(friend, serverId, input);
    getDatabase().prepare("UPDATE logical_servers SET container_id=NULL WHERE id=?").run(serverId);
    assert.equal(listSchedules(admin, serverId)[0].nextRunUnavailableReason, "binding_unavailable");
    const pending = reconcileServers([{ ...observation, gameType: "factorio" }])[0];
    reviewServerBinding(serverId, pending.pendingFingerprint!);
    assert.equal(listSchedules(admin, serverId)[0].nextRunUnavailableReason, "binding_changed");
    getDatabase().prepare("UPDATE logical_servers SET container_id=NULL WHERE id=?").run(serverId);
    assert.equal(listSchedules(admin, serverId)[0].nextRunUnavailableReason, "binding_changed");
  });

  it("clears the unavailability reason while paused and hides unexpected diagnostics", () => {
    const schedule = createSchedule(friend, serverId, input);
    spyOn(identity, "resolveServerBinding").mockImplementation(() => {
      throw new Error("fixture-private-binding-detail");
    });
    const unavailable = listSchedules(admin, serverId)[0];
    assert.equal(unavailable.nextRunUnavailableReason, "unavailable");
    assert.doesNotMatch(JSON.stringify(unavailable), /fixture-private-binding-detail/);
    const paused = setScheduleEnabled(admin, serverId, schedule.id, { enabled: false, revision: 1 });
    assert.equal(paused.nextRunAt, null);
    assert.equal(paused.nextRunUnavailableReason, null);
  });
});

describe("schedule failure isolation", () => {
  for (const [kind, inputJson] of [
    ["malformed JSON", "fixture-schedule-private-detail: not JSON"],
    ["invalid schema", JSON.stringify({ ...input, action: "fixture-schedule-private-detail" })],
    ["invalid timezone", JSON.stringify({ ...input, timezone: "fixture-schedule-private-detail" })],
  ]) {
    it(`isolates a selected row with ${kind} and still queues the next due schedule`, () => {
      const warn = spyOn(console, "warn").mockImplementation(() => {});
      const [invalidId] = insertScheduleRows(1, { inputJson });
      const valid = createSchedule(friend, serverId, input);

      assert.doesNotThrow(() => runSchedules(due));

      const operations = serverOperations();
      assert.equal(operations.length, 1);
      assert.equal(getOperation(operations[0].id)?.input.scheduleId, valid.id);
      const invalid = getDatabase().prepare(
        "SELECT last_slot,last_result FROM schedules WHERE id=?",
      ).get(invalidId) as { last_slot: string | null; last_result: string | null };
      assert.equal(invalid.last_slot, null);
      assert.match(invalid.last_result!, /Suspended: saved schedule configuration is invalid; recreate/);
      assert.equal(warn.mock.calls.length, 1);
      assert.doesNotMatch(JSON.stringify(warn.mock.calls), /fixture-schedule-private-detail/);
      assert.doesNotMatch(invalid.last_result!, /fixture-schedule-private-detail/);

      runSchedules(due + 30_000);
      assert.equal(serverOperations().length, 1);
      assert.equal(warn.mock.calls.length, 1, "unchanged invalid rows must not repeat diagnostics");
    });
  }

  it("does not evaluate disabled schedules or consume their slots", () => {
    const warn = spyOn(console, "warn").mockImplementation(() => {});
    const disabled = createSchedule(friend, serverId, { ...input, enabled: false });
    const valid = createSchedule(friend, serverId, input);

    runSchedules(due);

    const operations = serverOperations();
    assert.equal(operations.length, 1);
    assert.equal(getOperation(operations[0].id)?.input.scheduleId, valid.id);
    const state = getDatabase().prepare(
      "SELECT last_slot,last_result FROM schedules WHERE id=?",
    ).get(disabled.id) as { last_slot: string | null; last_result: string | null };
    assert.equal(state.last_slot, null);
    assert.equal(state.last_result, null);
    assert.equal(warn.mock.calls.length, 0);
  });

  it("continues when recording an invalid row's result fails", () => {
    const warn = spyOn(console, "warn").mockImplementation(() => {});
    insertScheduleRows(1, { inputJson: "not valid JSON" });
    const valid = createSchedule(friend, serverId, input);
    getDatabase().exec(`CREATE TRIGGER fail_invalid_schedule_result BEFORE UPDATE ON schedules
      WHEN OLD.input_json='not valid JSON'
      BEGIN SELECT RAISE(ABORT, 'fixture-schedule-private-detail'); END`);

    assert.doesNotThrow(() => runSchedules(due));

    const operations = serverOperations();
    assert.equal(operations.length, 1);
    assert.equal(getOperation(operations[0].id)?.input.scheduleId, valid.id);
    assert.equal(warn.mock.calls.length, 1);
    assert.match(String(warn.mock.calls[0][0]), /Could not record or notify/);
    assert.doesNotMatch(JSON.stringify(warn.mock.calls), /fixture-schedule-private-detail/);
  });
});

describe("schedule resource limits", () => {
  it("rejects schedule payloads above the endpoint-specific byte limit", () => {
    assert.throws(
      () =>
        createSchedule(friend, serverId, {
          ...input,
          padding: "x".repeat(MAX_SCHEDULE_PAYLOAD_BYTES),
        }),
      (error) =>
        error instanceof AppError &&
        error.code === "SCHEDULE_PAYLOAD_TOO_LARGE" &&
        error.statusCode === 413,
    );
    assert.equal(listSchedules(friend, serverId).length, 0);
  });

  it("caps each server and every list response", () => {
    insertScheduleRows(MAX_SCHEDULES_PER_SERVER, { ownerId: admin.id });
    insertScheduleRows(1, { ownerId: friend.id });

    assert.equal(listSchedules(admin, serverId).length, MAX_SCHEDULES_PER_SERVER);
    assert.equal(listSchedules(friend, serverId).length, 1);
    assert.throws(
      () => createSchedule(admin, serverId, input),
      (error) =>
        error instanceof AppError &&
        error.code === "SCHEDULE_LIMIT_REACHED" &&
        error.message.includes(String(MAX_SCHEDULES_PER_SERVER)),
    );
  });

  it("caps total schedules and scheduler work per tick", () => {
    const warn = spyOn(console, "warn").mockImplementation(() => {});
    insertScheduleRows(MAX_SCHEDULES_TOTAL, {
      ownerId: admin.id,
      data: { ...input, enabled: false },
    });
    const nextServer = reconcileServers([
      observation,
      {
        ...observation,
        containerId: "second",
        name: "second-world",
        displayName: "Second World",
      },
    ]).find((server) => server.containerId === "second")!;

    assert.throws(
      () => createSchedule(admin, nextServer.id, input),
      (error) =>
        error instanceof AppError &&
        error.code === "SCHEDULE_LIMIT_REACHED" &&
        error.message.includes(String(MAX_SCHEDULES_TOTAL)),
    );
    assert.doesNotThrow(() => runSchedules(due));
    assert.equal(warn.mock.calls.length, 0, "exactly the supported cap is not overflow");
    const [overflowId] = insertScheduleRows(1, {
      ownerId: admin.id,
      inputJson: "not valid JSON",
    });
    assert.doesNotThrow(() => runSchedules(due));
    assert.equal(warn.mock.calls.length, 1);
    assert.match(String(warn.mock.calls[0][0]), /Schedule limit exceeded/);
    const overflow = getDatabase().prepare(
      "SELECT last_slot,last_result FROM schedules WHERE id=?",
    ).get(overflowId) as { last_slot: string | null; last_result: string | null };
    assert.equal(overflow.last_slot, null);
    assert.equal(overflow.last_result, null, "rows beyond the cap must not be evaluated");
  });
});
