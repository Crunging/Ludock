import { mkdtemp, realpath, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import type * as Docker from "../src/docker-client.js";
import { expect, afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";
import * as storage from "../src/backup-storage.js";
import { getBackupPreflight, getBackupStorageStatus } from "../src/backups.js";
import { closeDatabase } from "../src/database.js";
import { docker } from "../src/docker-client.js";
import { reconcileServers } from "../src/identity.js";
import { setSetting } from "../src/settings.js";
import type { ServerContext } from "../src/servers.js";
import { dockerId } from "./fixtures/ids.js";

let directory: string;
let context: ServerContext;
let oldRoots: string | undefined;
let oldSelf: string | undefined;

beforeEach(async () => {
  process.env.LUDOCK_DB_PATH = ":memory:";
  closeDatabase();
  directory = await realpath(await mkdtemp(path.join(tmpdir(), "ludock-readiness-")));
  oldRoots = process.env.LUDOCK_BACKUP_ROOTS;
  oldSelf = process.env.LUDOCK_SELF_CONTAINER;
  process.env.LUDOCK_BACKUP_ROOTS = directory;
  process.env.LUDOCK_SELF_CONTAINER = "ludock-readiness-fixture";
  const observation = {
    containerId: dockerId("readiness-game"), name: "readiness-game", displayName: "Readiness fixture",
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
      ? { Mounts: [{ Type: "bind", Source: "/srv/ludock-archives", Destination: directory, RW: true }] }
      : { State: { Status: "running", Running: true, Paused: false }, Mounts: [{ Type: "bind", Source: "/srv/game", Destination: "/data", RW: true }] },
  }) as Docker.Container);
  spyOn(docker, "listContainers").mockResolvedValue([]);
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

describe("backup storage visibility", () => {
  it("preserves unknown disk space and hides raw filesystem diagnostics", async () => {
    spyOn(storage, "availableBackupDestinationBytes").mockRejectedValue(new Error("EACCES /private/secret token=fixture-secret"));
    const result = await getBackupStorageStatus();
    expect(result.availableBytes).toBe(null);
    expect(result.issues[0].code).toBe("BACKUP_DISK_UNAVAILABLE");
    expect(JSON.stringify(result)).not.toMatch(/private|secret|token=/);
  });
});

describe("read-only backup preflight", () => {
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
