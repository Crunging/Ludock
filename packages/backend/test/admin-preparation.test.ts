import assert from "node:assert/strict";
import { Readable } from "node:stream";
import { serve, type Server } from "bun";
import { afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";
import type { BackupSettings } from "@ludock/shared";
import type { ServerObservation } from "../src/identity.js";
import type { ServerContext } from "../src/servers.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const [{ createApp }, auth, database, identity, servers, backups, storage, settings] = await Promise.all([
  import("../src/app.js"), import("../src/auth.js"), import("../src/database.js"),
  import("../src/identity.js"), import("../src/servers.js"), import("../src/backups.js"),
  import("../src/backup-storage.js"), import("../src/settings.js"),
]);
const actor = { id: "admin", username: "admin", role: "admin" as const };
const observation: ServerObservation = {
  containerId: "docker-fixture", name: "fixture", displayName: "Fixture", gameType: "minecraft",
  mounts: [{ type: "bind", source: "/srv/game", destination: "/data", writable: true }],
};
let server: Server<unknown>;
let cookie: string;
let sessionHash: string;
const releases: Array<() => void> = [];

function gate() {
  let started!: () => void;
  let release!: () => void;
  const pending = new Promise<void>((resolve) => { started = resolve; });
  const waiting = new Promise<void>((resolve) => { release = resolve; });
  releases.push(release);
  return { pending, release, async wait() { started(); await waiting; } };
}
beforeEach(() => {
  database.closeDatabase();
  database.createUser({ ...actor, disabled: false, passwordHash: "unused-fixture-hash", createdAt: 1 });
  const session = auth.createSession(actor, new Request("http://127.0.0.1"), "127.0.0.1");
  cookie = `ludock_session=${session.token}`;
  sessionHash = new Bun.CryptoHasher("sha256").update(session.token).digest("hex");
  server = serve({ ...createApp({ frontendDist: false }), hostname: "127.0.0.1", port: 0 });
});
afterEach(async () => {
  releases.splice(0).forEach((release) => release());
  await server.stop(true);
  mock.restore();
  database.closeDatabase();
});
function request(path: string, method = "POST", body?: unknown) {
  return fetch(new URL(path, server.url), {
    method, headers: { Cookie: cookie, ...(body === undefined ? {} : { "Content-Type": "application/json" }) },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

describe("authorization after administrator request preparation", () => {
  for (const change of ["revoke-session", "demote"] as const) {
    it(`keeps a changed binding suspended after ${change} during discovery`, async () => {
      const logical = identity.reconcileServers([observation])[0];
      identity.reconcileServers([{ ...observation, mounts: [{ ...observation.mounts[0], source: "/srv/replacement" }] }]);
      const pendingFingerprint = identity.getLogicalServer(logical.id)!.pendingFingerprint;
      const hold = gate();
      spyOn(servers, "refreshServers").mockImplementation(async () => { await hold.wait(); return new Map(); });
      const response = request(`/api/v1/servers/${logical.id}/binding-review`, "POST", { confirmation: "Fixture" });
      await hold.pending;
      if (change === "revoke-session") database.deleteSessionRecord(sessionHash);
      else database.getDatabase().prepare("UPDATE users SET role='viewer' WHERE id=?").run(actor.id);
      hold.release();
      assert.equal((await response).status, change === "revoke-session" ? 401 : 403);
      const current = identity.getLogicalServer(logical.id)!;
      assert.equal(current.status, "review_required");
      assert.equal(current.pendingFingerprint, pendingFingerprint);
      assert.equal(current.reviewRequired, true);
    });
  }

  it("does not replace backup settings when the session is revoked during destination validation", async () => {
    const initial: BackupSettings = { destination: "/backups", retentionCount: 2, maxBytes: 1024, reserveBytes: 0 };
    settings.setSetting("backups", initial);
    const hold = gate();
    spyOn(storage, "validateBackupSettings").mockImplementation(async (value) => {
      await hold.wait();
      return value as BackupSettings;
    });
    const response = request("/api/v1/settings/backups", "PUT", { ...initial, retentionCount: 9 });
    await hold.pending;
    database.deleteSessionRecord(sessionHash);
    hold.release();
    assert.equal((await response).status, 401);
    assert.deepEqual(settings.getSetting("backups"), initial);
  });

  it("closes an opened backup stream before returning bytes after access is revoked", async () => {
    const logical = identity.reconcileServers([observation])[0];
    const backupId = crypto.randomUUID();
    database.getDatabase().prepare(`INSERT INTO backups
      (id,server_id,binding_fingerprint,destination,roots_json,size,checksum,created_at,state)
      VALUES(?,?,?,?,?,?,?,?,'complete')`).run(backupId, logical.id, logical.bindingFingerprint, "/backups", "[]", 15, "fixture", 1);
    const stream = Readable.from([Buffer.from("private archive")]);
    const hold = gate();
    spyOn(backups, "openBackupDownload").mockImplementation(async () => { await hold.wait(); return stream; });
    const response = request(`/api/v1/servers/${logical.id}/backups/${backupId}/download`, "GET");
    await hold.pending;
    database.deleteSessionRecord(sessionHash);
    hold.release();
    const result = await response;
    assert.equal(result.status, 401);
    assert.doesNotMatch(await result.text(), /private archive/);
    assert.equal(stream.destroyed, true);
  });

  it("does not enqueue a backup after its requesting session is revoked during binding resolution", async () => {
    const logical = identity.reconcileServers([observation])[0];
    const hold = gate();
    spyOn(servers, "resolveAuthorizedServer").mockImplementation(async () => {
      await hold.wait();
      return { logical, observation, lockKeys: [`server:${logical.id}`] } as ServerContext;
    });
    const response = request(`/api/v1/servers/${logical.id}/backups`);
    await hold.pending;
    database.deleteSessionRecord(sessionHash);
    hold.release();
    assert.equal((await response).status, 401);
    assert.equal(database.getDatabase().prepare("SELECT COUNT(*) AS count FROM operations").get()?.count, 0);
  });
});
