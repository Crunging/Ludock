import { expect, afterAll as after, beforeEach, describe, it } from "bun:test";
import {
  closeDatabase,
  createUser,
  getDatabase,
  updateUserAccess,
  type SessionUser,
} from "../src/database.js";
import {
  assertAdministrator,
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
});
