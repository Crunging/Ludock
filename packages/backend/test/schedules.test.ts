import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it } from "node:test";
import type { ScheduleInput } from "@ludock/shared";
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
  scheduleSlot,
} from "../src/schedules.js";
import { setServerGrant } from "../src/authorization.js";
import { reconcileServers, reviewServerBinding } from "../src/identity.js";
import {
  listOperations,
  getOperation,
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
  it("does not run twice in a slot and does not catch up after a missed slot", () => {
    createSchedule(friend, serverId, input);
    runSchedules(due);
    runSchedules(due + 30_000);
    assert.equal(listOperations(serverId).length, 1);
    getDatabase().prepare("UPDATE operations SET status='succeeded'").run();
    runSchedules(due + 86400_000 + 60_000);
    assert.equal(listOperations(serverId).length, 1);
  });
  it("blocks revoked actions and records an actionable suspension", () => {
    createSchedule(friend, serverId, input);
    setServerGrant(
      friend.id,
      serverId,
      ["server.view", "schedules.manage"],
      admin,
    );
    runSchedules(due);
    assert.equal(listOperations(serverId).length, 0);
    assert.match(
      listSchedules(admin, serverId)[0].lastResult!,
      /Suspended: required access/,
    );
  });
  it("blocks disabled owners and removes schedules with deleted owners", () => {
    createSchedule(friend, serverId, input);
    updateUserAccess(friend.id, "operator", true);
    runSchedules(due);
    assert.equal(listOperations(serverId).length, 0);
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
    const operations = listOperations(serverId);
    assert.equal(operations.length, 1);
    assert.equal(
      getOperation(operations[0].id)?.bindingRevision,
      replacement.bindingRevision,
    );
  });
  it("keeps schedules suspended after material data changes until explicitly recreated", () => {
    createSchedule(friend, serverId, input);
    const pending = reconcileServers([
      { ...observation, gameType: "factorio" },
    ])[0];
    reviewServerBinding(serverId, pending.pendingFingerprint!);
    runSchedules(due);
    assert.equal(listOperations(serverId).length, 0);
    assert.match(
      listSchedules(admin, serverId)[0].lastResult!,
      /server configuration changed/,
    );
  });
});
