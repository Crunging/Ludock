import { afterEach, beforeEach, describe, expect, it, jest, mock, spyOn } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scheduleSchema } from "@ludock/shared";
import { closeDatabase, createUser, getDatabase, type SessionUser } from "../src/database.js";
import { reconcileServers } from "../src/identity.js";
import {
  createSchedule, runScheduleAnnouncements, runSchedules, setScheduleEnabled, updateSchedule,
  waitForScheduleAnnouncements,
} from "../src/schedules.js";
import * as servers from "../src/servers.js";
import * as runtime from "../src/game-console-runtime.js";
import * as operations from "../src/operations.js";
import { acquireLocks, isServerBusy } from "../src/operation-locks.js";
import { getGameConsoleAdapterSummary, resolveBroadcastTemplate } from "../src/game-console.js";
import { dockerId } from "./fixtures/ids.js";
import { setServerGrant } from "./fixtures/grants.js";

const admin: SessionUser = { id: "owner", username: "owner", role: "admin" };
const friend: SessionUser = { id: "friend", username: "friend", role: "operator" };
const due = Date.parse("2026-09-09T08:00:00Z");
const input = {
  kind: "recurring", action: "restart", enabled: true, time: "08:00", days: [0, 1, 2, 3, 4, 5, 6], timezone: "UTC", announcements: {},
};
let serverId: string;
let now: number;
let context: servers.ServerContext;
let send: ReturnType<typeof spyOn<typeof runtime, "executeGameCommand">>;

async function until(condition: () => boolean) {
  for (let attempt = 0; attempt < 100 && !condition(); attempt++)
    await new Promise((resolve) => setTimeout(resolve, 2));
  expect(condition()).toBe(true);
}

async function announce() {
  runScheduleAnnouncements();
  await waitForScheduleAnnouncements();
}

function openFixtureDatabase(path: string) {
  closeDatabase();
  process.env.LUDOCK_DB_PATH = path;
  for (const user of [admin, friend]) createUser({ ...user, passwordHash: "fake", disabled: false, createdAt: 0 });
  const observation = { containerId: dockerId("countdown"), name: "world", displayName: "World", gameType: "minecraft", mounts: [] };
  const logical = reconcileServers([observation])[0];
  serverId = logical.id;
  setServerGrant(friend.id, serverId, ["server.view", "server.restart", "schedules.manage", "console.execute"], admin);
  context = {
    logical: { ...logical, containerId: observation.containerId }, observation,
    lockKeys: [`server:${serverId}`],
    container: {
      id: observation.containerId, shortId: "countdown", name: "world", displayName: "World", image: "itzg/minecraft-server",
      gameType: "minecraft", gameName: "Minecraft", state: "running", status: "Up", labels: {}, health: null,
      stateSince: null, exit: null, addressLabel: null, connectPort: null, gameConsole: null, fileRoots: [], ports: [], created: 0,
    },
  };
}

beforeEach(async () => {
  await operations.stopOperationRunner();
  openFixtureDatabase(":memory:");
  now = due - 10 * 60_000;
  spyOn(Date, "now").mockImplementation(() => now);
  spyOn(servers, "resolveAuthorizedServer").mockImplementation(async () => context);
  send = spyOn(runtime, "executeGameCommand").mockImplementation(async (_container, _server, _adapter, _command, output, assertAccess) => {
    expect(isServerBusy(serverId)).toBe(true);
    assertAccess?.();
    output.stdout("fixture-console-secret");
  });
});
afterEach(async () => {
  await waitForScheduleAnnouncements();
  mock.restore();
  await operations.stopOperationRunner();
  closeDatabase();
});

