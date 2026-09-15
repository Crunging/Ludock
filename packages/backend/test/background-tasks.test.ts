import assert from "node:assert/strict";
import { it, mock, spyOn } from "bun:test";
import { startServer } from "../src/index.js";
import { closeDatabase, createUser, getDatabase } from "../src/database.js";
import { reconcileServers } from "../src/identity.js";
import { createSchedule } from "../src/schedules.js";
import * as servers from "../src/servers.js";
import * as notifications from "../src/notifications.js";

process.env.LUDOCK_DB_PATH = ":memory:";
process.env.LUDOCK_API_TOKEN = "background-fixture-token-0123456789";

it("evaluates due schedules during slow notifications and drains both tasks on shutdown", async () => {
  closeDatabase();
  let now = Date.parse("2026-09-15T07:59:00Z");
  spyOn(Date, "now").mockImplementation(() => now);
  const actor = { id: crypto.randomUUID(), username: "scheduler", role: "admin" as const };
  createUser({ ...actor, passwordHash: "fixture", disabled: false, createdAt: now });
  const server = reconcileServers([{
    containerId: "background-fixture", name: "world", displayName: "World", gameType: "minecraft", mounts: [],
  }])[0];
  const schedule = createSchedule(actor, server.id, {
    action: "start", enabled: true, time: "08:00", timezone: "UTC", days: [0, 1, 2, 3, 4, 5, 6],
  });
  const database = getDatabase();
  const discovery = spyOn(servers, "refreshServers").mockResolvedValue(new Map());
  let releaseDelivery!: () => void;
  const gate = new Promise<void>((resolve) => { releaseDelivery = resolve; });
  const delivery = spyOn(notifications, "deliverNotifications").mockImplementation(async () => {
    await gate;
    // Shutdown must leave storage open until delivery has finished recording its result.
    assert.equal(database.prepare("SELECT 1 AS value").get()?.value, 1);
  });
  let tick: (() => void) | undefined;
  const nativeInterval = globalThis.setInterval;
  spyOn(globalThis, "setInterval").mockImplementation(((callback: () => void, delay?: number) => {
    if (delay === 15_000) tick = callback;
    return nativeInterval(callback, delay);
  }) as typeof setInterval);
  const flush = async () => { for (let i = 0; i < 30; i++) await Promise.resolve(); };
  let runtime: ReturnType<typeof startServer> | undefined;
  try {
    runtime = startServer({ port: 0, hostname: "127.0.0.1", frontendDist: false });
    await flush();
    assert.ok(tick);
    assert.equal(delivery.mock.calls.length, 1);
    now += 60_000;
    tick();
    await flush();
    const row = database.prepare("SELECT last_slot,last_operation_id FROM schedules WHERE id=?")
      .get(schedule.id) as { last_slot: string | null; last_operation_id: string | null };
    assert.equal(row.last_slot, "2026-09-15T08:00:UTC");
    assert.ok(row.last_operation_id, "The due operation must queue while delivery is pending");
    assert.equal(delivery.mock.calls.length, 1, "Delivery batches must not overlap");

    let stopped = false;
    const shutdown = runtime.shutdown("test").then(() => { stopped = true; });
    await flush();
    assert.equal(stopped, false);
    const discoveries = discovery.mock.calls.length;
    tick();
    await flush();
    assert.equal(discovery.mock.calls.length, discoveries, "Shutdown prevents new discovery");
    assert.equal(delivery.mock.calls.length, 1);
    releaseDelivery();
    await shutdown;
    assert.equal(stopped, true);
    assert.throws(() => database.prepare("SELECT 1").get(), /closed/i);
  } finally {
    releaseDelivery();
    await runtime?.shutdown("test cleanup");
    mock.restore();
    closeDatabase();
  }
});
