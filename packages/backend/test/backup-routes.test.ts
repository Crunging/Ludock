import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";
import type { RequestContext } from "../src/routes/request.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const [database, identity, authorization, servers, { backupsRoutes }] =
  await Promise.all([
    import("../src/database.js"),
    import("../src/identity.js"),
    import("../src/authorization.js"),
    import("../src/servers.js"),
    import("../src/routes/backups.js"),
  ]);

const admin = { id: "admin", username: "admin", role: "admin" as const };
const operator = {
  id: "operator",
  username: "operator",
  role: "operator" as const,
};
let serverId: string;
let backupId: string;

function context(user: typeof admin | typeof operator): RequestContext {
  const url = new URL(`http://localhost/api/v1/servers/${serverId}/backups`);
  return {
    request: new Request(url),
    url,
    params: { id: serverId },
    body: undefined,
    headers: new Headers(),
    user,
  };
}

beforeEach(() => {
  database.closeDatabase();
  for (const actor of [admin, operator]) {
    database.createUser({
      ...actor,
      passwordHash: "unused",
      disabled: false,
      createdAt: 1,
    });
  }
  serverId = identity.reconcileServers([
    {
      containerId: "backup-route-fixture",
      name: "backup-route-fixture",
      displayName: "Backup route fixture",
      gameType: "minecraft",
      mounts: [],
    },
  ])[0].id;
  authorization.setServerGrant(
    operator.id,
    serverId,
    ["server.view", "backups.create"],
    admin,
  );
  backupId = crypto.randomUUID();
  database.getDatabase().prepare(
    `INSERT INTO backups
      (id,server_id,binding_fingerprint,destination,roots_json,size,checksum,created_at,state)
      VALUES(?,?,?,?,?,?,?,?,?)`,
  ).run(backupId, serverId, "fixture", "/backups", "[]", 12, "checksum", 1, "complete");
  spyOn(servers, "refreshServers").mockResolvedValue(new Map());
});

afterEach(() => {
  mock.restore();
  database.closeDatabase();
});

describe("backup metadata routes", () => {
  it("requires administrator backup-read access rather than backup-create access", async () => {
    const handler = backupsRoutes["/api/v1/servers/:id/backups"].GET!;
    const denied = await handler(context(operator));
    assert.equal(denied.status, 403);
    assert.doesNotMatch(await denied.text(), new RegExp(backupId));

    const allowed = await handler(context(admin));
    assert.equal(allowed.status, 200);
    const body = (await allowed.json()) as { backups: Array<{ id: string }> };
    assert.deepEqual(body.backups.map((backup) => backup.id), [backupId]);
  });
});
