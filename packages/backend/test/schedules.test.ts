import { thrownBy } from "./fixtures/errors.js";
import { expect, afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";
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
  setScheduleEnabled,
  updateSchedule,
} from "../src/schedules.js";
import { AppError } from "../src/errors.js";
import { setServerGrant } from "./fixtures/grants.js";
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

    expect(cleanup.mock.calls.length).toBe(1);
    expect(cleanupInTransaction).toBe(false);
    expect(listSchedules(friend, serverId)[0].id).toBe(schedule.id);
    const audit = db.prepare(
      "SELECT details_json FROM audit_log WHERE action='schedule.created'",
    ).get() as { details_json: string };
    expect(JSON.parse(audit.details_json).scheduleId).toBe(schedule.id);
    expect(warn.mock.calls.length).toBe(1);
    expect(String(warn.mock.calls[0][0])).toMatch(/retention cleanup failed/);
    expect(String(warn.mock.calls[0][0])).not.toMatch(/fixture-retention-private-detail/);
  });

  it("rolls back creation when its audit cannot be written", () => {
    const db = getDatabase();
    db.exec(`CREATE TRIGGER fail_schedule_audit BEFORE INSERT ON audit_log
      WHEN NEW.action='schedule.created'
      BEGIN SELECT RAISE(ABORT, 'fixture schedule audit failure'); END`);

    expect(() => createSchedule(friend, serverId, input)).toThrow(/fixture schedule audit failure/);
    expect(listSchedules(friend, serverId).length).toBe(0);
    expect(db.inTransaction).toBe(false);
  });

  it("preserves the original failure when SQLite has already rolled back", () => {
    const db = getDatabase();
    db.exec(`CREATE TRIGGER rollback_schedule_audit BEFORE INSERT ON audit_log
      WHEN NEW.action='schedule.created'
      BEGIN SELECT RAISE(ROLLBACK, 'fixture original audit failure'); END`);

    expect(() => createSchedule(friend, serverId, input)).toThrow(/fixture original audit failure/);
    expect(listSchedules(friend, serverId).length).toBe(0);
    expect(db.inTransaction).toBe(false);
  });
});

