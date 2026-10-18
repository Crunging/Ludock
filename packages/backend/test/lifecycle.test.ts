import { afterEach, beforeEach, describe, expect, it, spyOn } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { operationResponseSchema } from "@ludock/shared";
import { createApp } from "../src/app.js";
import { createSession } from "../src/auth.js";
import { listAttention } from "../src/attention.js";
import { closeDatabase, createUser, getDatabase, type SessionUser } from "../src/database.js";
import { docker } from "../src/docker-client.js";
import { listLogicalServers, reviewServerBinding } from "../src/identity.js";
import { registerBackgroundJobs } from "../src/jobs.js";
import { checkAvailability, configureAvailability, getAvailability, getAvailabilityProblem } from "../src/monitoring.js";
import { isServerBusy } from "../src/operation-locks.js";
import { enqueueOperation, getOperation, startOperationRunner, stopOperationRunner } from "../src/operations.js";
import { listServers, refreshServers } from "../src/servers.js";
import { createSchedule, runSchedules } from "../src/schedules.js";
import { setServerGrant } from "./fixtures/grants.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const admin: SessionUser = { id: "owner", username: "owner", role: "admin" };
const friend: SessionUser = { id: "friend", username: "friend", role: "operator" };
const originals = { getContainer: docker.getContainer, listContainers: docker.listContainers };
const realTimeout = globalThis.setTimeout;
const clock = spyOn(globalThis, "setTimeout");
let serverId: string;
let name: string;
let state: string;
let startedAt: string;
let exitCode: number;
let labels: Record<string, string>;
let commands: string[];
let mutations: string[];
let onSave: (() => void) | undefined;
let stopGate: Promise<void> | undefined;
let attachGate: Promise<void> | undefined;
let stopExitCode: number;
let streamsClosed: number;

function queue(kind: "stop" | "restart" = "stop") {
  return enqueueOperation({ serverId, actorId: friend.id, kind, bindingRevision: 1 });
}
function interrupted(kind: "stop" | "restart", recovery: Record<string, unknown> = {}) {
  const operation = queue(kind);
  getDatabase().query("UPDATE operations SET status='running',phase='stopping',recovery_json=? WHERE id=?")
    .run(JSON.stringify({
      containerId: "fixture", stopStarted: true,
      stopInitialStartedAt: Date.parse(startedAt), stopRecoveryDeadlineAt: Date.now() + 300_000,
      ...recovery,
    }), operation.id);
  return operation;
}
function pauseRecoveryPolls() {
  const polls: Array<() => void> = [];
  clock.mockImplementation(((callback: Parameters<typeof setTimeout>[0], delay?: number, ...args: unknown[]) => {
    if (delay === 1000) {
      polls.push(() => callback(...args));
      return realTimeout(() => {}, 1);
    }
    return realTimeout(callback, delay, ...args);
  }) as typeof setTimeout);
  return {
    pending: () => until(() => polls.length > 0),
    resume: async () => { await until(() => polls.length > 0); polls.shift()!(); },
  };
}
async function until(check: () => boolean) {
  for (let attempt = 0; attempt < 200 && !check(); attempt++)
    await new Promise((resolve) => realTimeout(resolve, 5));
  expect(check()).toBe(true);
}
async function finished(id: string) {
  await until(() => !["queued", "running"].includes(getOperation(id)!.status));
  return getOperation(id)!;
}
beforeEach(async () => {
  await stopOperationRunner();
  closeDatabase();
  clock.mockImplementation(((callback: Parameters<typeof setTimeout>[0], delay?: number, ...args: unknown[]) =>
    realTimeout(callback, delay === 5000 ? 1 : delay === 10000 ? 20 : delay, ...args)) as typeof setTimeout);
  for (const actor of [admin, friend]) createUser({ ...actor, passwordHash: "fake", disabled: false, createdAt: 0 });
  name = "world";
  state = "running";
  startedAt = "2026-01-01T00:00:00Z";
  exitCode = 0;
  stopExitCode = 0;
  commands = [];
  mutations = [];
  labels = {};
  streamsClosed = 0;
  onSave = undefined;
  stopGate = undefined;
  attachGate = undefined;
  docker.listContainers = (async () => [{ Id: "fixture", Names: [`/${name}`], Image: "beardedio/terraria", Labels: labels }]) as typeof docker.listContainers;
  docker.getContainer = ((id: string) => {
    expect(id).toBe("fixture"); // A logical URL UUID never reaches Docker.
    return {
      inspect: async () => ({
        Id: id, Name: `/${name}`, Image: "image-id", Created: "2026-01-01T00:00:00Z",
        Config: { Image: "beardedio/terraria", Labels: labels, OpenStdin: true, StdinOnce: false, Tty: false },
        State: { Status: state, StartedAt: startedAt, Running: state === "running", ExitCode: exitCode, OOMKilled: false },
        Mounts: [], NetworkSettings: { Ports: {} },
      }),
      attach: async () => {
        await attachGate;
        return {
          writable: new WritableStream<Uint8Array>({ write(data) {
            commands.push(new TextDecoder().decode(data));
            const operation = getDatabase().query<{ phase: string }, []>("SELECT phase FROM operations WHERE status='running'").get();
            expect(operation?.phase).toBe("saving_world");
            expect(isServerBusy(serverId)).toBe(true);
            onSave?.();
          } }),
          abort() { streamsClosed++; },
        };
      },
      stop: async (options: { t: number }) => {
        expect(options).toEqual({ t: 120 });
        expect(isServerBusy(serverId)).toBe(true);
        const operation = getDatabase().query<{ phase: string; recovery_json: string }, []>("SELECT phase,recovery_json FROM operations WHERE status='running'").get();
        expect(operation?.phase).toBe("stopping");
        expect(JSON.parse(operation!.recovery_json).containerId).toBe("fixture");
        expect(JSON.parse(operation!.recovery_json).stopRecoveryDeadlineAt).toBeGreaterThan(Date.now());
        expect(JSON.parse(operation!.recovery_json).stopInitialStartedAt).toBe(state === "running" ? Date.parse(startedAt) : null);
        mutations.push("stop");
        await stopGate;
        state = "exited";
        exitCode = stopExitCode;
      },
      start: async () => { mutations.push("start"); state = "running"; startedAt = new Date().toISOString(); exitCode = 0; },
    };
  }) as unknown as typeof docker.getContainer;
  await refreshServers();
  serverId = listLogicalServers()[0].id;
  setServerGrant(friend.id, serverId, ["server.view", "server.stop", "server.restart"], admin);
  registerBackgroundJobs();
});
afterEach(async () => {
  await stopOperationRunner();
  clock.mockReset();
  clock.mockImplementation(realTimeout);
  Object.assign(docker, originals);
  closeDatabase();
});

