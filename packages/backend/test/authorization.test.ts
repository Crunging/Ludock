import assert from "node:assert/strict";
import { afterAll as after, beforeEach, describe, it } from "bun:test";
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
  setServerGrant,
  setUserServerGrants,
} from "../src/authorization.js";
import {
  reconcileServers,
  reviewServerBinding,
  type ServerObservation,
} from "../src/identity.js";

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
    containerId: `docker-${name}`,
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
    assert.equal(hasServerCapability(admin, servers[0], "console.shell"), true);
    assert.deepEqual(getEffectiveCapabilities(operator, servers[0]), []);
    assert.deepEqual(getEffectiveCapabilities(viewer, servers[0]), []);
    assert.throws(
      () => assertServerCapability(operator, servers[0].id, "server.view"),
      { statusCode: 404, message: "Server not found" },
    );
    assert.throws(
      () => assertServerCapability(operator, "unknown-uuid", "server.view"),
      { statusCode: 404, message: "Server not found" },
    );
  });

  it("lets a friend start and stop only assigned servers, without adjacent powers", () => {
    setServerGrant(
      operator.id,
      servers[0].id,
      ["server.view", "server.start", "server.stop"],
      admin,
    );
    assert.deepEqual(getEffectiveCapabilities(operator, servers[0]), [
      "server.view",
      "server.start",
      "server.stop",
    ]);
    assert.doesNotThrow(() =>
      assertServerCapability(operator, servers[0].id, "server.stop"),
    );
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
      assert.throws(
        () => assertServerCapability(operator, servers[0].id, capability),
        { statusCode: 403 },
      );
    }
    assert.throws(
      () => assertServerCapability(operator, servers[1].id, "server.stop"),
      { statusCode: 404 },
    );
    assert.ok(
      listAuditHistory({ limit: 50 }).entries.some((event) => event.action === "authorization.denied"),
    );
  });

  it("rejects grants exceeding ceilings and requires explicit read prerequisites", () => {
    assert.throws(
      () =>
        setServerGrant(
          viewer.id,
          servers[0].id,
          ["server.view", "server.start"],
          admin,
        ),
      /exceeds/,
    );
    assert.throws(
      () =>
        setServerGrant(
          operator.id,
          servers[0].id,
          ["server.view", "console.shell"],
          admin,
        ),
      /exceeds/,
    );
    assert.throws(
      () => setServerGrant(operator.id, servers[0].id, ["server.start"], admin),
      /Server access is required/,
    );
    assert.throws(
      () =>
        setServerGrant(
          operator.id,
          servers[0].id,
          ["server.view", "files.write"],
          admin,
        ),
      /requires file reading/,
    );
    assert.throws(
      () =>
        setServerGrant(
          operator.id,
          servers[0].id,
          ["server.view", "made.up"],
          admin,
        ),
      /exceeds/,
    );
    assert.deepEqual(listUserServerGrants(operator.id), []);
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
    assert.deepEqual(getEffectiveCapabilities(viewer, servers[0]), [
      "server.view",
      "logs.read",
      "files.read",
    ]);
    assert.equal(
      hasServerCapability(viewer, servers[0], "server.start"),
      false,
    );
    getDatabase()
      .prepare("UPDATE server_grants SET capabilities_json = ?")
      .run("invalid json");
    assert.deepEqual(getEffectiveCapabilities(viewer, servers[0]), []);
  });

  it("does not turn backup/schedule permissions into lifecycle or console access", () => {
    setServerGrant(
      operator.id,
      servers[0].id,
      ["server.view", "backups.create", "schedules.manage"],
      admin,
    );
    assert.equal(
      hasServerCapability(operator, servers[0], "backups.create"),
      true,
    );
    assert.equal(
      hasServerCapability(operator, servers[0], "server.stop"),
      false,
    );
    assert.equal(
      hasServerCapability(operator, servers[0], "console.execute"),
      false,
    );
    assert.equal(
      hasServerCapability(operator, servers[0], "backups.read"),
      false,
    );
    assert.equal(
      hasServerCapability(operator, servers[0], "backups.restore"),
      false,
    );
  });

  it("immediately rechecks revocation, disabling, and role downgrades despite stale actors", () => {
    setServerGrant(
      operator.id,
      servers[0].id,
      ["server.view", "server.stop"],
      admin,
    );
    updateUserAccess(operator.id, "viewer", false);
    assert.equal(
      hasServerCapability(operator, servers[0], "server.stop"),
      false,
    );
    assert.equal(
      hasServerCapability(operator, servers[0], "server.view"),
      true,
    );
    updateUserAccess(operator.id, "operator", true);
    assert.deepEqual(getEffectiveCapabilities(operator, servers[0]), []);
    updateUserAccess(operator.id, "operator", false);
    assert.equal(
      hasServerCapability(operator, servers[0], "server.stop"),
      true,
    );
    setUserServerGrants(operator.id, [], admin);
    assert.deepEqual(getEffectiveCapabilities(operator, servers[0]), []);
    updateUserAccess(admin.id, "viewer", false);
    assert.throws(() => assertAdministrator(admin), /Administrator permission/);
    assert.deepEqual(getEffectiveCapabilities(admin, servers[0]), []);
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
      containerId: `${entry.containerId}-replacement`,
    }));
    reconcileServers(replaced);
    assert.equal(hasServerCapability(operator, first.id, "server.stop"), true);
    replaced[0] = { ...replaced[0], gameType: "unrelated-server" };
    const pending = reconcileServers(replaced).find(
      (server) => server.id === first.id,
    )!;
    assert.equal(hasServerCapability(operator, first.id, "server.view"), false);
    assert.deepEqual(getEffectiveCapabilities(admin, first.id), [
      "server.view",
    ]);
    assert.equal(listUserServerGrants(operator.id).length, 1);
    reviewServerBinding(first.id, pending.pendingFingerprint!);
    assert.equal(hasServerCapability(operator, first.id, "server.stop"), true);
  });

  it("keeps an assignment replacement atomic when one supplied grant is invalid", () => {
    setServerGrant(operator.id, servers[0].id, ["server.view"], admin);
    assert.throws(
      () =>
        setUserServerGrants(
          operator.id,
          [
            { serverId: servers[1].id, capabilities: ["server.view"] },
            { serverId: "nonexistent", capabilities: ["server.view"] },
          ],
          admin,
        ),
      /Server not found/,
    );
    assert.equal(listUserServerGrants(operator.id)[0].serverId, servers[0].id);
    assert.throws(
      () => setServerGrant(viewer.id, servers[0].id, ["server.view"], operator),
      /Administrator/,
    );
    assert.throws(
      () => setUserServerGrants(admin.id, [], admin),
      /already have access/,
    );
  });

  it("supports the authenticated API-token principal without a synthetic users row", () => {
    const token: SessionUser = {
      id: "api-token",
      username: "api-token",
      role: "admin",
    };
    assert.equal(
      hasServerCapability(token, servers[0], "server.recreate"),
      true,
    );
    assert.doesNotThrow(() =>
      setServerGrant(viewer.id, servers[0].id, ["server.view"], token),
    );
  });
});
