import { afterEach, beforeEach, describe, expect, it, mock, spyOn } from "bun:test";
import { mkdir, mkdtemp, realpath, rm, symlink } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import type { BackupSettings } from "@ludock/shared";
import { getBackupStorageStatus, initializeBackupStorage, runBackup } from "../src/backups.js";
import * as storage from "../src/backup-storage.js";
import * as database from "../src/database.js";
import * as discovery from "../src/docker.js";
import { docker, type Container, type ContainerInspectInfo } from "../src/docker-client.js";
import { startServer } from "../src/index.js";
import { reconcileServers } from "../src/identity.js";
import { getOperation } from "../src/operations.js";
import { createSchedule, runSchedules } from "../src/schedules.js";
import * as servers from "../src/servers.js";
import { getSetting, setSetting } from "../src/settings.js";
import { dockerId } from "./fixtures/ids.js";

process.env.LUDOCK_DB_PATH = ":memory:";
let directory: string;
let oldRoots: string | undefined;
let oldSelf: string | undefined;
let mounts: ContainerInspectInfo["Mounts"];
let runtime: ReturnType<typeof startServer> | undefined;
const observed = [{
  observation: {
    containerId: dockerId("auto-backup-game"), name: "game", displayName: "Game", gameType: "minecraft",
    mounts: [{ type: "bind", source: "/srv/game", destination: "/data", writable: true }],
  },
}] as discovery.ManagedContainerObservation[];

beforeEach(async () => {
  database.closeDatabase();
  directory = await realpath(await mkdtemp(path.join(tmpdir(), "ludock-auto-backups-")));
  oldRoots = process.env.LUDOCK_BACKUP_ROOTS;
  oldSelf = process.env.LUDOCK_SELF_CONTAINER;
  process.env.LUDOCK_BACKUP_ROOTS = directory;
  process.env.LUDOCK_SELF_CONTAINER = "ludock-auto-fixture";
  mounts = [{ Type: "bind", Source: "/srv/ludock-archives", Destination: directory, RW: true }];
  spyOn(docker, "getContainer").mockImplementation(() => ({
    inspect: async () => ({ Mounts: mounts }),
  }) as Container);
  spyOn(discovery, "listManagedContainerObservations").mockResolvedValue(observed);
});

afterEach(async () => {
  await runtime?.shutdown("test cleanup");
  runtime = undefined;
  mock.restore();
  database.closeDatabase();
  if (oldRoots === undefined) delete process.env.LUDOCK_BACKUP_ROOTS;
  else process.env.LUDOCK_BACKUP_ROOTS = oldRoots;
  if (oldSelf === undefined) delete process.env.LUDOCK_SELF_CONTAINER;
  else process.env.LUDOCK_SELF_CONTAINER = oldSelf;
  await rm(directory, { recursive: true, force: true });
});

function automaticAudits() {
  return database.getDatabase().query(
    "SELECT user_id,action,details_json FROM audit_log WHERE action='settings.backups.auto_enabled'",
  ).all();
}

function savedRow() {
  return database.getDatabase().query("SELECT value_json FROM settings WHERE key='backups'").get();
}

