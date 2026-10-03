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
  stopOperationRunner,
} from "../src/operations.js";
import type { SQLQueryBindings } from "bun:sqlite";
import { dockerId } from "./fixtures/ids.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const admin: SessionUser = { id: "admin", username: "owner", role: "admin" };
const friend: SessionUser = {
  id: "friend",
  username: "friend",
  role: "operator",
};
const other: SessionUser = { id: "other", username: "other", role: "operator" };
const observation = {
  containerId: dockerId("original"),
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
    const audit = db.prepare<Record<string, unknown>, SQLQueryBindings[]>(
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

  for (const status of ["missing", "ambiguous", "review_required"] as const)
    it(`lets administrators inspect, pause, and delete schedules with a ${status} binding`, () => {
      const schedule = createSchedule(friend, serverId, input);
      reconcileServers(status === "missing" ? [] : status === "ambiguous"
        ? [observation, { ...observation, containerId: dockerId("duplicate") }]
        : [{ ...observation, gameType: "factorio" }]);
      expect(identity.getLogicalServer(serverId)?.status).toBe(status);
      expect(listSchedules(admin, serverId)[0].nextRunUnavailableReason).toBe("binding_unavailable");
      expect(() => listSchedules(friend, serverId)).toThrow(/not found/i);
      const paused = setScheduleEnabled(admin, serverId, schedule.id, { enabled: false, revision: 1 });
      expect(paused.enabled).toBe(false);
      expect(() => setScheduleEnabled(admin, serverId, schedule.id, {
        enabled: true, revision: paused.revision,
      })).toThrow(/permissions/);
      expect(() => updateSchedule(admin, serverId, schedule.id, {
        ...input, enabled: false, time: "09:00", revision: paused.revision,
      })).toThrow(/permissions/);
      deleteSchedule(admin, serverId, schedule.id);
      expect(listSchedules(admin, serverId)).toStrictEqual([]);
      reconcileServers([observation]);
      runSchedules(due);
      expect(serverOperations()).toStrictEqual([]);
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
    expect(getDatabase().prepare<Record<string, unknown>, SQLQueryBindings[]>("SELECT binding_revision FROM schedules WHERE id=?")
      .get(schedule.id)?.binding_revision).toBe(1);
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
});

describe("schedule outcomes", () => {
  it("never projects an unrelated or malformed operation reference", () => {
    const schedule = createSchedule(friend, serverId, input);
    runSchedules(due);
    const operation = listSchedules(friend, serverId)[0].lastOperation!;
    const another = reconcileServers([observation, { ...observation, name: "another", containerId: dockerId("another") }])
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

  it("rolls back queued work if associating the attempt fails and still consumes the skipped slot", () => {
    const schedule = createSchedule(friend, serverId, input);
    const db = getDatabase();
    db.exec(`CREATE TRIGGER fail_operation_link BEFORE UPDATE ON schedules
      WHEN NEW.last_operation_id IS NOT NULL
      BEGIN SELECT RAISE(ABORT, 'fixture association failed'); END`);
    runSchedules(due);
    expect(serverOperations().length).toBe(0);
    expect(db.prepare<Record<string, unknown>, SQLQueryBindings[]>("SELECT COUNT(*) AS count FROM audit_log WHERE action='server.start.queued'").get()?.count).toBe(0);
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
      const invalid = getDatabase().prepare<Record<string, unknown>, SQLQueryBindings[]>(
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
    const state = getDatabase().prepare<Record<string, unknown>, SQLQueryBindings[]>(
      "SELECT last_slot,last_result FROM schedules WHERE id=?",
    ).get(disabled.id) as { last_slot: string | null; last_result: string | null };
    expect(state.last_slot).toBe(null);
    expect(state.last_result).toBe(null);
    expect(warn.mock.calls.length).toBe(0);
  });
});