describe("graceful lifecycle operations", () => {
  it("waits briefly after sending save before issuing Docker stop", async () => {
    let resumeSave: (() => void) | undefined;
    clock.mockImplementation(((callback: Parameters<typeof setTimeout>[0], delay?: number, ...args: unknown[]) => {
      if (delay === 5000) {
        resumeSave = () => callback(...args);
        return realTimeout(() => {}, 1);
      }
      return realTimeout(callback, delay, ...args);
    }) as typeof setTimeout);
    const operation = queue();
    await startOperationRunner();
    await until(() => Boolean(resumeSave));
    expect(getOperation(operation.id)?.phase).toBe("waiting_for_save");
    expect(commands).toEqual(["save\n"]);
    expect(mutations).toEqual([]);
    resumeSave!();
    expect((await finished(operation.id)).status).toBe("succeeded");
  });
  it("uses the same graceful stop and save steps for a scheduled restart", async () => {
    createSchedule(admin, serverId, {
      kind: "recurring", action: "restart", enabled: true, time: "08:00", days: [0, 1, 2, 3, 4, 5, 6], timezone: "UTC",
    });
    runSchedules(Date.parse("2026-09-09T08:00:00Z"));
    const saved = getDatabase().query<{ id: string }, []>("SELECT id FROM operations").get()!;
    expect(getOperation(saved.id)?.status).toBe("queued");
    await startOperationRunner();
    expect((await finished(saved.id)).result).toMatchObject({ action: "restart", saveStatus: "sent" });
    expect(commands).toEqual(["save\n"]);
    expect(mutations).toEqual(["stop", "start"]);
  });
  for (const action of ["stop", "restart"] as const) {
    it(`persists manual ${action} before returning 202 and runs independently of HTTP`, async () => {
      const url = `http://localhost/api/v1/servers/${serverId}/${action}`;
      const session = createSession(friend, new Request(url));
      const response = await createApp({ frontendDist: false }).fetch(new Request(url, {
        method: "POST", headers: { Cookie: `ludock_session=${session.token}` },
      }), { timeout() {}, requestIP: () => null });
      expect(response.status).toBe(202);
      const { operation } = operationResponseSchema.parse(await response.json());
      expect(getOperation(operation.id)?.status).toBe("queued");
      expect(mutations).toEqual([]);
      expect((await listServers(friend))[0].activeOperation).toMatchObject({ id: operation.id, kind: action, status: "queued" });
      await startOperationRunner();
      const result = await finished(operation.id);
      expect(result.status).toBe("succeeded");
      expect(result.result).toMatchObject({ saveStatus: "sent" });
      expect(commands).toEqual(["save\n"]);
      expect(mutations).toEqual(action === "stop" ? ["stop"] : ["stop", "start"]);
      expect(getAvailability(serverId).state.intentionallyStopped).toBe(action === "stop");
      expect(isServerBusy(serverId)).toBe(false);
    });
  }
  it("records an unavailable console and proceeds to stop", async () => {
    labels["ludock.console"] = "disabled";
    await refreshServers();
    // Console settings change the binding, so queue its current revision.
    let logical = listLogicalServers()[0];
    reviewServerBinding(logical.id, logical.pendingFingerprint!);
    logical = listLogicalServers()[0];
    serverId = logical.id;
    const operation = enqueueOperation({ serverId, actorId: admin.id, kind: "stop", bindingRevision: logical.bindingRevision });
    await startOperationRunner();
    expect((await finished(operation.id)).result).toMatchObject({ saveStatus: "skipped", saveReason: "console_unavailable" });
    expect(commands).toEqual([]);
    expect(mutations).toEqual(["stop"]);
  });
  it("times out an unavailable stdin console and never writes after a late attach", async () => {
    let release!: () => void;
    attachGate = new Promise((resolve) => { release = resolve; });
    const operation = queue();
    await startOperationRunner();
    expect((await finished(operation.id)).result).toMatchObject({ saveStatus: "skipped", saveReason: "timed_out" });
    expect(mutations).toEqual(["stop"]);
    release();
    await until(() => streamsClosed > 0);
    expect(commands).toEqual([]);
  });
  for (const change of ["permission", "binding"] as const) {
    it(`rechecks ${change} after saving and does not stop`, async () => {
      onSave = () => {
        if (change === "permission") setServerGrant(friend.id, serverId, ["server.view"], admin);
        else name = "different-world";
      };
      const operation = queue();
      await startOperationRunner();
      expect((await finished(operation.id)).status).toBe("failed");
      expect(mutations).toEqual([]);
    });
  }
  it("retains locks throughout Docker stop and rechecks restart permission before starting", async () => {
    let release!: () => void;
    stopGate = new Promise((resolve) => { release = resolve; });
    configureAvailability(serverId, { enabled: true, graceSeconds: 10 });
    const operation = queue("restart");
    await startOperationRunner();
    try {
      await until(() => mutations.includes("stop"));
      expect(isServerBusy(serverId)).toBe(true);
      expect(getAvailabilityProblem(serverId, Date.now() + 600_000)).toBeNull();
      expect(() => queue()).toThrow(/already queued or running/);
      setServerGrant(friend.id, serverId, ["server.view"], admin);
    } finally { release(); }
    expect((await finished(operation.id)).status).toBe("failed");
    expect(mutations).toEqual(["stop"]);
    expect(isServerBusy(serverId)).toBe(false);
  });
  for (const action of ["stop", "restart"] as const) {
    it(`keeps exit 137 from ${action} in Needs attention, even after starting`, async () => {
      stopExitCode = 137;
      const operation = queue(action);
      await startOperationRunner();
      expect((await finished(operation.id)).result).toMatchObject({ forceStopped: true });
      const items = (await listAttention(friend)).items;
      expect(items).toContainEqual(expect.objectContaining({ kind: "operation", operationId: operation.id, forceStopped: true, status: "succeeded" }));
      if (action === "restart") expect(exitCode).toBe(0);
    });
  }
  it("preserves a force-stop warning if the following restart start fails", async () => {
    stopExitCode = 137;
    const getContainer = docker.getContainer;
    docker.getContainer = ((id: string) => {
      const container = getContainer(id);
      container.start = async () => { throw new Error("private-fixture-start-error"); };
      return container;
    }) as typeof docker.getContainer;
    const operation = queue("restart");
    await startOperationRunner();
    const result = await finished(operation.id);
    expect(result).toMatchObject({ status: "failed", result: { forceStopped: true } });
    expect(JSON.stringify(result.result)).not.toContain("private-fixture-start-error");
    expect((await listAttention(friend)).items).toContainEqual(expect.objectContaining({ forceStopped: true, status: "failed" }));
  });
  it("does not report clean stops or unrelated previous exit codes as force-stopped", async () => {
    state = "exited";
    exitCode = 137;
    const operation = queue();
    await startOperationRunner();
    expect((await finished(operation.id)).result?.forceStopped).toBeUndefined();
    expect((await listAttention(friend)).items).toEqual([]);
  });
  for (const action of ["stop", "restart"] as const) {
    it(`waits for an in-flight ${action} to exit before releasing recovery locks`, async () => {
      configureAvailability(serverId, { enabled: true, graceSeconds: 10 });
      const operation = interrupted(action);
      const polls = pauseRecoveryPolls();
      let settled = false;
      const starting = startOperationRunner().finally(() => { settled = true; });
      try {
        await polls.pending();
        expect(settled).toBe(false);
        expect(getOperation(operation.id)).toMatchObject({ status: "running", phase: "reconciling_stop" });
        expect(isServerBusy(serverId)).toBe(true);
        expect(getAvailabilityProblem(serverId, Date.now() + 600_000)).toBeNull();
        expect(() => queue()).toThrow(/already queued or running/);
        expect(mutations).toEqual([]);
        state = "exited";
        exitCode = 137;
        await polls.resume();
        await starting;
        expect(getOperation(operation.id)).toMatchObject({ status: "interrupted", result: { forceStopped: true } });
        expect(getAvailability(serverId).state.intentionallyStopped).toBe(action === "stop");
        expect(isServerBusy(serverId)).toBe(false);
        expect(mutations).toEqual([]);
        expect(commands).toEqual([]);
        expect((await listAttention(friend)).items).toContainEqual(expect.objectContaining({ forceStopped: true }));
        if (action === "stop") {
          const now = Date.now() + 600_000;
          checkAvailability(await refreshServers(), now);
          expect(getAvailabilityProblem(serverId, now + 20_000)).toBeNull();
        }
      } finally {
        if (!settled) {
          state = "exited";
          await polls.resume();
          await starting;
        }
      }
    });
  }
  it("bounds an unconfirmed stop by its persisted deadline without extending it on recovery", async () => {
    const deadline = Date.now() - 1;
    const operation = interrupted("stop", { stopRecoveryDeadlineAt: deadline });
    await startOperationRunner();
    expect(getOperation(operation.id)).toMatchObject({
      status: "interrupted", recovery: { stopRecoveryDeadlineAt: deadline },
      result: { stopRecoveryTimedOut: true, guidance: expect.stringContaining("Review the server state") },
    });
    expect(state).toBe("running");
    expect(isServerBusy(serverId)).toBe(false);
    expect(mutations).toEqual([]);
    expect(commands).toEqual([]);
  });
  it("releases recovery locks with explicit guidance when a pending stop exceeds its deadline", async () => {
    const now = Date.now();
    const operation = interrupted("stop", { stopRecoveryDeadlineAt: now + 300_000 });
    const polls = pauseRecoveryPolls();
    let settled = false;
    const starting = startOperationRunner().finally(() => { settled = true; });
    const time = spyOn(Date, "now");
    try {
      await polls.pending();
      expect(isServerBusy(serverId)).toBe(true);
      time.mockReturnValue(now + 300_001);
      await polls.resume();
      await starting;
      expect(getOperation(operation.id)).toMatchObject({ status: "interrupted", result: { stopRecoveryTimedOut: true } });
      expect(isServerBusy(serverId)).toBe(false);
      expect(state).toBe("running");
      expect(mutations).toEqual([]);
    } finally {
      time.mockRestore();
      if (!settled) {
        state = "exited";
        await polls.resume();
        await starting;
      }
    }
  });
  it("does not wait on the running server after the recorded restart stop already settled", async () => {
    const operation = interrupted("restart", { stopSettled: true, forceStopped: true, stopRecoveryDeadlineAt: 0 });
    await startOperationRunner();
    expect(getOperation(operation.id)).toMatchObject({ status: "interrupted", result: { forceStopped: true } });
    expect(getOperation(operation.id)?.result?.stopRecoveryTimedOut).toBeUndefined();
    expect(mutations).toEqual([]);
  });
  it("recognizes an externally restarted container without waiting on its new running instance", async () => {
    const operation = interrupted("restart", { stopRecoveryDeadlineAt: 0 });
    startedAt = "2026-01-02T00:00:00Z";
    await startOperationRunner();
    expect(getOperation(operation.id)?.status).toBe("interrupted");
    expect(getOperation(operation.id)?.result?.stopRecoveryTimedOut).toBeUndefined();
    expect(mutations).toEqual([]);
  });
  for (const change of ["permission", "binding"] as const) {
    it(`rechecks ${change} while reconciling an in-flight stop`, async () => {
      const operation = interrupted("stop");
      const polls = pauseRecoveryPolls();
      const starting = startOperationRunner();
      await polls.pending();
      if (change === "permission") setServerGrant(friend.id, serverId, ["server.view"], admin);
      else name = "different-world";
      await polls.resume();
      await starting;
      expect(getOperation(operation.id)?.status).toBe("interrupted");
      expect(isServerBusy(serverId)).toBe(false);
      expect(mutations).toEqual([]);
    });
  }
  it("retains recovery locks across a transient daemon failure and observes the eventual exit", async () => {
    const operation = interrupted("stop");
    const polls = pauseRecoveryPolls();
    let settled = false;
    const starting = startOperationRunner().finally(() => { settled = true; });
    try {
      await polls.pending();
      const listContainers = docker.listContainers;
      let failed = false;
      docker.listContainers = (async () => {
        if (!failed) { failed = true; throw new Error("private daemon failure"); }
        return listContainers();
      }) as typeof docker.listContainers;
      await polls.resume();
      await polls.pending();
      expect(failed).toBe(true);
      expect(getOperation(operation.id)?.status).toBe("running");
      expect(isServerBusy(serverId)).toBe(true);
      state = "exited";
      exitCode = 137;
      await polls.resume();
      await starting;
      expect(getOperation(operation.id)).toMatchObject({ status: "interrupted", result: { forceStopped: true } });
      expect(JSON.stringify(getOperation(operation.id)?.result)).not.toContain("private daemon failure");
      expect(mutations).toEqual([]);
    } finally {
      if (!settled) {
        state = "exited";
        await polls.resume();
        await starting;
      }
    }
  });
  it("reopens persisted lifecycle state from disk and reconciles it without replay", async () => {
    const directory = await mkdtemp(join(tmpdir(), "ludock-lifecycle-"));
    closeDatabase();
    process.env.LUDOCK_DB_PATH = join(directory, "ludock.sqlite");
    try {
      for (const actor of [admin, friend]) createUser({ ...actor, passwordHash: "fake", disabled: false, createdAt: 0 });
      await refreshServers();
      serverId = listLogicalServers()[0].id;
      setServerGrant(friend.id, serverId, ["server.view", "server.restart"], admin);
      const operation = queue("restart");
      getDatabase().query("UPDATE operations SET status='running',phase='stopping',recovery_json=? WHERE id=?")
        .run(JSON.stringify({ containerId: "fixture", stopStarted: true }), operation.id);
      closeDatabase();
      state = "exited";
      exitCode = 137;
      expect(getOperation(operation.id)?.phase).toBe("stopping");
      await startOperationRunner();
      expect(getOperation(operation.id)).toMatchObject({ status: "interrupted", result: { forceStopped: true } });
      expect(mutations).toEqual([]);
    } finally {
      await stopOperationRunner();
      closeDatabase();
      process.env.LUDOCK_DB_PATH = ":memory:";
      await rm(directory, { recursive: true, force: true });
    }
  });
  for (const action of ["stop", "restart"] as const) {
    it(`reconciles interrupted ${action}, records exit 137, and never replays mutations`, async () => {
      const operation = queue(action);
      state = "exited";
      exitCode = 137;
      getDatabase().query("UPDATE operations SET status='running',phase='stopping',recovery_json=? WHERE id=?")
        .run(JSON.stringify({ containerId: "fixture", stopStarted: true }), operation.id);
      await startOperationRunner();
      expect(getOperation(operation.id)).toMatchObject({ status: "interrupted", result: { forceStopped: true } });
      expect(mutations).toEqual([]);
      expect(getAvailability(serverId).state.intentionallyStopped).toBe(action === "stop");
      await stopOperationRunner();
      await startOperationRunner();
      expect(mutations).toEqual([]);
      expect((await listAttention(friend)).items).toContainEqual(expect.objectContaining({ forceStopped: true }));
    });
  }
});
