import { expect, afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";
import { recoverBackup, recoverRestore, stopForDataOperation } from "../src/backups.js";
import * as backupStorage from "../src/backup-storage.js";
import { listLogicalServers } from "../src/identity.js";
import { setServerGrant } from "./fixtures/grants.js";
import {
  closeDatabase,
  createUser,
  updateUserAccess,
} from "../src/database.js";
import { docker } from "../src/docker-client.js";
import type { JobContext } from "../src/operations.js";
import { createSchedule, setScheduleEnabled } from "../src/schedules.js";
import {
  refreshServers,
  resolveAuthorizedServer,
  type ServerContext,
} from "../src/servers.js";

describe("backup execution authority", () => {
  const admin = { id: "admin", username: "admin", role: "admin" as const };
  const operator = {
    id: "operator", username: "operator", role: "operator" as const,
  };
  const originalList = docker.listContainers;
  const originalGet = docker.getContainer;
  let context: ServerContext;
  let job: JobContext;
  let stopped: number;
  let running: boolean;
  let onInspect: () => void;
  beforeEach(async () => {
    process.env.LUDOCK_DB_PATH = ":memory:";
    closeDatabase();
    stopped = 0;
    running = true;
    onInspect = () => {};
    for (const actor of [admin, operator])
      createUser({ ...actor, passwordHash: "fake", disabled: false, createdAt: 0 });
    const labels = { "ludock.enable": "true" };
    docker.listContainers = (async () => [{
      Id: "fixture", Names: ["/fixture"], Image: "alpine:latest",
      State: "running", Status: "Up", Ports: [], Created: 0, Labels: labels,
    }]) as unknown as typeof docker.listContainers;
    docker.getContainer = ((id: string) => ({
      inspect: async () => {
        onInspect();
        return {
          Id: id, Name: "/fixture",
          Config: { Image: "alpine:latest", Labels: labels },
          State: { Status: running ? "running" : "exited", Running: running },
          Mounts: [
            { Type: "volume", Name: "world", Source: "/var/lib/docker/volumes/world/_data", Destination: "/data", RW: true },
            { Type: "volume", Name: "settings", Source: "/var/lib/docker/volumes/settings/_data", Destination: "/config", RW: true },
          ], NetworkSettings: { Ports: {} },
          Created: "2026-01-01T00:00:00Z",
        };
      },
      stop: async () => { stopped++; running = false; },
      start: async () => { running = true; },
    })) as unknown as typeof docker.getContainer;
    await refreshServers();
    const serverId = listLogicalServers()[0].id;
    setServerGrant(operator.id, serverId, ["server.view", "backups.create"], admin);
    context = await resolveAuthorizedServer(operator, serverId, "backups.create");
    const saved: JobContext["job"] = {
      id: crypto.randomUUID(), serverId, actorId: operator.id, kind: "backup",
      status: "running", phase: "validating", input: {}, recovery: {},
      bindingRevision: context.logical.bindingRevision,
      createdAt: 0, updatedAt: 0, error: null, result: null,
    };
    job = {
      job: saved,
      progress: (phase, patch) => {
        saved.phase = phase;
        Object.assign(saved.recovery, patch);
      },
    };
  });
  afterEach(() => {
    mock.restore();
    docker.listContainers = originalList;
    docker.getContainer = originalGet;
    closeDatabase();
  });
  it("rechecks the backup grant after Docker inspection before stopping", async () => {
    onInspect = () => setServerGrant(
      operator.id, context.logical.id, ["server.view"], admin,
    );
    await expect(stopForDataOperation(context, job)).rejects.toThrow(/permission/);
    expect(stopped).toBe(0);
    expect(job.job.recovery).toStrictEqual({});
  });
  it("rejects an owner disabled during Docker inspection", async () => {
    onInspect = () => updateUserAccess(operator.id, "operator", true);
    await expect(stopForDataOperation(context, job)).rejects.toThrow(/no longer has access/);
    expect(stopped).toBe(0);
  });
  function scheduledBackup(): string {
    setServerGrant(operator.id, context.logical.id, [
      "server.view", "backups.create", "schedules.manage",
    ], admin);
    const schedule = createSchedule(operator, context.logical.id, {
      action: "backup", enabled: true, time: "08:00", days: [1], timezone: "UTC",
    });
    job.job.input = { scheduleId: schedule.id, scheduleRevision: schedule.revision };
    return schedule.id;
  }

  it("rechecks a paused and resumed backup schedule after inspection before stopping", async () => {
    const scheduleId = scheduledBackup();
    onInspect = () => {
      onInspect = () => {};
      setScheduleEnabled(operator, context.logical.id, scheduleId, { enabled: false, revision: 1 });
      setScheduleEnabled(operator, context.logical.id, scheduleId, { enabled: true, revision: 2 });
    };
    await expect(stopForDataOperation(context, job)).rejects.toThrow(/deleted, disabled, or changed/);
    expect(stopped).toBe(0);
    expect(job.job.recovery).toStrictEqual({});
  });

  for (const restoreRoots of [null,
    [{ root: { id: "../outside", path: "/data" }, phase: "replaced" }],
    [0, 1].map(() => ({ root: { id: "root-0", path: "/data" }, phase: "replaced" })),
  ]) {
    it(`leaves the server stopped when restore recovery records are invalid: ${JSON.stringify(restoreRoots)}`, async () => {
      await stopForDataOperation(context, job);
      job.job.kind = "restore";
      job.job.recovery.restore = { status: "replacing", roots: restoreRoots };
      await expect(recoverRestore(job)).rejects.toMatchObject({ code: "INVALID_RESTORE_JOURNAL" });
      expect(running).toBe(false);
      expect(job.job.recovery.stateRestored).toBe(undefined);
      expect(job.job.recovery.restore).toStrictEqual({ status: "replacing", roots: restoreRoots });
    });
  }

  it("rejects a missing restore journal after data replacement began", async () => {
    await stopForDataOperation(context, job);
    job.job.kind = "restore";
    job.job.recovery.dataSafe = false;
    await expect(recoverRestore(job)).rejects.toMatchObject({ code: "INVALID_RESTORE_JOURNAL" });
    expect(running).toBe(false);
  });

  it("restores initial running state when interrupted before restore staging", async () => {
    await stopForDataOperation(context, job);
    job.job.kind = "restore";
    await recoverRestore(job);
    expect(running).toBe(true);
  });

  it("restores running state after committed restore cleanup was persisted", async () => {
    await stopForDataOperation(context, job);
    job.job.kind = "restore";
    job.job.recovery.restore = { status: "cleaned" };
    await recoverRestore(job);
    expect(running).toBe(true);
    expect(job.job.recovery.stateRestored).toBe(true);
  });

  it("resumes a multi-root rollback without deleting an already recovered root", async () => {
    await stopForDataOperation(context, job);
    job.job.kind = "restore";
    const roots = context.container.fileRoots.map(({ id, path }) => ({ id, path }));
    expect(roots).toHaveLength(2);
    job.job.recovery.restore = {
      status: "rolling_back",
      roots: roots.map((root, index) => ({ root, phase: index === 0 ? "replaced" : "rolled_back" })),
    };
    const cleanup = mock(async () => {});
    spyOn(backupStorage, "createDataHelper").mockResolvedValue({
      container: { id: "restore-helper" } as backupStorage.DataHelper["container"], roots, cleanup,
    });
    const steps: string[] = [];
    spyOn(backupStorage, "helperExec").mockImplementation(async (_container, command) => {
      const request = JSON.parse(command[3]) as { operation: string; root: string };
      steps.push(`${request.operation}:${request.root}`);
      if (request.operation === "rollbackOld") {
        // This persisted checkpoint must still identify the completed root
        // if the process exits while returning the next root's originals.
        const checkpoint = job.job.recovery.restore as { roots: unknown[] };
        expect(checkpoint.roots).toHaveLength(2);
      }
      return "{}";
    });
    await recoverRestore(job);
    expect(steps).toEqual([
      `cleanup:/mounts/${roots[1].id}`,
      `rollbackClean:/mounts/${roots[0].id}`,
      `rollbackOld:/mounts/${roots[0].id}`,
      `cleanup:/mounts/${roots[0].id}`,
    ]);
    expect(job.job.recovery.restore).toStrictEqual({ status: "rolled_back" });
    expect(running).toBe(true);
    expect(cleanup).toHaveBeenCalledTimes(1);
  });

  it("recovers only this operation's helpers and removes their anonymous volumes", async () => {
    const removed: Array<{ id: string; options: unknown }> = [];
    docker.listContainers = (async () => [
      { Id: "backup", Labels: { "ludock.internal": "backup-helper", "ludock.operation": job.job.id } },
      { Id: "validator", Labels: { "ludock.internal": "mount-validator", "ludock.operation": job.job.id } },
      { Id: "other-job", Labels: { "ludock.internal": "backup-helper", "ludock.operation": "other" } },
      { Id: "game", Labels: { "ludock.enable": "true", "ludock.operation": job.job.id } },
    ]) as unknown as typeof docker.listContainers;
    docker.getContainer = ((id: string) => ({
      remove: async (options: unknown) => { removed.push({ id, options }); },
    })) as unknown as typeof docker.getContainer;
    await recoverBackup(job);
    expect(removed).toEqual([
      { id: "backup", options: { force: true, v: true } },
      { id: "validator", options: { force: true, v: true } },
    ]);
  });

  it("restores a stopped backup server during recovery even after its schedule is paused", async () => {
    const scheduleId = scheduledBackup();
    await stopForDataOperation(context, job);
    expect(running).toBe(false);
    setScheduleEnabled(operator, context.logical.id, scheduleId, { enabled: false, revision: 1 });
    await recoverBackup(job);
    expect(running).toBe(true);
    expect(job.job.recovery.stateRestored).toBe(true);
  });
});
