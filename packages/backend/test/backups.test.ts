import { expect, afterEach, beforeEach, describe, it } from "bun:test";
import { mountsOverlap, recoverBackup, recoverRestore, stopForDataOperation } from "../src/backups.js";
import type { ServerObservation } from "../src/identity.js";
import { listLogicalServers } from "../src/identity.js";
import { setServerGrant } from "../src/authorization.js";
import {
  closeDatabase,
  createUser,
  updateUserAccess,
} from "../src/database.js";
import { getDockerInstance } from "../src/docker.js";
import type { JobContext } from "../src/operations.js";
import { createSchedule, setScheduleEnabled } from "../src/schedules.js";
import {
  refreshServers,
  resolveAuthorizedServer,
  type ServerContext,
} from "../src/servers.js";

const bind = (source: string): ServerObservation["mounts"][number] => ({
  type: "bind",
  source,
  destination: "/data",
  writable: true,
});

describe("backup shared-writer boundaries", () => {
  it("recognizes host root and normalized parent mounts in both directions", () => {
    for (const [left, right] of [
      ["/", "/srv/games/world"],
      ["/srv/games/", "/srv/games/world"],
      ["/srv//games/./", "/srv/games/world"],
      ["/srv/games/world", "/srv/games/world"],
    ]) {
      expect(mountsOverlap(bind(left), bind(right)), `${left} contains ${right}`).toBe(true);
      expect(mountsOverlap(bind(right), bind(left)), `${right} overlaps ${left}`).toBe(true);
    }
  });

  it("keeps sibling paths and distinct named volumes separate", () => {
    expect(mountsOverlap(bind("/srv/game"), bind("/srv/games"))).toBe(false);
    expect(mountsOverlap(bind(""), bind("/srv/games"))).toBe(false);
    const volume = {
      ...bind("/var/lib/docker/volumes/world/_data"),
      type: "volume", name: "world",
    };
    expect(mountsOverlap(volume, { ...volume, destination: "/other-data" })).toBe(true);
    expect(mountsOverlap(volume, { ...volume, name: "other-world" })).toBe(false);
    expect(mountsOverlap(volume, bind("/var/lib/docker/volumes"))).toBe(true);
  });
});

describe("backup execution authority", () => {
  const admin = { id: "admin", username: "admin", role: "admin" as const };
  const operator = {
    id: "operator", username: "operator", role: "operator" as const,
  };
  const docker = getDockerInstance();
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
          Mounts: [], NetworkSettings: { Ports: {} },
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
  it("records and stops for an authorized backup without requiring a lifecycle grant", async () => {
    await stopForDataOperation(context, job);
    expect(stopped).toBe(1);
    expect(job.job.recovery.initialRunning).toBe(true);
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

  for (const restoreRoots of [null, {}, [{ root: {}, phase: "staging" }],
    [{ root: { id: "root-0", path: "/data" }, phase: "unknown" }],
    [{ root: { id: "../outside", path: "/data" }, phase: "replaced" }],
    [0, 1].map(() => ({ root: { id: "root-0", path: "/data" }, phase: "replaced" })),
  ]) {
    it(`leaves the server stopped when restore recovery records are invalid: ${JSON.stringify(restoreRoots)}`, async () => {
      await stopForDataOperation(context, job);
      job.job.kind = "restore";
      job.job.recovery.restoreRoots = restoreRoots;
      await expect(recoverRestore(job)).rejects.toMatchObject({ code: "INVALID_RESTORE_JOURNAL" });
      expect(running).toBe(false);
      expect(job.job.recovery.stateRestored).toBe(undefined);
      expect(job.job.recovery.restoreRoots).toStrictEqual(restoreRoots);
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