describe("schedule authority", () => {
  it("requires an independently granted action, in addition to schedule management", () => {
    expect(() => createSchedule(friend, serverId, { ...input, action: "backup" })).toThrow(/permission/i);
    expect(() => createSchedule(friend, serverId, { ...input, action: "restart" })).toThrow(/permission/i);
    expect(listSchedules(friend, serverId).length).toBe(0);
  });
  it("hides other owners' schedules and prevents changing their authority", () => {
    const schedule = createSchedule(admin, serverId, input);
    expect(listSchedules(friend, serverId).length).toBe(0);
    expect(() => deleteSchedule(friend, serverId, schedule.id)).toThrow(/not found/);
    expect(listSchedules(admin, serverId).length).toBe(1);
  });
  it("uses current account roles when enforcing schedule ownership", () => {
    const schedule = createSchedule(admin, serverId, input);
    const staleAdministrator = { ...friend, role: "admin" as const };
    expect(listSchedules(staleAdministrator, serverId).length).toBe(0);
    expect(() => deleteSchedule(staleAdministrator, serverId, schedule.id)).toThrow(/not found/);
  });
  it("does not run twice in a slot and does not catch up after a missed slot", () => {
    createSchedule(friend, serverId, input);
    runSchedules(due);
    runSchedules(due + 30_000);
    expect(serverOperations().length).toBe(1);
    getDatabase().prepare("UPDATE operations SET status='succeeded'").run();
    runSchedules(due + 86400_000 + 60_000);
    expect(serverOperations().length).toBe(1);
  });
  it("blocks revoked actions and records an actionable suspension", () => {
    createSchedule(friend, serverId, input);
    expect(listSchedules(friend, serverId)[0].nextRunAt).toBeTruthy();
    setServerGrant(
      friend.id,
      serverId,
      ["server.view", "schedules.manage"],
      admin,
    );
    runSchedules(due);
    expect(serverOperations().length).toBe(0);
    expect(listSchedules(admin, serverId)[0].lastResult!).toMatch(/Suspended: required access/);
  });
  it("blocks disabled owners and removes schedules with deleted owners", () => {
    createSchedule(friend, serverId, input);
    expect(listSchedules(friend, serverId)[0].nextRunAt).toBeTruthy();
    updateUserAccess(friend.id, "operator", true);
    runSchedules(due);
    expect(serverOperations().length).toBe(0);
    expect(listSchedules(admin, serverId)[0].lastResult!).toMatch(/Suspended/);
    deleteUser(friend.id);
    expect(listSchedules(admin, serverId).length).toBe(0);
  });
  it("follows validated ordinary recreation using its new binding revision", () => {
    createSchedule(friend, serverId, input);
    const replacement = reconcileServers([
      { ...observation, containerId: "replacement" },
    ])[0];
    runSchedules(due);
    const operations = serverOperations();
    expect(operations.length).toBe(1);
    expect(getOperation(operations[0].id)?.bindingRevision).toBe(replacement.bindingRevision);
  });
  it("keeps schedules suspended after material data changes until explicitly recreated", () => {
    createSchedule(friend, serverId, input);
    expect(listSchedules(friend, serverId)[0].nextRunAt).toBeTruthy();
    const pending = reconcileServers([
      { ...observation, gameType: "factorio" },
    ])[0];
    reviewServerBinding(serverId, pending.pendingFingerprint!);
    runSchedules(due);
    expect(serverOperations().length).toBe(0);
    expect(listSchedules(admin, serverId)[0].lastResult!).toMatch(/server configuration changed/);
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
    expect(edited.id).toBe(created.id);
    expect(edited.ownerId).toBe(friend.id);
    expect(edited.revision).toBe(2);
    expect(edited.time).toBe("09:00");
    const after = getDatabase().prepare("SELECT * FROM schedules WHERE id=?")
      .get(created.id) as Record<string, unknown>;
    for (const key of ["owner_id", "binding_revision", "last_slot", "last_result", "created_at"])
      expect(after[key]).toBe(before[key]);
    updateSchedule(friend, serverId, created.id, { ...input, revision: 2 });
    getDatabase().prepare("UPDATE operations SET status='succeeded'").run();
    runSchedules(due + 30_000);
    expect(serverOperations().length).toBe(1);
  });

  it("rejects stale edits and toggles without losing newer input", () => {
    const schedule = createSchedule(friend, serverId, input);
    setScheduleEnabled(friend, serverId, schedule.id, { enabled: false, revision: 1 });
    for (const mutate of [
      () => updateSchedule(friend, serverId, schedule.id, { ...input, time: "09:00", revision: 1 }),
      () => setScheduleEnabled(friend, serverId, schedule.id, { enabled: true, revision: 1 }),
    ]) expect(thrownBy(mutate)).toSatisfy((error: unknown) =>
      error instanceof AppError && error.code === "SCHEDULE_CHANGED" && error.statusCode === 409);
    const current = listSchedules(friend, serverId)[0];
    expect(current.revision).toBe(2);
    expect(current.enabled).toBe(false);
    expect(current.time).toBe(input.time);
  });

  it("does not change revision or audit repeated identical updates", () => {
    const schedule = createSchedule(friend, serverId, input);
    const edited = updateSchedule(friend, serverId, schedule.id, {
      ...input, days: [...input.days].reverse(), revision: 1,
    });
    expect(edited.revision).toBe(1);
    expect(setScheduleEnabled(friend, serverId, schedule.id, {
      enabled: true, revision: 1,
    }).revision).toBe(1);
    expect(getDatabase().prepare(
      "SELECT COUNT(*) AS count FROM audit_log WHERE action IN ('schedule.updated','schedule.resumed')",
    ).get()?.count).toBe(0);
  });

  it("recognizes newly selected weekdays even when older input repeats a day", () => {
    const schedule = createSchedule(friend, serverId, { ...input, days: [1, 1] });
    const edited = updateSchedule(friend, serverId, schedule.id, {
      ...input, days: [1, 2], revision: 1,
    });
    expect(edited.revision).toBe(2);
    expect(edited.days).toStrictEqual([1, 2]);
  });

  it("uses current ownership authority for editing and toggling", () => {
    const schedule = createSchedule(admin, serverId, input);
    const staleAdministrator = { ...friend, role: "admin" as const };
    expect(() => updateSchedule(staleAdministrator, serverId, schedule.id, {
      ...input, time: "09:00", revision: 1,
    })).toThrow(/not found/);
    expect(() => setScheduleEnabled(staleAdministrator, serverId, schedule.id, {
      enabled: false, revision: 1,
    })).toThrow(/not found/);
  });

  it("requires action grants for both requester and original owner", () => {
    const schedule = createSchedule(friend, serverId, input);
    expect(() => updateSchedule(friend, serverId, schedule.id, {
      ...input, action: "backup", revision: 1,
    })).toThrow(/permission/);
    expect(() => updateSchedule(admin, serverId, schedule.id, {
      ...input, action: "backup", revision: 1,
    })).toThrow(/required access/);
    expect(listSchedules(admin, serverId)[0].revision).toBe(1);
  });

  it("allows pausing after action revocation but prevents edits and resume under either actor", () => {
    const schedule = createSchedule(friend, serverId, input);
    setServerGrant(friend.id, serverId, ["server.view", "schedules.manage"], admin);
    expect(listSchedules(admin, serverId)[0].nextRunAt).toBe(null);
    const paused = setScheduleEnabled(friend, serverId, schedule.id, { enabled: false, revision: 1 });
    expect(paused.enabled).toBe(false);
    expect(paused.nextRunAt).toBe(null);
    for (const actor of [friend, admin]) {
      expect(() => updateSchedule(actor, serverId, schedule.id, {
        ...input, enabled: false, time: "09:00", revision: 2,
      })).toThrow(/permission|required access/);
      expect(() => setScheduleEnabled(actor, serverId, schedule.id, {
        enabled: true, revision: 2,
      })).toThrow(/permission|required access/);
    }
    expect(listSchedules(admin, serverId)[0].revision).toBe(2);
  });

  it("permits administrators to pause disabled owners without transferring their schedules", () => {
    const schedule = createSchedule(friend, serverId, input);
    updateUserAccess(friend.id, "operator", true);
    const paused = setScheduleEnabled(admin, serverId, schedule.id, { enabled: false, revision: 1 });
    expect(paused.ownerId).toBe(friend.id);
    expect(() => setScheduleEnabled(admin, serverId, schedule.id, {
      enabled: true, revision: 2,
    })).toThrow(/account is disabled/);
  });

  it("keeps materially changed bindings suspended after pause, edit, or resume attempts", () => {
    const schedule = createSchedule(friend, serverId, input);
    const pending = reconcileServers([{ ...observation, gameType: "factorio" }])[0];
    reviewServerBinding(serverId, pending.pendingFingerprint!);
    expect(listSchedules(admin, serverId)[0].nextRunAt).toBe(null);
    const paused = setScheduleEnabled(friend, serverId, schedule.id, { enabled: false, revision: 1 });
    expect(paused.enabled).toBe(false);
    expect(() => updateSchedule(admin, serverId, schedule.id, {
      ...input, time: "09:00", revision: 2,
    })).toThrow(/server configuration changed/);
    expect(() => setScheduleEnabled(admin, serverId, schedule.id, {
      enabled: true, revision: 2,
    })).toThrow(/server configuration changed/);
    expect(getDatabase().prepare("SELECT binding_revision FROM schedules WHERE id=?")
      .get(schedule.id)?.binding_revision).toBe(1);
  });

  it("previews enabled authorized schedules and does not consume slots while paused", () => {
    spyOn(Date, "now").mockReturnValue(due - 60_000);
    const schedule = createSchedule(friend, serverId, input);
    expect(schedule.nextRunAt).toBe(due);
    setScheduleEnabled(friend, serverId, schedule.id, { enabled: false, revision: 1 });
    runSchedules(due);
    expect(serverOperations().length).toBe(0);
    const resumed = setScheduleEnabled(friend, serverId, schedule.id, { enabled: true, revision: 2 });
    expect(resumed.nextRunAt).toBe(due);
    expect(resumed.revision).toBe(3);
    runSchedules(due);
    expect(getOperation(serverOperations()[0].id)?.input.scheduleRevision).toBe(3);
    expect(listSchedules(friend, serverId)[0].nextRunAt).toBe(due + 86_400_000);
  });

  for (const action of ["schedule.updated", "schedule.paused", "schedule.resumed"])
    it(`rolls back ${action} together with a failed audit`, () => {
      const enabled = action !== "schedule.resumed";
      const schedule = createSchedule(friend, serverId, { ...input, enabled });
      const db = getDatabase();
      db.exec(`CREATE TRIGGER fail_schedule_mutation_audit BEFORE INSERT ON audit_log
        WHEN NEW.action='${action}'
        BEGIN SELECT RAISE(ABORT, 'fixture audit failed'); END`);
      expect(() => action === "schedule.updated"
        ? updateSchedule(friend, serverId, schedule.id, { ...input, time: "09:00", revision: 1 })
        : setScheduleEnabled(friend, serverId, schedule.id, { enabled: !enabled, revision: 1 })).toThrow(/fixture audit failed/);
      const current = listSchedules(friend, serverId)[0];
      expect(current.revision).toBe(1);
      expect(current.enabled).toBe(enabled);
      expect(current.time).toBe(input.time);
      expect(db.inTransaction).toBe(false);
    });

  it("commits mutation before retention cleanup and hides cleanup failures", () => {
    const schedule = createSchedule(friend, serverId, input);
    const db = getDatabase();
    const warn = spyOn(console, "warn").mockImplementation(() => {});
    const cleanup = spyOn(database, "pruneAuditLogIfNeeded").mockImplementation(() => {
      expect(db.inTransaction).toBe(false);
      throw new Error("fixture-private-cleanup-error");
    });
    expect(setScheduleEnabled(friend, serverId, schedule.id, {
      enabled: false, revision: 1,
    }).revision).toBe(2);
    expect(cleanup.mock.calls.length).toBe(1);
    expect(JSON.stringify(warn.mock.calls)).not.toMatch(/fixture-private-cleanup-error/);
  });
});

