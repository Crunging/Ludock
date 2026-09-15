import assert from "node:assert/strict";
import { mkdtemp, readdir, realpath, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import type * as Docker from "../src/docker-client.js";
import { afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";
import { backupPreflightResponseSchema, backupStorageResponseSchema } from "@ludock/shared";
import * as storage from "../src/backup-storage.js";
import { getBackupPreflight, getBackupStorageStatus } from "../src/backups.js";
import { closeDatabase, getDatabase } from "../src/database.js";
import { getDockerInstance } from "../src/docker.js";
import { reconcileServers } from "../src/identity.js";
import { setSetting } from "../src/settings.js";
import type { ServerContext } from "../src/servers.js";

const docker = getDockerInstance();
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
    assert.deepEqual(result, {
      configured: true, archiveBytes: 500, maxBytes: 10000,
      reserveBytes: 100, availableBytes: 5000, issues: [],
    });
    assert.equal(backupStorageResponseSchema.safeParse({ storage: result }).success, true);
  });

  it("reports retained usage even when no destination is configured", async () => {
    addBackup(200, "complete");
    getDatabase().prepare("DELETE FROM settings WHERE key='backups'").run();
    const result = await getBackupStorageStatus();
    assert.equal(result.configured, false);
    assert.equal(result.archiveBytes, 200);
    assert.equal(result.maxBytes, null);
    assert.equal(result.availableBytes, null);
    assert.equal(result.issues[0].code, "BACKUPS_NOT_CONFIGURED");
  });

  it("shows both exhausted archive capacity and the disk reserve problem", async () => {
    addBackup(10000, "complete");
    spyOn(storage, "availableBackupDestinationBytes").mockResolvedValue(100);
    const result = await getBackupStorageStatus();
    assert.equal(result.issues.length, 2);
    assert.match(result.issues[0].message, /global backup byte limit/);
    assert.match(result.issues[1].message, /reserve/);
  });

  it("preserves unknown disk space and hides raw filesystem diagnostics", async () => {
    spyOn(storage, "availableBackupDestinationBytes").mockRejectedValue(new Error("EACCES /private/secret token=fixture-secret"));
    const result = await getBackupStorageStatus();
    assert.equal(result.availableBytes, null);
    assert.equal(result.issues[0].code, "BACKUP_DISK_UNAVAILABLE");
    assert.doesNotMatch(JSON.stringify(result), /private|secret|token=/);
  });

  it("explains missing destinations and read-only mounts without returning paths", async () => {
    setSetting("backups", { destination: path.join(directory, "missing-private-directory"), retentionCount: 3, maxBytes: 10000, reserveBytes: 100 });
    let result = await getBackupStorageStatus();
    assert.equal(result.issues[0].code, "BACKUP_DESTINATION");
    assert.doesNotMatch(JSON.stringify(result), /missing-private-directory/);
    setSetting("backups", { destination: directory, retentionCount: 3, maxBytes: 10000, reserveBytes: 100 });
    destinationWritable = false;
    result = await getBackupStorageStatus();
    assert.equal(result.issues[0].code, "BACKUP_MOUNT_UNVERIFIED");
    assert.equal(result.availableBytes, null);
  });
});

describe("read-only backup preflight", () => {
  it("accepts running and stopped servers without creating helpers or archives", async () => {
    const create = spyOn(docker, "createContainer");
    for (state of ["running", "exited"]) {
      const result = await getBackupPreflight(context);
      assert.equal(result.ready, true);
      assert.deepEqual(result.issues, []);
      assert.equal(backupPreflightResponseSchema.safeParse({ preflight: result }).success, true);
    }
    assert.equal(create.mock.calls.length, 0);
    assert.deepEqual(await readdir(directory), []);
    assert.equal(getDatabase().prepare("SELECT COUNT(*) AS count FROM operations").get()?.count, 0);
  });

  it("collects independent root, state and shared-writer problems", async () => {
    context.container.fileRoots = [];
    state = "paused";
    otherWriter = true;
    const result = await getBackupPreflight(context);
    assert.equal(result.ready, false);
    assert.deepEqual(result.issues.map((issue) => issue.code), ["NO_BACKUP_ROOTS", "SERVER_STATE", "SHARED_DATA_WRITER"]);
  });

  it("explains nested mounts and an overlapping destination", async () => {
    context.observation.mounts.push({ type: "bind", source: "/srv/other", destination: "/data/other", writable: true });
    destinationSource = "/srv/game/backups";
    const result = await getBackupPreflight(context);
    assert.deepEqual(result.issues.map((issue) => issue.code), ["NESTED_BACKUP_MOUNT", "BACKUP_SOURCE_OVERLAP"]);
    assert.doesNotMatch(JSON.stringify(result), /\/srv\//);
  });

  it("keeps authoritative root checks when roots change after preflight", async () => {
    assert.equal((await getBackupPreflight(context)).ready, true);
    context.observation.mounts[0].writable = false;
    const create = spyOn(docker, "createContainer");
    await assert.rejects(storage.createDataHelper(context, true, crypto.randomUUID()), /no longer belongs to a writable mount/);
    assert.equal(create.mock.calls.length, 0);
  });

  it("does not expose Docker connection details when state and writers cannot be checked", async () => {
    spyOn(docker, "listContainers").mockRejectedValue(new Error("docker://fixture-secret@private-host"));
    const result = await getBackupPreflight(context);
    assert.equal(result.ready, false);
    assert.equal(result.issues[0].code, "BACKUP_WRITERS_UNVERIFIED");
    assert.doesNotMatch(JSON.stringify(result), /fixture-secret|private-host/);
  });
});