describe("restart countdown announcements", () => {
  it("accepts bounded, unique lead times for restarts only", () => {
    expect(scheduleSchema.parse(input).announcements?.leadMinutes).toEqual([10, 5, 1]);
    for (const leadMinutes of [[], [0], [61], [1.5], [1, 1], Array.from({ length: 11 }, (_, i) => i + 1)])
      expect(scheduleSchema.safeParse({ ...input, announcements: { leadMinutes } }).success).toBe(false);
    for (const action of ["start", "stop", "backup"])
      expect(scheduleSchema.safeParse({ ...input, action }).success).toBe(false);
    expect(scheduleSchema.safeParse({ ...input, announcements: { command: "op friend" } }).success).toBe(false);
  });

  it("uses preset broadcast templates only with the preset's console adapter", () => {
    expect(resolveBroadcastTemplate({ gameType: "minecraft", labels: {} })).toBe("say {message}");
    expect(getGameConsoleAdapterSummary({ gameType: "minecraft", labels: {} })?.supportsBroadcast).toBe(true);
    for (const [gameType, labels] of [
      ["unknown", { "ludock.console": "minecraft-rcon" }], ["valheim", {}],
      ["minecraft", { "ludock.console": "none" }], ["minecraft", { "ludock.console": "stdin-console" }],
    ] as const)
      expect(resolveBroadcastTemplate({ gameType, labels })).toBeNull();
  });

  it("requires console access to save warnings", () => {
    setServerGrant(friend.id, serverId, ["server.view", "server.restart", "schedules.manage"], admin);
    expect(() => createSchedule(friend, serverId, input)).toThrow(/permission/i);
    const schedule = createSchedule(friend, serverId, { ...input, announcements: undefined });
    expect(() => updateSchedule(friend, serverId, schedule.id, { ...input, revision: 1 })).toThrow(/permission/i);
  });

  it("sends each warning once, in its minute, without catching up or persisting output", async () => {
    createSchedule(friend, serverId, input);
    await announce();
    now += 30_000;
    await announce();
    now = due - 7 * 60_000;
    await announce();
    // Ludock was down during the five-minute warning.
    now = due - 60_000;
    await announce();
    expect(send.mock.calls.map((call) => call[3])).toEqual([
      "say Server restart in 10 minutes.", "say Server restart in 1 minute.",
    ]);
    expect(JSON.stringify(getDatabase().query("SELECT * FROM schedules").all())).not.toContain("fixture-console-secret");
    now = due;
    runSchedules();
    expect(getDatabase().query("SELECT last_result FROM schedules").get()).toEqual({ last_result: expect.stringMatching(/^Queued/) });
  });

  it("warns before a one-time restart with a backup, and never after it completes", async () => {
    setServerGrant(friend.id, serverId,
      ["server.view", "server.restart", "schedules.manage", "console.execute", "backups.create"], admin);
    const { days: _days, ...base } = input;
    createSchedule(friend, serverId, { ...base, kind: "once", date: "2026-09-09", createBackup: true });
    await announce();
    now = due;
    runSchedules();
    const { last_operation_id: id } = getDatabase().query("SELECT last_operation_id FROM schedules").get() as
      { last_operation_id: string };
    expect(operations.getOperation(id)?.input).toMatchObject({ createBackup: true });
    // A clock moved back after the run must not announce the completed schedule.
    now = due - 5 * 60_000;
    await announce();
    expect(send.mock.calls.map((call) => call[3])).toEqual(["say Server restart in 10 minutes."]);
  });

  it("sends each warning of a slot once across a restart into the repeated hour", async () => {
    const directory = await mkdtemp(join(tmpdir(), "ludock-countdown-"));
    try {
      openFixtureDatabase(join(directory, "ludock.sqlite"));
      createSchedule(friend, serverId, { ...input, time: "01:30", timezone: "America/Los_Angeles" });
      const daylight = Date.parse("2026-11-01T08:30:00Z");
      for (const lead of [10, 5, 1]) {
        now = daylight - lead * 60_000;
        await announce();
      }
      // Ludock restarts, misses the first 01:30, and is back before the second.
      closeDatabase();
      for (const lead of [10, 5, 1]) {
        now = daylight + 60 * 60_000 - lead * 60_000;
        await announce();
      }
      expect(send).toHaveBeenCalledTimes(3);
    } finally {
      closeDatabase();
      await rm(directory, { recursive: true, force: true });
    }
  });

  it("does not resend a claimed warning after a failure or an edit", async () => {
    const schedule = createSchedule(friend, serverId, input);
    send.mockRejectedValueOnce(new Error("fixture-password"));
    await announce();
    await announce();
    updateSchedule(friend, serverId, schedule.id, { ...input, announcements: { leadMinutes: [10, 2] }, revision: 1 });
    await announce();
    expect(send).toHaveBeenCalledTimes(1);
  });

  it.each(["paused", "revoked console", "revoked action", "binding changed", "unsupported console"])(
    "skips warnings for %s schedules", async (reason) => {
      const schedule = createSchedule(friend, serverId, input);
      if (reason === "paused") setScheduleEnabled(friend, serverId, schedule.id, { revision: 1, enabled: false });
      if (reason === "revoked console" || reason === "revoked action") setServerGrant(friend.id, serverId,
        ["server.view", "schedules.manage", reason === "revoked console" ? "server.restart" : "console.execute"], admin);
      if (reason === "binding changed")
        getDatabase().query("UPDATE logical_servers SET binding_fingerprint='changed' WHERE id=?").run(serverId);
      if (reason === "unsupported console") context.container.labels = { "ludock.console": "none" };
      await announce();
      expect(send).not.toHaveBeenCalled();
    });

  it("rechecks the schedule while the command runs", async () => {
    const schedule = createSchedule(friend, serverId, input);
    send.mockImplementationOnce(async (_container, _server, _adapter, _command, _output, assertAccess) => {
      setScheduleEnabled(friend, serverId, schedule.id, { revision: 1, enabled: false });
      expect(() => assertAccess?.()).toThrow(/no longer current/);
    });
    await announce();
    expect(isServerBusy(serverId)).toBe(false);
  });

  it("skips a warning when the server is busy", async () => {
    createSchedule(friend, serverId, input);
    const release = acquireLocks(context.lockKeys);
    try { await announce(); } finally { release(); }
    await announce();
    expect(send).not.toHaveBeenCalled();
  });

  it("keeps a due restart queued until the warning releases its locks", async () => {
    const command = Promise.withResolvers<void>();
    let effects = 0;
    createSchedule(admin, serverId, { ...input, announcements: { leadMinutes: [1] } });
    send.mockImplementation(() => command.promise);
    operations.registerJobHandler("restart", {
      run: (job) => operations.withJobLocks(job, context.lockKeys, () => { effects++; }),
    });
    now = due - 60_000;
    runScheduleAnnouncements();
    await until(() => send.mock.calls.length === 1);
    now = due;
    runSchedules();
    const { last_operation_id: id } = getDatabase().query("SELECT last_operation_id FROM schedules").get() as
      { last_operation_id: string };
    await operations.startOperationRunner();
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(operations.getOperation(id)?.status).toBe("queued");
    command.resolve();
    await waitForScheduleAnnouncements();
    await until(() => operations.getOperation(id)?.status === "succeeded");
    expect(effects).toBe(1);
  });

  it("skips a warning without time to finish before its restart", async () => {
    createSchedule(friend, serverId, { ...input, announcements: { leadMinutes: [1] } });
    now = due - 29_999;
    await announce();
    expect(send).not.toHaveBeenCalled();
  });

  it("cancels a stalled warning before its restart", async () => {
    createSchedule(friend, serverId, { ...input, announcements: { leadMinutes: [1] } });
    let signal: AbortSignal | undefined;
    send.mockImplementation(async (...args) => {
      signal = args[6];
      await new Promise((resolve) => signal?.addEventListener("abort", resolve, { once: true }));
    });
    now = due - 60_000;
    jest.useFakeTimers();
    try {
      runScheduleAnnouncements();
      for (let tick = 0; tick < 20 && !signal; tick++) await Promise.resolve();
      jest.advanceTimersByTime(44_999);
      expect(signal?.aborted).toBe(false);
      jest.advanceTimersByTime(1);
      expect(signal?.aborted).toBe(true);
    } finally {
      jest.useRealTimers();
    }
    await waitForScheduleAnnouncements();
    expect(isServerBusy(serverId)).toBe(false);
  });

  it("cancels a warning for a job and keeps other jobs running during its cleanup", async () => {
    const cleanup = Promise.withResolvers<void>();
    let signal: AbortSignal | undefined;
    const effects: string[] = [];
    createSchedule(admin, serverId, { ...input, announcements: { leadMinutes: [5] } });
    send.mockImplementation(async (...args) => {
      signal = args[6];
      await new Promise((resolve) => signal?.addEventListener("abort", resolve, { once: true }));
      await cleanup.promise;
    });
    for (const [kind, keys] of [["restart", context.lockKeys], ["start", ["server:unrelated"]]] as const)
      operations.registerJobHandler(kind, {
        run: (job) => operations.withJobLocks(job, keys, () => { effects.push(kind); }),
      });
    now = due - 5 * 60_000;
    runScheduleAnnouncements();
    await until(() => send.mock.calls.length === 1);
    const restart = operations.enqueueOperation({ serverId, actorId: admin.id, kind: "restart", bindingRevision: 1 });
    await operations.startOperationRunner();
    await until(() => signal?.aborted === true);
    const other = reconcileServers([context.observation, { ...context.observation, containerId: dockerId("unrelated"), name: "other", displayName: "Other" }])
      .find((server) => server.id !== serverId)!;
    const unrelated = operations.enqueueOperation({ serverId: other.id, actorId: admin.id, kind: "start", bindingRevision: 1 });
    await until(() => operations.getOperation(unrelated.id)?.status === "succeeded");
    expect(operations.getOperation(restart.id)?.status).toBe("queued");
    cleanup.resolve();
    await waitForScheduleAnnouncements();
    await until(() => operations.getOperation(restart.id)?.status === "succeeded");
    expect(effects).toEqual(["start", "restart"]);
  });
});
