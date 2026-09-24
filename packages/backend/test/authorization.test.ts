import { expect, afterAll as after, beforeEach, describe, it } from "bun:test";
import { listAuditHistory } from "../src/history.js";
import {
  closeDatabase,
  createUser,
  getDatabase,
  updateUserAccess,
  type SessionUser,
} from "../src/database.js";
import {
  assertAdministrator,
  assertServerCapability,
  getEffectiveCapabilities,
  hasServerCapability,
  listUserServerGrants,
  setUserServerGrants,
} from "../src/authorization.js";
import { setServerGrant } from "./fixtures/grants.js";
import {
  reconcileServers,
  reviewServerBinding,
  type ServerObservation,
} from "../src/identity.js";
import { dockerId } from "./fixtures/ids.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const admin: SessionUser = { id: "admin", username: "owner", role: "admin" };
const operator: SessionUser = {
  id: "operator",
  username: "friend",
  role: "operator",
};
const viewer: SessionUser = {
  id: "viewer",
  username: "reader",
  role: "viewer",
};
const observations: ServerObservation[] = ["minecraft", "factorio"].map(
  (name) => ({
    containerId: dockerId(`docker-${name}`),
    name,
    displayName: name,
    gameType: name,
    mounts: [],
  }),
);
let servers: ReturnType<typeof reconcileServers>;
beforeEach(() => {
  closeDatabase();
  for (const user of [admin, operator, viewer]) {
    createUser({
      ...user,
      passwordHash: "not-a-real-hash",
      disabled: false,
      createdAt: Date.now(),
    });
  }
  servers = reconcileServers(observations);
});
after(() => closeDatabase());