describe("schedule outcomes", () => {
  it("creates paused schedules without consuming a slot or fabricating a run", () => {
    const schedule = createSchedule(friend, serverId, { ...input, enabled: false });
    runSchedules(due);
    expect(serverOperations().length).toBe(0);
    expect(listSchedules(friend, serverId)[0]).toStrictEqual(schedule);
    expect(schedule.lastOperation).toBe(null);
    expect(schedule.lastRunAt).toBe(null);
    expect(schedule.nextRunAt).toBe(null);
    expect(schedule.nextRunUnavailableReason).toBe(null);
  });

  it("projects live queued, running, and completed outcomes without exposing job state", async () => {
    createSchedule(friend, serverId, input);
    runSchedules(due + 12_345);
    const queued = listSchedules(friend, serverId)[0];
    expect(queued.lastRunAt).toBe(due + 12_345);
    expect(queued.lastOperation?.status).toBe("queued");
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
      expect(running.lastOperation?.status).toBe("running");
      expect(running.lastOperation?.phase).toBe("starting");
      expect(running.lastRunAt).toBe(queued.lastRunAt);
      expect(running.lastOperation?.id).toBe(queued.lastOperation?.id);
      expect(JSON.stringify(running)).not.toMatch(/fixture-private-recovery|actorId|recovery|scheduleRevision/);
    } finally {
      finish();
      await stopOperationRunner();
    }
    const completed = listSchedules(friend, serverId)[0];
    expect(completed.lastOperation?.status).toBe("succeeded");
    expect(completed.lastOperation?.result).toStrictEqual({ action: "start" });
    expect(completed.lastRunAt).toBe(queued.lastRunAt);
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
    expect(schedule.lastOperation?.status).toBe("failed");
    expect(schedule.lastOperation?.error).toBe("The fixture could not start.");
    expect(schedule.lastRunAt).toBe(due);
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
      expect(listSchedules(friend, serverId)[0].lastOperation?.phase).toBe("restarting");
    } finally {
      finish();
      await starting;
      await stopOperationRunner();
    }
    const schedule = listSchedules(friend, serverId)[0];
    expect(schedule.lastOperation?.status).toBe("interrupted");
    expect(schedule.lastOperation?.error ?? "").toMatch(/Ludock restarted/);
    expect(schedule.lastRunAt).toBe(due);
  });

  it("keeps a newer skipped attempt after an older queued operation finishes", () => {
    createSchedule(friend, serverId, input);
    runSchedules(due);
    const older = listSchedules(friend, serverId)[0].lastOperation!;
    runSchedules(due + 86_400_000);
    getDatabase().prepare("UPDATE operations SET status='succeeded',phase='succeeded' WHERE id=?").run(older.id);
    const latest = listSchedules(friend, serverId)[0];
    expect(latest.lastOperation).toBe(null);
    expect(latest.lastRunAt).toBe(due + 86_400_000);
    expect(latest.lastResult!).toMatch(/Skipped:/);
    runSchedules(due + 86_400_000 + 30_000);
    expect(listSchedules(friend, serverId)[0].lastRunAt).toBe(latest.lastRunAt);
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
      expect(current.lastOperation).toBe(null);
      expect(JSON.stringify(current)).not.toMatch(/fixture-secret/);
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
      expect(schedule.lastOperation).toBe(null);
      expect(schedule.lastRunAt).toBe(due);
    }
  });

  it("rolls back queued work if associating the attempt fails and still consumes the skipped slot", () => {
    const schedule = createSchedule(friend, serverId, input);
    const db = getDatabase();
    db.exec(`CREATE TRIGGER fail_operation_link BEFORE UPDATE ON schedules
      WHEN NEW.last_operation_id IS NOT NULL
      BEGIN SELECT RAISE(ABORT, 'fixture association failed'); END`);
    runSchedules(due);
    expect(serverOperations().length).toBe(0);
    expect(db.prepare("SELECT COUNT(*) AS count FROM audit_log WHERE action='server.start.queued'").get()?.count).toBe(0);
    const skipped = listSchedules(friend, serverId)[0];
    expect(skipped.id).toBe(schedule.id);
    expect(skipped.lastOperation).toBe(null);
    expect(skipped.lastRunAt).toBe(due);
    expect(skipped.lastResult!).toMatch(/Skipped/);
    db.exec("DROP TRIGGER fail_operation_link");
    runSchedules(due + 30_000);
    expect(serverOperations().length).toBe(0);
    expect(db.inTransaction).toBe(false);
  });

  it("prunes audit retention after committing the associated operation", () => {
    createSchedule(friend, serverId, input);
    const db = getDatabase();
    const warn = spyOn(console, "warn").mockImplementation(() => {});
    const cleanup = spyOn(database, "pruneAuditLogIfNeeded").mockImplementation(() => {
      expect(db.inTransaction).toBe(false);
      expect(listSchedules(friend, serverId)[0].lastOperation?.status).toBe("queued");
      throw new Error("fixture-private-cleanup-error");
    });
    runSchedules(due);
    expect(cleanup.mock.calls.length).toBe(1);
    expect(listSchedules(friend, serverId)[0].lastOperation?.status).toBe("queued");
    expect(JSON.stringify(warn.mock.calls)).not.toMatch(/fixture-private-cleanup-error/);
  });
});