describe("automatic backup storage setup", () => {
  it("enables a single valid root with current defaults and audits once without paths", async () => {
    await Promise.all([initializeBackupStorage(), initializeBackupStorage()]);
    expect(getSetting<BackupSettings>("backups")).toStrictEqual({
      destination: directory, retentionCount: 10, maxBytes: 100 * 1024 ** 3, reserveBytes: 5 * 1024 ** 3,
    });
    await initializeBackupStorage();
    expect(automaticAudits()).toStrictEqual([{
      user_id: null, action: "settings.backups.auto_enabled", details_json: null,
    }]);
    for (const table of ["backups", "operations", "schedules"])
      expect(database.getDatabase().query(`SELECT * FROM ${table}`).all()).toStrictEqual([]);
  });

  for (const saved of [null, false, { destination: "" }, {
    destination: "/chosen-backups", retentionCount: 2, maxBytes: 12345, reserveBytes: 0,
  }]) {
    it(`preserves a saved configuration: ${JSON.stringify(saved)}`, async () => {
      setSetting("backups", saved);
      await initializeBackupStorage();
      expect(getSetting<unknown>("backups")).toStrictEqual(saved);
      expect(automaticAudits()).toStrictEqual([]);
      expect(discovery.listManagedContainerObservations).not.toHaveBeenCalled();
    });
  }

  for (const count of [0, 2]) {
    it(`leaves storage unconfigured with ${count} approved roots`, async () => {
      process.env.LUDOCK_BACKUP_ROOTS = count === 0 ? "" : [directory, path.join(directory, "other")].join(path.delimiter);
      await initializeBackupStorage();
      expect(savedRow()).toBeNull();
      expect(automaticAudits()).toStrictEqual([]);
      expect(docker.getContainer).not.toHaveBeenCalled();
    });
  }

  for (const invalid of ["missing", "symlink", "read-only mount", "unmounted", "unverifiable", "overlapping game data"]) {
    it(`leaves storage unconfigured for an invalid root: ${invalid}`, async () => {
      if (invalid === "missing") process.env.LUDOCK_BACKUP_ROOTS = path.join(directory, "missing");
      if (invalid === "symlink") {
        await mkdir(path.join(directory, "archives"));
        await symlink(path.join(directory, "archives"), path.join(directory, "link"));
        process.env.LUDOCK_BACKUP_ROOTS = path.join(directory, "link");
      }
      if (invalid === "read-only mount") mounts[0].RW = false;
      if (invalid === "unmounted") mounts = [];
      if (invalid === "unverifiable") spyOn(docker, "getContainer").mockImplementation(() => ({
        inspect: async () => { throw new Error("Docker unavailable /private/host-path"); },
      }) as unknown as Container);
      if (invalid === "overlapping game data") mounts[0].Source = "/srv/game/archives";
      await initializeBackupStorage();
      expect(savedRow()).toBeNull();
      expect(automaticAudits()).toStrictEqual([]);
      expect(await getBackupStorageStatus()).toMatchObject({
        configured: false, issues: [{ code: "BACKUPS_NOT_CONFIGURED" }],
      });
    });
  }

  it("does not enable storage when game data separation cannot be checked", async () => {
    spyOn(discovery, "listManagedContainerObservations").mockRejectedValue(new Error("Discovery unavailable"));
    await initializeBackupStorage();
    expect(savedRow()).toBeNull();
    expect(automaticAudits()).toStrictEqual([]);
  });

  for (const saved of [null, { destination: "/chosen-backups", retentionCount: 7, maxBytes: 100, reserveBytes: 0 }]) {
    it(`preserves a decision saved during validation: ${JSON.stringify(saved)}`, async () => {
      spyOn(discovery, "listManagedContainerObservations").mockImplementation(async () => {
        setSetting("backups", saved);
        return observed;
      });
      await initializeBackupStorage();
      expect(getSetting<unknown>("backups")).toStrictEqual(saved);
      expect(automaticAudits()).toStrictEqual([]);
    });
  }

  it("rolls back automatic setup if its audit event cannot be persisted", async () => {
    spyOn(database, "writeAuditLog").mockImplementation(() => { throw new Error("Audit unavailable"); });
    await expect(initializeBackupStorage()).rejects.toThrow("Audit unavailable");
    expect(savedRow()).toBeNull();
  });

  it("waits for startup storage initialization before executing a due backup schedule", async () => {
    const actor = { id: "scheduler", username: "scheduler", role: "admin" as const };
    database.createUser({ ...actor, passwordHash: "fixture", disabled: false, createdAt: 1 });
    const logical = reconcileServers([observed[0].observation])[0];
    const schedule = createSchedule(actor, logical.id, {
      action: "backup", enabled: true, time: "08:00", timezone: "UTC", days: [0, 1, 2, 3, 4, 5, 6],
    });
    runSchedules(Date.parse("2026-09-15T08:00:00Z"));
    const attempt = database.getDatabase().query(
      "SELECT last_operation_id AS id FROM schedules WHERE id=?",
    ).get(schedule.id) as { id: string };
    const job = getOperation(attempt.id)!;
    let started!: () => void;
    let release!: () => void;
    const startedValidation = new Promise<void>((resolve) => { started = resolve; });
    const gate = new Promise<void>((resolve) => { release = resolve; });
    spyOn(discovery, "listManagedContainerObservations").mockImplementation(async () => {
      started();
      await gate;
      return observed;
    });
    spyOn(servers, "resolveAuthorizedServer").mockResolvedValue({
      logical, observation: observed[0].observation, container: { id: logical.containerId }, lockKeys: [],
    } as unknown as servers.ServerContext);
    // End the fixture before any server stop or archive work, once settings
    // have passed validation and execution reaches the usual capacity check.
    const capacity = spyOn(storage, "assertDestinationSpace").mockRejectedValue(new Error("Fixture capacity check"));
    const initialization = initializeBackupStorage();
    await startedValidation;
    let settled = false;
    const outcome = runBackup({ job, progress() {} }).then(
      () => { settled = true; return null; },
      (error: unknown) => { settled = true; return error; },
    );
    try {
      for (let index = 0; index < 30; index++) await Promise.resolve();
      expect(settled, "A due backup must wait instead of failing with unconfigured storage").toBe(false);
      expect(discovery.listManagedContainerObservations).toHaveBeenCalledTimes(1);
      expect(capacity).not.toHaveBeenCalled();
      release();
      await initialization;
      expect(await outcome).toMatchObject({ message: "Fixture capacity check" });
      expect(getSetting<BackupSettings>("backups")?.destination).toBe(directory);
      expect(capacity).toHaveBeenCalledTimes(1);
    } finally {
      release();
      await Promise.allSettled([initialization, outcome]);
    }
  });

  it("enables storage during startup without an administrator opening Settings", async () => {
    const enabled = new Promise<void>((resolve) => {
      const write = database.writeAuditLog;
      spyOn(database, "writeAuditLog").mockImplementation((...args) => {
        write(...args);
        if (args[0].action === "settings.backups.auto_enabled") resolve();
      });
    });
    spyOn(docker, "listContainers").mockResolvedValue([]);
    spyOn(discovery, "listManagedContainerObservations").mockResolvedValue([]);
    runtime = startServer({ port: 0, hostname: "127.0.0.1", frontendDist: false });
    await enabled;
    expect(getSetting<BackupSettings>("backups")?.destination).toBe(directory);
  });
});
