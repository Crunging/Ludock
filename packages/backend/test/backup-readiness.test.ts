import { mkdtemp, readdir, realpath, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import type * as Docker from "../src/docker-client.js";
import { expect, afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";
import { backupPreflightResponseSchema, backupStorageResponseSchema } from "@ludock/shared";
import * as storage from "../src/backup-storage.js";
import { getBackupPreflight, getBackupStorageStatus } from "../src/backups.js";
import { closeDatabase, getDatabase } from "../src/database.js";
import { docker } from "../src/docker-client.js";
import { reconcileServers } from "../src/identity.js";
import { setSetting } from "../src/settings.js";
import type { ServerContext } from "../src/servers.js";

let directory: string;
let context: ServerContext;
let oldRoots: string | undefined;
let oldSelf: string | undefined;
let state: string;
let destinationWritable: boolean;
let destinationSource: string;
let otherWriter: boolean;

beforeEach(async () => {
  process.env.LUDOCK_DB_PATH = ":memory:";
  closeDatabase();
  directory = await realpath(await mkdtemp(path.join(tmpdir(), "ludock-readiness-")));
  oldRoots = process.env.LUDOCK_BACKUP_ROOTS;
  oldSelf = process.env.LUDOCK_SELF_CONTAINER;
  process.env.LUDOCK_BACKUP_ROOTS = directory;
  process.env.LUDOCK_SELF_CONTAINER = "ludock-readiness-fixture";
  state = "running";
  destinationWritable = true;
  destinationSource = "/srv/ludock-archives";
  otherWriter = false;
  const observation = {
    containerId: "readiness-game", name: "readiness-game", displayName: "Readiness fixture",
    gameType: "minecraft" as const,
    mounts: [{ type: "bind", source: "/srv/game", destination: "/data", writable: true }],
  };
  const logical = reconcileServers([observation])[0];
  context = {
    logical, observation,
    container: { id: observation.containerId, fileRoots: [{ id: "root-0", name: "Data", path: "/data" }] },
    lockKeys: [],
  } as unknown as ServerContext;
  setSetting("backups", { destination: directory, retentionCount: 3, maxBytes: 10000, reserveBytes: 100 });
  spyOn(storage, "availableBackupDestinationBytes").mockResolvedValue(5000);
  spyOn(docker, "getContainer").mockImplementation((id: string) => ({
    inspect: async () => id === process.env.LUDOCK_SELF_CONTAINER
      ? { Mounts: [{ Type: "bind", Source: destinationSource, Destination: directory, RW: destinationWritable }] }
      : { State: { Status: state, Running: state === "running", Paused: state === "paused" }, Mounts: [{ Type: "bind", Source: "/srv/game", Destination: "/data", RW: true }] },
  }) as Docker.Container);
  spyOn(docker, "listContainers").mockImplementation(async () => otherWriter
    ? [{ Id: "other-writer", State: "running" }] as Docker.ContainerInfo[]
    : []);
});

afterEach(async () => {
  mock.restore();
  closeDatabase();
  if (oldRoots === undefined) delete process.env.LUDOCK_BACKUP_ROOTS;
  else process.env.LUDOCK_BACKUP_ROOTS = oldRoots;
  if (oldSelf === undefined) delete process.env.LUDOCK_SELF_CONTAINER;
  else process.env.LUDOCK_SELF_CONTAINER = oldSelf;
  await rm(directory, { recursive: true, force: true });
});

function addBackup(size: number, state: "complete" | "failed", destination = directory): void {
  getDatabase().prepare(`INSERT INTO backups
    (id,server_id,binding_fingerprint,destination,roots_json,size,checksum,created_at,state)
    VALUES(?,?,?,?,?,?,?,?,?)`).run(
    crypto.randomUUID(), context.logical.id, "fixture", destination, "[]", size, "fixture", Date.now(), state,
  );
}

describe("backup storage visibility", () => {
  it("reports the same global completed archive accounting as backup execution", async () => {
    addBackup(200, "complete");
    addBackup(300, "complete", "/previous-destination");
    addBackup(900, "failed");
    const result = await getBackupStorageStatus();
    expect(result).toStrictEqual({
      configured: true, archiveBytes: 500, maxBytes: 10000,
      reserveBytes: 100, availableBytes: 5000, issues: [],
    });
    expect(backupStorageResponseSchema.safeParse({ storage: result }).success).toBe(true);
  });

  it("reports retained usage even when no destination is configured", async () => {
    addBackup(200, "complete");
    getDatabase().prepare("DELETE FROM settings WHERE key='backups'").run();
    const result = await getBackupStorageStatus();
    expect(result.configured).toBe(false);
    expect(result.archiveBytes).toBe(200);
    expect(result.maxBytes).toBe(null);
    expect(result.availableBytes).toBe(null);
    expect(result.issues[0].code).toBe("BACKUPS_NOT_CONFIGURED");
  });

  it("shows both exhausted archive capacity and the disk reserve problem", async () => {
    addBackup(10000, "complete");
    spyOn(storage, "availableBackupDestinationBytes").mockResolvedValue(100);
    const result = await getBackupStorageStatus();
    expect(result.issues.length).toBe(2);
    expect(result.issues[0].message).toMatch(/global backup byte limit/);
    expect(result.issues[1].message).toMatch(/reserve/);
  });

  it("preserves unknown disk space and hides raw filesystem diagnostics", async () => {
    spyOn(storage, "availableBackupDestinationBytes").mockRejectedValue(new Error("EACCES /private/secret token=fixture-secret"));
    const result = await getBackupStorageStatus();
    expect(result.availableBytes).toBe(null);
    expect(result.issues[0].code).toBe("BACKUP_DISK_UNAVAILABLE");
    expect(JSON.stringify(result)).not.toMatch(/private|secret|token=/);
  });

  it("explains missing destinations and read-only mounts without returning paths", async () => {
    setSetting("backups", { destination: path.join(directory, "missing-private-directory"), retentionCount: 3, maxBytes: 10000, reserveBytes: 100 });
    let result = await getBackupStorageStatus();
    expect(result.issues[0].code).toBe("BACKUP_DESTINATION");
    expect(JSON.stringify(result)).not.toMatch(/missing-private-directory/);
    setSetting("backups", { destination: directory, retentionCount: 3, maxBytes: 10000, reserveBytes: 100 });
    destinationWritable = false;
    result = await getBackupStorageStatus();
    expect(result.issues[0].code).toBe("BACKUP_MOUNT_UNVERIFIED");
    expect(result.availableBytes).toBe(null);
  });
});

describe("read-only backup preflight", () => {
  it("accepts running and stopped servers without creating helpers or archives", async () => {
    const create = spyOn(docker, "createContainer");
    for (state of ["running", "exited"]) {
      const result = await getBackupPreflight(context);
      expect(result.ready).toBe(true);
      expect(result.issues).toStrictEqual([]);
      expect(backupPreflightResponseSchema.safeParse({ preflight: result }).success).toBe(true);
    }
    expect(create.mock.calls.length).toBe(0);
    expect(await readdir(directory)).toStrictEqual([]);
    expect(getDatabase().prepare("SELECT COUNT(*) AS count FROM operations").get()?.count).toBe(0);
  });

  it("collects independent root, state and shared-writer problems", async () => {
    context.container.fileRoots = [];
    state = "paused";
    otherWriter = true;
    const result = await getBackupPreflight(context);
    expect(result.ready).toBe(false);
    expect(result.issues.map((issue) => issue.code)).toStrictEqual(["NO_BACKUP_ROOTS", "SERVER_STATE", "SHARED_DATA_WRITER"]);
  });

  it("explains nested mounts and an overlapping destination", async () => {
    context.observation.mounts.push({ type: "bind", source: "/srv/other", destination: "/data/other", writable: true });
    destinationSource = "/srv/game/backups";
    const result = await getBackupPreflight(context);
    expect(result.issues.map((issue) => issue.code)).toStrictEqual(["NESTED_BACKUP_MOUNT", "BACKUP_SOURCE_OVERLAP"]);
    expect(JSON.stringify(result)).not.toMatch(/\/srv\//);
  });

  it("keeps authoritative root checks when roots change after preflight", async () => {
    expect((await getBackupPreflight(context)).ready).toBe(true);
    context.observation.mounts[0].writable = false;
    const create = spyOn(docker, "createContainer");
    await expect(storage.createDataHelper(context, true, crypto.randomUUID())).rejects.toThrow(/no longer belongs to a writable mount/);
    expect(create.mock.calls.length).toBe(0);
  });

  it("does not expose Docker connection details when state and writers cannot be checked", async () => {
    spyOn(docker, "listContainers").mockRejectedValue(new Error("docker://fixture-secret@private-host"));
    const result = await getBackupPreflight(context);
    expect(result.ready).toBe(false);
    expect(result.issues[0].code).toBe("BACKUP_WRITERS_UNVERIFIED");
    expect(JSON.stringify(result)).not.toMatch(/fixture-secret|private-host/);
  });
});