describe("schedule preview availability reasons", () => {
  it("bounds repeated authority reads to distinct owners, actions, and binding baselines", () => {
    insertScheduleRows(1);
    const owners = spyOn(database, "findUserById");
    const bindings = spyOn(identity, "resolveServerBinding");
    expect(listSchedules(admin, serverId)[0].nextRunAt).toBeTruthy();
    const singleOwnerReads = owners.mock.calls.filter(([id]) => id === friend.id).length;
    const singleBindingReads = bindings.mock.calls.length;
    expect(singleOwnerReads > 0).toBeTruthy();
    expect(singleBindingReads > 0).toBeTruthy();

    insertScheduleRows(MAX_SCHEDULES_PER_SERVER - 1);
    owners.mockClear();
    bindings.mockClear();
    const schedules = listSchedules(admin, serverId);
    expect(schedules.length).toBe(MAX_SCHEDULES_PER_SERVER);
    expect(schedules.every((schedule) => schedule.nextRunAt !== null)).toBeTruthy();
    expect(owners.mock.calls.filter(([id]) => id === friend.id).length).toBe(singleOwnerReads);
    expect(bindings.mock.calls.length).toBe(singleBindingReads);
  });

  it("keeps different owners, actions, and binding baselines independent within a list", () => {
    const [available] = insertScheduleRows(1);
    const [revokedOwner] = insertScheduleRows(1, { ownerId: other.id });
    const [revokedAction] = insertScheduleRows(1, { data: { ...input, action: "stop" } });
    const [changedBinding] = insertScheduleRows(1);
    setServerGrant(other.id, serverId, ["server.view", "schedules.manage"], admin);
    getDatabase().prepare("UPDATE schedules SET binding_revision=999 WHERE id=?").run(changedBinding);

    const schedules = new Map(listSchedules(admin, serverId).map((schedule) => [schedule.id, schedule]));
    expect(schedules.get(available)!.nextRunAt).toBeTruthy();
    expect(schedules.get(available)!.nextRunUnavailableReason).toBe(null);
    expect(schedules.get(revokedOwner)!.nextRunUnavailableReason).toBe("action_access_removed");
    expect(schedules.get(revokedAction)!.nextRunUnavailableReason).toBe("action_access_removed");
    expect(schedules.get(changedBinding)!.nextRunUnavailableReason).toBe("binding_changed");
  });

  it("reuses unavailable outcomes only within the current list request", () => {
    insertScheduleRows(MAX_SCHEDULES_PER_SERVER);
    updateUserAccess(friend.id, "operator", true);
    const owners = spyOn(database, "findUserById");
    expect(listSchedules(admin, serverId)
      .every((schedule) => schedule.nextRunUnavailableReason === "owner_disabled")).toBeTruthy();
    expect(owners.mock.calls.filter(([id]) => id === friend.id).length).toBe(1);

    updateUserAccess(friend.id, "operator", false);
    expect(listSchedules(admin, serverId).every((schedule) => schedule.nextRunAt !== null)).toBeTruthy();
    setServerGrant(friend.id, serverId, ["server.view", "schedules.manage"], admin);
    expect(listSchedules(admin, serverId)
      .every((schedule) => schedule.nextRunUnavailableReason === "action_access_removed")).toBeTruthy();
  });

  it("distinguishes missing schedule-management grants, action grants, and disabled owners", () => {
    createSchedule(friend, serverId, input);
    setServerGrant(friend.id, serverId, ["server.view", "schedules.manage"], admin);
    expect(listSchedules(admin, serverId)[0].nextRunUnavailableReason).toBe("action_access_removed");
    setServerGrant(friend.id, serverId, ["server.view", "server.start"], admin);
    expect(listSchedules(admin, serverId)[0].nextRunUnavailableReason).toBe("owner_access_removed");
    updateUserAccess(friend.id, "viewer", false);
    expect(listSchedules(admin, serverId)[0].nextRunUnavailableReason).toBe("owner_access_removed");
    updateUserAccess(friend.id, "operator", true);
    expect(listSchedules(admin, serverId)[0].nextRunUnavailableReason).toBe("owner_disabled");
  });

  it("reports a missing owner without revealing private account details", () => {
    createSchedule(friend, serverId, input);
    const original = database.findUserById;
    spyOn(database, "findUserById").mockImplementation((id) => id === friend.id ? null : original(id));
    const schedule = listSchedules(admin, serverId)[0];
    expect(schedule.nextRunUnavailableReason).toBe("owner_missing");
    expect(schedule.nextRunAt).toBe(null);
  });

  it("distinguishes unavailable bindings from accepted material baseline changes", () => {
    createSchedule(friend, serverId, input);
    getDatabase().prepare("UPDATE logical_servers SET container_id=NULL WHERE id=?").run(serverId);
    expect(listSchedules(admin, serverId)[0].nextRunUnavailableReason).toBe("binding_unavailable");
    const pending = reconcileServers([{ ...observation, gameType: "factorio" }])[0];
    reviewServerBinding(serverId, pending.pendingFingerprint!);
    expect(listSchedules(admin, serverId)[0].nextRunUnavailableReason).toBe("binding_changed");
    getDatabase().prepare("UPDATE logical_servers SET container_id=NULL WHERE id=?").run(serverId);
    expect(listSchedules(admin, serverId)[0].nextRunUnavailableReason).toBe("binding_changed");
  });

  it("clears the unavailability reason while paused and hides unexpected diagnostics", () => {
    const schedule = createSchedule(friend, serverId, input);
    spyOn(identity, "resolveServerBinding").mockImplementation(() => {
      throw new Error("fixture-private-binding-detail");
    });
    const unavailable = listSchedules(admin, serverId)[0];
    expect(unavailable.nextRunUnavailableReason).toBe("unavailable");
    expect(JSON.stringify(unavailable)).not.toMatch(/fixture-private-binding-detail/);
    const paused = setScheduleEnabled(admin, serverId, schedule.id, { enabled: false, revision: 1 });
    expect(paused.nextRunAt).toBe(null);
    expect(paused.nextRunUnavailableReason).toBe(null);
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

      expect(() => runSchedules(due)).not.toThrow();

      const operations = serverOperations();
      expect(operations.length).toBe(1);
      expect(getOperation(operations[0].id)?.input.scheduleId).toBe(valid.id);
      const invalid = getDatabase().prepare(
        "SELECT last_slot,last_result FROM schedules WHERE id=?",
      ).get(invalidId) as { last_slot: string | null; last_result: string | null };
      expect(invalid.last_slot).toBe(null);
      expect(invalid.last_result!).toMatch(/Suspended: saved schedule configuration is invalid; recreate/);
      expect(warn.mock.calls.length).toBe(1);
      expect(JSON.stringify(warn.mock.calls)).not.toMatch(/fixture-schedule-private-detail/);
      expect(invalid.last_result!).not.toMatch(/fixture-schedule-private-detail/);

      runSchedules(due + 30_000);
      expect(serverOperations().length).toBe(1);
      expect(warn.mock.calls.length, "unchanged invalid rows must not repeat diagnostics").toBe(1);
    });
  }

  it("does not evaluate disabled schedules or consume their slots", () => {
    const warn = spyOn(console, "warn").mockImplementation(() => {});
    const disabled = createSchedule(friend, serverId, { ...input, enabled: false });
    const valid = createSchedule(friend, serverId, input);

    runSchedules(due);

    const operations = serverOperations();
    expect(operations.length).toBe(1);
    expect(getOperation(operations[0].id)?.input.scheduleId).toBe(valid.id);
    const state = getDatabase().prepare(
      "SELECT last_slot,last_result FROM schedules WHERE id=?",
    ).get(disabled.id) as { last_slot: string | null; last_result: string | null };
    expect(state.last_slot).toBe(null);
    expect(state.last_result).toBe(null);
    expect(warn.mock.calls.length).toBe(0);
  });

  it("continues when recording an invalid row's result fails", () => {
    const warn = spyOn(console, "warn").mockImplementation(() => {});
    insertScheduleRows(1, { inputJson: "not valid JSON" });
    const valid = createSchedule(friend, serverId, input);
    getDatabase().exec(`CREATE TRIGGER fail_invalid_schedule_result BEFORE UPDATE ON schedules
      WHEN OLD.input_json='not valid JSON'
      BEGIN SELECT RAISE(ABORT, 'fixture-schedule-private-detail'); END`);

    expect(() => runSchedules(due)).not.toThrow();

    const operations = serverOperations();
    expect(operations.length).toBe(1);
    expect(getOperation(operations[0].id)?.input.scheduleId).toBe(valid.id);
    expect(warn.mock.calls.length).toBe(1);
    expect(String(warn.mock.calls[0][0])).toMatch(/Could not record or notify/);
    expect(JSON.stringify(warn.mock.calls)).not.toMatch(/fixture-schedule-private-detail/);
  });
});

