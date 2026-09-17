import { expect, afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";
import type { SessionUser } from "../src/database.js";
import type { ServerObservation } from "../src/identity.js";
import type { RequestContext } from "../src/routes/request.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const [database, identity, authorization, servers, backups, docker, auth, { backupsRoutes }] =
  await Promise.all([
    import("../src/database.js"),
    import("../src/identity.js"),
    import("../src/authorization.js"),
    import("../src/servers.js"),
    import("../src/backups.js"),
    import("../src/docker.js"),
    import("../src/auth.js"),
    import("../src/routes/backups.js"),
  ]);

const admin = { id: "admin", username: "admin", role: "admin" as const };
const operator = {
  id: "operator",
  username: "operator",
  role: "operator" as const,
};
const viewer = { id: "viewer", username: "viewer", role: "viewer" as const };
const observation: ServerObservation = {
  containerId: "backup-route-fixture",
  name: "backup-route-fixture",
  displayName: "Backup route fixture",
  gameType: "minecraft",
  mounts: [],
};
const storageStatus = {
  configured: true,
  archiveBytes: 12,
  maxBytes: 1024,
  reserveBytes: 128,
  availableBytes: 4096,
  issues: [],
};
const preflight = { ready: true, checkedAt: 1, issues: [] };
let serverId: string;
let backupId: string;

function context(
  user: SessionUser,
  pathname = `/api/v1/servers/${serverId}/backups`,
): RequestContext {
  const url = new URL(pathname, "http://localhost");
  const { token } = auth.createSession(user, new Request(url));
  return {
    request: new Request(url, { headers: { Cookie: `ludock_session=${token}` } }),
    url,
    params: { id: serverId },
    body: undefined,
    headers: new Headers(),
    user,
  };
}

beforeEach(() => {
  database.closeDatabase();
  for (const actor of [admin, operator, viewer]) {
    database.createUser({
      ...actor,
      passwordHash: "unused",
      disabled: false,
      createdAt: 1,
    });
  }
  serverId = identity.reconcileServers([observation])[0].id;
  authorization.setServerGrant(
    operator.id,
    serverId,
    ["server.view", "backups.create"],
    admin,
  );
  authorization.setServerGrant(viewer.id, serverId, ["server.view"], admin);
  backupId = crypto.randomUUID();
  database.getDatabase().prepare(
    `INSERT INTO backups
      (id,server_id,binding_fingerprint,destination,roots_json,size,checksum,created_at,state)
      VALUES(?,?,?,?,?,?,?,?,?)`,
  ).run(backupId, serverId, "fixture", "/backups", "[]", 12, "checksum", 1, "complete");
  spyOn(servers, "refreshServers").mockResolvedValue(new Map());
  spyOn(docker, "getManagedContainerObservation").mockResolvedValue({
    observation,
    container: {
      id: observation.containerId,
      shortId: "backup-route",
      name: observation.name,
      displayName: observation.displayName,
      image: "itzg/minecraft-server:fixture",
      state: "running",
      status: "Up",
      gameType: "minecraft",
      gameConsole: null,
      fileRoots: [],
      ports: [],
      created: 1,
      labels: {},
    },
  });
});

describe("backup storage status routes", () => {
  it("limits global storage status to administrators", async () => {
    const inspect = spyOn(backups, "getBackupStorageStatus").mockResolvedValue(storageStatus);
    const handler = backupsRoutes["/api/v1/settings/backups/status"].GET!;
    for (const user of [operator, viewer]) {
      const denied = await handler(context(user, "/api/v1/settings/backups/status"));
      expect(denied.status).toBe(403);
      expect(await denied.text()).not.toMatch(/archiveBytes|availableBytes/);
    }
    expect(inspect.mock.calls.length).toBe(0);

    const allowed = await handler(context(admin, "/api/v1/settings/backups/status"));
    expect(allowed.status).toBe(200);
    expect(await allowed.json()).toStrictEqual({ storage: storageStatus });
    expect(inspect.mock.calls.length).toBe(1);
  });

  for (const change of ["revoke-session", "demote"] as const) {
    it(`withholds storage status after ${change} during inspection`, async () => {
      const ctx = context(admin, "/api/v1/settings/backups/status");
      spyOn(backups, "getBackupStorageStatus").mockImplementation(async () => {
        if (change === "revoke-session") auth.deleteRequestSession(ctx.request);
        else database.getDatabase().prepare("UPDATE users SET role='viewer' WHERE id=?").run(admin.id);
        return storageStatus;
      });
      const handler = backupsRoutes["/api/v1/settings/backups/status"].GET!;
      await expect((async () => handler(ctx))()).rejects.toMatchObject({
        code: change === "revoke-session" ? "AUTHENTICATION_REQUIRED" : "FORBIDDEN",
      });
    });
  }
});

describe("backup preflight routes", () => {
  it("allows administrators and operators with explicit backup-create access", async () => {
    const inspect = spyOn(backups, "getBackupPreflight").mockResolvedValue(preflight);
    const handler = backupsRoutes["/api/v1/servers/:id/backups/preflight"].GET!;
    for (const user of [admin, operator]) {
      const result = await handler(context(user, `/api/v1/servers/${serverId}/backups/preflight`));
      expect(result.status).toBe(200);
      expect(await result.json()).toStrictEqual({ preflight });
    }
    expect(inspect.mock.calls.length).toBe(2);
    expect(inspect.mock.calls[0][0].logical.id).toBe(serverId);
    expect(database.getDatabase().prepare("SELECT COUNT(*) AS count FROM operations").get()?.count).toBe(0);
  });

  it("denies viewers and lifecycle-only operators before inspecting backup readiness", async () => {
    authorization.setServerGrant(operator.id, serverId, ["server.view", "server.start", "server.stop"], admin);
    const inspect = spyOn(backups, "getBackupPreflight").mockResolvedValue(preflight);
    const handler = backupsRoutes["/api/v1/servers/:id/backups/preflight"].GET!;
    for (const user of [viewer, operator]) {
      await expect((async () => handler(context(user)))()).rejects.toMatchObject({ code: "FORBIDDEN" });
    }
    expect(inspect.mock.calls.length).toBe(0);
  });

  it("rejects an unavailable binding before inspecting backup readiness", async () => {
    identity.reconcileServers([]);
    const inspect = spyOn(backups, "getBackupPreflight").mockResolvedValue(preflight);
    const handler = backupsRoutes["/api/v1/servers/:id/backups/preflight"].GET!;
    await expect((async () => handler(context(admin)))()).rejects.toMatchObject({ code: "FORBIDDEN" });
    expect(inspect.mock.calls.length).toBe(0);
  });

  it("returns advisory problems without exposing internal diagnostic fields", async () => {
    const diagnostic = {
      ready: false,
      checkedAt: 1,
      issues: [{
        code: "BACKUP_DESTINATION",
        message: "Ask an administrator to check the backup destination.",
        destination: "/private/backup-fixture",
      }],
      containerId: observation.containerId,
    };
    spyOn(backups, "getBackupPreflight").mockResolvedValue(diagnostic);
    const handler = backupsRoutes["/api/v1/servers/:id/backups/preflight"].GET!;
    const result = await handler(context(operator));
    expect(result.status).toBe(200);
    expect(await result.json()).toStrictEqual({
      preflight: {
        ready: false,
        checkedAt: 1,
        issues: [{
          code: "BACKUP_DESTINATION",
          message: "Ask an administrator to check the backup destination.",
        }],
      },
    });
  });

  it("does not inspect readiness after a session is revoked during binding resolution", async () => {
    const resolved = await servers.resolveAuthorizedServer(operator, serverId, "backups.create");
    const ctx = context(operator);
    spyOn(servers, "resolveAuthorizedServer").mockImplementation(async () => {
      auth.deleteRequestSession(ctx.request);
      return resolved;
    });
    const inspect = spyOn(backups, "getBackupPreflight").mockResolvedValue(preflight);
    const handler = backupsRoutes["/api/v1/servers/:id/backups/preflight"].GET!;
    await expect((async () => handler(ctx))()).rejects.toMatchObject({ code: "AUTHENTICATION_REQUIRED" });
    expect(inspect.mock.calls.length).toBe(0);
  });

  for (const change of ["revoke-session", "remove-grant", "demote"] as const) {
    it(`withholds preflight results after ${change} during inspection`, async () => {
      const ctx = context(operator);
      spyOn(backups, "getBackupPreflight").mockImplementation(async () => {
        if (change === "revoke-session") auth.deleteRequestSession(ctx.request);
        else if (change === "remove-grant")
          authorization.setServerGrant(operator.id, serverId, ["server.view"], admin);
        else database.getDatabase().prepare("UPDATE users SET role='viewer' WHERE id=?").run(operator.id);
        return preflight;
      });
      const handler = backupsRoutes["/api/v1/servers/:id/backups/preflight"].GET!;
      await expect((async () => handler(ctx))()).rejects.toMatchObject({
        code: change === "revoke-session" ? "AUTHENTICATION_REQUIRED" : "FORBIDDEN",
      });
    });
  }
});

afterEach(() => {
  mock.restore();
  database.closeDatabase();
});

describe("backup metadata routes", () => {
  it("keeps saved backup metadata readable when Docker discovery fails", async () => {
    spyOn(servers, "refreshServers").mockRejectedValue(new Error("Docker unavailable"));
    const handler = backupsRoutes["/api/v1/servers/:id/backups"].GET!;
    const response = await handler(context(admin));
    expect(response.status).toBe(200);
    const body = await response.json() as { backups: Array<{ id: string }> };
    expect(body.backups.map((backup) => backup.id)).toEqual([backupId]);
    expect((await handler(context(operator))).status).toBe(403);
  });

  for (const unavailable of [false, true]) {
    it(`rechecks the session after ${unavailable ? "failed" : "successful"} backup discovery`, async () => {
      const ctx = context(admin);
      spyOn(servers, "refreshServers").mockImplementation(async () => {
        auth.deleteRequestSession(ctx.request);
        if (unavailable) throw new Error("Docker unavailable");
        return new Map();
      });
      const handler = backupsRoutes["/api/v1/servers/:id/backups"].GET!;
      await expect((async () => handler(ctx))()).rejects.toMatchObject({
        code: "AUTHENTICATION_REQUIRED",
      });
    });
  }

  for (const change of ["demote", "disable", "binding-review"] as const) {
    it(`withholds backup metadata after ${change} during discovery`, async () => {
      spyOn(servers, "refreshServers").mockImplementation(async () => {
        if (change === "demote")
          database.getDatabase().query("UPDATE users SET role='viewer' WHERE id=?").run(admin.id);
        else if (change === "disable")
          database.getDatabase().query("UPDATE users SET disabled=1 WHERE id=?").run(admin.id);
        else identity.reconcileServers([{ ...observation, gameType: "changed" }]);
        throw new Error("Docker unavailable");
      });
      const handler = backupsRoutes["/api/v1/servers/:id/backups"].GET!;
      await expect((async () => handler(context(admin)))()).rejects.toMatchObject({
        code: change === "disable" ? "AUTHENTICATION_REQUIRED"
          : change === "demote" ? "SERVER_NOT_FOUND" : "FORBIDDEN",
      });
    });
  }

  it("requires administrator backup-read access rather than backup-create access", async () => {
    const handler = backupsRoutes["/api/v1/servers/:id/backups"].GET!;
    const denied = await handler(context(operator));
    expect(denied.status).toBe(403);
    expect(await denied.text()).not.toMatch(new RegExp(backupId));

    const allowed = await handler(context(admin));
    expect(allowed.status).toBe(200);
    const body = (await allowed.json()) as { backups: Array<{ id: string }> };
    expect(body.backups.map((backup) => backup.id)).toStrictEqual([backupId]);
  });
});