describe("server assignments and independent capabilities", () => {
  it("gives administrators discovered servers and gives new non-admin accounts none", () => {
    expect(hasServerCapability(admin, servers[0], "console.shell")).toBe(true);
    expect(getEffectiveCapabilities(operator, servers[0])).toStrictEqual([]);
    expect(getEffectiveCapabilities(viewer, servers[0])).toStrictEqual([]);
    expect(() => assertServerCapability(operator, servers[0].id, "server.view")).toThrow(expect.objectContaining({ statusCode: 404, message: "Server not found" }));
    expect(() => assertServerCapability(operator, "unknown-uuid", "server.view")).toThrow(expect.objectContaining({ statusCode: 404, message: "Server not found" }));
  });

  it("lets a friend start and stop only assigned servers, without adjacent powers", () => {
    setServerGrant(
      operator.id,
      servers[0].id,
      ["server.view", "server.start", "server.stop"],
      admin,
    );
    expect(getEffectiveCapabilities(operator, servers[0])).toStrictEqual([
      "server.view",
      "server.start",
      "server.stop",
    ]);
    expect(() =>
      assertServerCapability(operator, servers[0].id, "server.stop")).not.toThrow();
    for (const capability of [
      "server.restart",
      "console.execute",
      "logs.read",
      "files.read",
      "files.write",
      "backups.create",
      "schedules.manage",
      "server.update",
    ] as const) {
      expect(() => assertServerCapability(operator, servers[0].id, capability)).toThrow(expect.objectContaining({ statusCode: 403 }));
    }
    expect(() => assertServerCapability(operator, servers[1].id, "server.stop")).toThrow(expect.objectContaining({ statusCode: 404 }));
    expect(listAuditHistory({ limit: 50 }).entries.some((event) => event.action === "authorization.denied")).toBeTruthy();
  });

  it("rejects grants exceeding ceilings and requires explicit read prerequisites", () => {
    expect(() =>
        setServerGrant(
          viewer.id,
          servers[0].id,
          ["server.view", "server.start"],
          admin,
        )).toThrow(/exceeds/);
    expect(() =>
        setServerGrant(
          operator.id,
          servers[0].id,
          ["server.view", "console.shell"],
          admin,
        )).toThrow(/exceeds/);
    expect(() => setServerGrant(operator.id, servers[0].id, ["server.start"], admin)).toThrow(/Server access is required/);
    expect(() =>
        setServerGrant(
          operator.id,
          servers[0].id,
          ["server.view", "files.write"],
          admin,
        )).toThrow(/requires file reading/);
    expect(() =>
        setServerGrant(
          operator.id,
          servers[0].id,
          ["server.view", "made.up"],
          admin,
        )).toThrow(/exceeds/);
    expect(listUserServerGrants(operator.id)).toStrictEqual([]);
  });

  it("enforces viewer ceilings even with malformed database grants", () => {
    getDatabase()
      .prepare("INSERT INTO server_grants VALUES (?, ?, ?, ?)")
      .run(
        viewer.id,
        servers[0].id,
        JSON.stringify([
          "server.view",
          "logs.read",
          "files.read",
          "files.write",
          "console.shell",
          "server.start",
        ]),
        Date.now(),
      );
    expect(getEffectiveCapabilities(viewer, servers[0])).toStrictEqual([
      "server.view",
      "logs.read",
      "files.read",
    ]);
    expect(hasServerCapability(viewer, servers[0], "server.start")).toBe(false);
    getDatabase()
      .prepare("UPDATE server_grants SET capabilities_json = ?")
      .run("invalid json");
    expect(getEffectiveCapabilities(viewer, servers[0])).toStrictEqual([]);
  });

  it("does not turn backup/schedule permissions into lifecycle or console access", () => {
    setServerGrant(
      operator.id,
      servers[0].id,
      ["server.view", "backups.create", "schedules.manage"],
      admin,
    );
    expect(hasServerCapability(operator, servers[0], "backups.create")).toBe(true);
    expect(hasServerCapability(operator, servers[0], "server.stop")).toBe(false);
    expect(hasServerCapability(operator, servers[0], "console.execute")).toBe(false);
    expect(hasServerCapability(operator, servers[0], "backups.read")).toBe(false);
    expect(hasServerCapability(operator, servers[0], "backups.restore")).toBe(false);
  });

  it("immediately rechecks revocation, disabling, and role downgrades despite stale actors", () => {
    setServerGrant(
      operator.id,
      servers[0].id,
      ["server.view", "server.stop"],
      admin,
    );
    updateUserAccess(operator.id, "viewer", false);
    expect(hasServerCapability(operator, servers[0], "server.stop")).toBe(false);
    expect(hasServerCapability(operator, servers[0], "server.view")).toBe(true);
    updateUserAccess(operator.id, "operator", true);
    expect(getEffectiveCapabilities(operator, servers[0])).toStrictEqual([]);
    updateUserAccess(operator.id, "operator", false);
    expect(hasServerCapability(operator, servers[0], "server.stop")).toBe(true);
    setUserServerGrants(operator.id, [], admin);
    expect(getEffectiveCapabilities(operator, servers[0])).toStrictEqual([]);
    updateUserAccess(admin.id, "viewer", false);
    expect(() => assertAdministrator(admin)).toThrow(/Administrator permission/);
    expect(getEffectiveCapabilities(admin, servers[0])).toStrictEqual([]);
  });

  it("preserves grants on ordinary recreation and suspends them on material changes until reviewed", () => {
    const first = servers.find((server) => server.gameType === "minecraft")!;
    setServerGrant(
      operator.id,
      first.id,
      ["server.view", "server.stop"],
      admin,
    );
    const replaced = observations.map((entry) => ({
      ...entry,
      containerId: dockerId(`${entry.containerId}-replacement`),
    }));
    reconcileServers(replaced);
    expect(hasServerCapability(operator, first.id, "server.stop")).toBe(true);
    replaced[0] = { ...replaced[0], gameType: "unrelated-server" };
    const pending = reconcileServers(replaced).find(
      (server) => server.id === first.id,
    )!;
    expect(hasServerCapability(operator, first.id, "server.view")).toBe(false);
    expect(getEffectiveCapabilities(admin, first.id)).toStrictEqual([
      "server.view",
    ]);
    expect(listUserServerGrants(operator.id).length).toBe(1);
    reviewServerBinding(first.id, pending.pendingFingerprint!);
    expect(hasServerCapability(operator, first.id, "server.stop")).toBe(true);
  });

  it("keeps an assignment replacement atomic when one supplied grant is invalid", () => {
    setServerGrant(operator.id, servers[0].id, ["server.view"], admin);
    expect(() =>
        setUserServerGrants(
          operator.id,
          [
            { serverId: servers[1].id, capabilities: ["server.view"] },
            { serverId: "nonexistent", capabilities: ["server.view"] },
          ],
          admin,
        )).toThrow(/Server not found/);
    expect(listUserServerGrants(operator.id)[0].serverId).toBe(servers[0].id);
    expect(() => setServerGrant(viewer.id, servers[0].id, ["server.view"], operator)).toThrow(/Administrator/);
    expect(() => setUserServerGrants(admin.id, [], admin)).toThrow(/already have access/);
  });

  it("supports the authenticated API-token principal without a synthetic users row", () => {
    const token: SessionUser = {
      id: "api-token",
      username: "api-token",
      role: "admin",
    };
    expect(hasServerCapability(token, servers[0], "server.recreate")).toBe(true);
    expect(() =>
      setServerGrant(viewer.id, servers[0].id, ["server.view"], token)).not.toThrow();
  });
});