describe("schedule resource limits", () => {
  it("rejects schedule payloads above the endpoint-specific byte limit", () => {
    expect(thrownBy(() =>
        createSchedule(friend, serverId, {
          ...input,
          padding: "x".repeat(MAX_SCHEDULE_PAYLOAD_BYTES),
        }))).toSatisfy((error) =>
        error instanceof AppError &&
        error.code === "SCHEDULE_PAYLOAD_TOO_LARGE" &&
        error.statusCode === 413);
    expect(listSchedules(friend, serverId).length).toBe(0);
  });

  it("caps each server and every list response", () => {
    insertScheduleRows(MAX_SCHEDULES_PER_SERVER, { ownerId: admin.id });
    insertScheduleRows(1, { ownerId: friend.id });

    expect(listSchedules(admin, serverId).length).toBe(MAX_SCHEDULES_PER_SERVER);
    expect(listSchedules(friend, serverId).length).toBe(1);
    expect(thrownBy(() => createSchedule(admin, serverId, input))).toSatisfy((error) =>
        error instanceof AppError &&
        error.code === "SCHEDULE_LIMIT_REACHED" &&
        error.message.includes(String(MAX_SCHEDULES_PER_SERVER)));
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

    expect(thrownBy(() => createSchedule(admin, nextServer.id, input))).toSatisfy((error) =>
        error instanceof AppError &&
        error.code === "SCHEDULE_LIMIT_REACHED" &&
        error.message.includes(String(MAX_SCHEDULES_TOTAL)));
    expect(() => runSchedules(due)).not.toThrow();
    expect(warn.mock.calls.length, "exactly the supported cap is not overflow").toBe(0);
    const [overflowId] = insertScheduleRows(1, {
      ownerId: admin.id,
      inputJson: "not valid JSON",
    });
    expect(() => runSchedules(due)).not.toThrow();
    expect(warn.mock.calls.length).toBe(1);
    expect(String(warn.mock.calls[0][0])).toMatch(/Schedule limit exceeded/);
    const overflow = getDatabase().prepare(
      "SELECT last_slot,last_result FROM schedules WHERE id=?",
    ).get(overflowId) as { last_slot: string | null; last_result: string | null };
    expect(overflow.last_slot).toBe(null);
    expect(overflow.last_result, "rows beyond the cap must not be evaluated").toBe(null);
  });
});
