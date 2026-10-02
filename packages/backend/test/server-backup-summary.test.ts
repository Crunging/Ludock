import { expect, afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";
import type { DockerContainerId } from "@ludock/shared";
import { closeDatabase, createUser, getDatabase, type SessionUser } from "../src/database.js";
import { setServerGrant } from "./fixtures/grants.js";
import * as docker from "../src/docker.js";
import { getServer, listServers } from "../src/servers.js";
import { dockerId } from "./fixtures/ids.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const admin: SessionUser = { id: "admin", username: "admin", role: "admin" };
const operator: SessionUser = { id: "operator", username: "operator", role: "operator" };
const viewer: SessionUser = { id: "viewer", username: "viewer", role: "viewer" };
let serverId: string;

function observation(name: string): docker.ManagedContainerObservation {
  return {
    container: {
      id: name as DockerContainerId,
      shortId: name,
      name,
      displayName: name,
      image: "itzg/minecraft-server",
      state: "running",
      status: "Up",
      gameType: "minecraft",
      gameConsole: null,
      fileRoots: [],
      ports: [],
      created: 0,
      labels: {},
    },
    observation: {
      containerId: dockerId(name),
      name,
      displayName: name,
      gameType: "minecraft",
      mounts: [],
    },
  };
}

function backup(createdAt: number, size: number) {
  const id = crypto.randomUUID();
  getDatabase().prepare(
    `INSERT INTO backups
      (id,server_id,binding_fingerprint,destination,roots_json,size,checksum,created_at,state)
      VALUES(?,?,?,?,?,?,?,?,?)`,
  ).run(id, serverId, "fixture-fingerprint", "/private-backup-destination", "[]", size, "private-checksum", createdAt, "complete");
}

beforeEach(async () => {
  closeDatabase();
  for (const actor of [admin, operator, viewer]) {
    createUser({ ...actor, passwordHash: "unused", disabled: false, createdAt: 1 });
  }
  const observations = [observation("backup-summary-fixture")];
  spyOn(docker, "listManagedContainerObservations").mockImplementation(async () => observations);
  serverId = (await listServers(admin))[0].id;
});

afterEach(() => {
  mock.restore();
  closeDatabase();
});

describe("server backup summaries", () => {
  it("exposes only timestamp and size to backup creators and removes them when the grant is revoked", async () => {
    backup(100, 12);
    setServerGrant(operator.id, serverId, ["server.view", "backups.create"], admin);
    const server = await getServer(operator, serverId);
    expect(server.latestBackup).toStrictEqual({ createdAt: 100, size: 12 });
    expect(server.permissions.includes("backups.read")).toBe(false);
    expect(JSON.stringify(server)).not.toMatch(/private-backup-destination|private-checksum|fixture-fingerprint/);

    setServerGrant(operator.id, serverId, ["server.view", "server.start"], admin);
    expect((await getServer(operator, serverId)).latestBackup).toBe(null);
    expect((await listServers(operator))[0].latestBackup).toBe(null);
  });

  it("keeps viewer and unassigned access within their role and server grants", async () => {
    backup(100, 12);
    setServerGrant(viewer.id, serverId, ["server.view"], admin);
    // A malformed/stale grant cannot bypass the viewer role ceiling.
    getDatabase().prepare("UPDATE server_grants SET capabilities_json=? WHERE user_id=?").run(
      JSON.stringify(["server.view", "backups.create", "backups.read"]), viewer.id,
    );
    expect((await getServer(viewer, serverId)).latestBackup).toBe(null);
    expect(await listServers(operator)).toStrictEqual([]);
    await expect(getServer(operator, serverId)).rejects.toThrow(/Server not found/);
  });
});
