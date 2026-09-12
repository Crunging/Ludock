import assert from "node:assert/strict";
import { serve, type Server } from "bun";
import { afterAll, afterEach, beforeAll, beforeEach, describe, it, spyOn } from "bun:test";
import path from "node:path";
import { scheduleResponseSchema, schedulesResponseSchema, type ServerGrantInput } from "@ludock/shared";

process.env.LUDOCK_DB_PATH = ":memory:";
process.env.LUDOCK_API_TOKEN = "integration-api-secret-0123456789abcdef";
process.env.MAX_UPLOAD_SIZE = "1.5 KiB";
process.env.MAX_UPLOAD_BYTES = "1";
process.env.LUDOCK_SETUP_CODE = "integration-setup-code-0123456789abcdef";

const [{ createApp }, { getDockerInstance }, { createLogger }, { closeDatabase, getDatabase }, { SetupWindow }, compose, { runSchedules }] =
  await Promise.all([
    import("../src/app.js"),
    import("../src/docker.js"),
    import("../src/logger.js"),
    import("../src/database.js"),
    import("../src/auth.js"),
    import("../src/compose.js"),
    import("../src/schedules.js"),
  ]);

const docker = getDockerInstance();
const testLogger = createLogger("http-test");
const originalPing = docker.ping.bind(docker);
const originalListContainers = docker.listContainers.bind(docker);
const originalGetContainer = docker.getContainer.bind(docker);
const composeAvailability = spyOn(compose, "isComposeAvailable").mockResolvedValue(false);

let server: Server<unknown> | undefined;
let baseUrl: string;
let managedStopCalled = false;
let managedStartCalled = false;
let stopGate: Promise<void> | undefined;
let unmanagedStopCalled = false;
let managedServerId = "";

const managedInfo = {
  Id: "managed-container-id",
  Names: ["/managed-fixture"],
  Image: "alpine:latest",
  State: "running",
  Status: "Up 1 minute",
  Ports: [],
  Created: 1_700_000_000,
  Labels: {
    "ludock.enable": "true",
    "ludock.name": "Managed Fixture",
    "unrelated.secret": "must-not-leak",
  },
};

beforeAll(() => {
  docker.ping = (async () => "OK") as typeof docker.ping;
  docker.listContainers = (async () => [
    managedInfo,
  ]) as unknown as typeof docker.listContainers;
  docker.getContainer = ((id: string) => {
    const managed = id === managedInfo.Id;
    return {
      inspect: async () => ({
        Id: id,
        Config: {
          Image: "alpine:latest",
          Labels: managed ? managedInfo.Labels : {},
        },
        Name: managed ? "/managed-fixture" : "/unmanaged-fixture",
        State: { Status: "running" },
        NetworkSettings: { Ports: {} },
        Created: "2024-01-01T00:00:00.000Z",
      }),
      start: async () => {
        managedStartCalled = managed;
      },
      stop: async () => {
        if (managed) managedStopCalled = true;
        else unmanagedStopCalled = true;
        if (stopGate) await stopGate;
      },
    };
  }) as unknown as typeof docker.getContainer;
});

beforeEach(async () => {
  composeAvailability.mockReset().mockResolvedValue(false);
  closeDatabase();
  managedStopCalled = false;
  managedStartCalled = false;
  unmanagedStopCalled = false;
  stopGate = undefined;
  server = serve({
    ...createApp({ frontendDist: false, setupWindow: new SetupWindow() }),
    hostname: "127.0.0.1",
    port: 0,
  });
  baseUrl = server.url.origin;
  const listed = await authorizedFetch("/api/v1/servers");
  const body = (await listed.json()) as { servers: Array<{ id: string }> };
  managedServerId = body.servers[0].id;
});

afterEach(async () => {
  await server?.stop(true);
  server = undefined;
  closeDatabase();
});

afterAll(() => {
  docker.ping = originalPing;
  docker.listContainers = originalListContainers;
  docker.getContainer = originalGetContainer;
  composeAvailability.mockRestore();
});

function authorizedFetch(path: string, init: RequestInit = {}) {
  const headers = new Headers(init.headers);
  headers.set(
    "Authorization",
    "Bearer integration-api-secret-0123456789abcdef",
  );
  return fetch(`${baseUrl}${path}`, { ...init, headers });
}

async function setupAdministrator(): Promise<string> {
  const response = await fetch(`${baseUrl}/api/v1/auth/setup`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      username: "admin",
      password: "integration-password",
      bootstrapCode: "integration-setup-code-0123456789abcdef",
    }),
  });
  assert.equal(response.status, 201);
  const cookie = (response.headers.get("set-cookie") || "").split(";")[0];
  assert.match(cookie, /ludock_session=/);
  return cookie;
}

async function createViewerSession(adminCookie: string) {
  const created = await fetch(`${baseUrl}/api/v1/users`, {
    method: "POST",
    headers: { Cookie: adminCookie, "Content-Type": "application/json" },
    body: JSON.stringify({
      username: "viewer",
      password: "viewer-password",
      role: "viewer",
    }),
  });
  assert.equal(created.status, 201);
  const { user } = await created.json() as { user: { id: string } };
  const login = await fetch(`${baseUrl}/api/v1/auth/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username: "viewer", password: "viewer-password" }),
  });
  assert.equal(login.status, 200);
  const cookie = (login.headers.get("set-cookie") || "").split(";")[0];
  assert.match(cookie, /ludock_session=/);
  return { id: user.id, cookie };
}

async function setServerGrants(userId: string, grants: ServerGrantInput[]) {
  const response = await authorizedFetch(`/api/v1/users/${userId}/server-grants`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ grants }),
  });
  assert.equal(response.status, 200);
}

async function assertAuditEntry(cookie: string, action: string, targetId?: string) {
  const response = await fetch(`${baseUrl}/api/v1/audit`, {
    headers: { Cookie: cookie },
  });
  assert.equal(response.status, 200);
  const { entries } = await response.json() as {
    entries: Array<{ action: string; targetId: string | null }>;
  };
  assert.ok(entries.some((entry) =>
    entry.action === action && (targetId === undefined || entry.targetId === targetId),
  ), `Missing ${action} audit entry`);
}

describe("HTTP application", () => {
  it("serves public status and health endpoints", async () => {
    const authStatus = await fetch(`${baseUrl}/api/v1/auth/status`);
    assert.equal(authStatus.status, 200);
    const status = (await authStatus.json()) as {
      setupRequired: boolean;
      setupLocked: boolean;
      setupExpiresAt: number | null;
      setupRemainingMs: number | null;
      authenticated: boolean;
      user: unknown;
    };
    assert.deepEqual(
      {
        ...status,
        setupExpiresAt: typeof status.setupExpiresAt,
        setupRemainingMs: typeof status.setupRemainingMs,
      },
      {
        setupRequired: true,
        setupLocked: false,
        setupExpiresAt: "number",
        setupRemainingMs: "number",
        authenticated: false,
        user: null,
      },
    );
    assert.equal(authStatus.headers.get("cache-control"), "no-store");

    const proxiedStatus = await fetch(`${baseUrl}/api/v1/auth/status`, {
      headers: {
        Cookie: "authelia_session=opaque%token; unrelated=value",
      },
    });
    assert.equal(proxiedStatus.status, 200);
    assert.equal(
      ((await proxiedStatus.json()) as { setupRequired: boolean })
        .setupRequired,
      true,
    );

    const health = await fetch(`${baseUrl}/api/v1/health`);
    assert.equal(health.status, 200);
    assert.deepEqual(await health.json(), {
      status: "ok",
      docker: "connected",
      database: "connected",
    });
    assert.equal(health.headers.get("x-content-type-options"), "nosniff");
    assert.equal(health.headers.get("x-frame-options"), "DENY");
    assert.equal(
      health.headers.get("cross-origin-opener-policy"),
      "same-origin",
    );
    assert.match(
      health.headers.get("content-security-policy") || "",
      /default-src 'self'/,
    );

    const proxiedHealth = await fetch(`${baseUrl}/api/v1/health`, {
      headers: { "X-Forwarded-Proto": "https" },
    });
    assert.equal(
      proxiedHealth.headers.get("strict-transport-security"),
      "max-age=31536000; includeSubDomains",
    );
  });

  it("completes initial setup without a token and establishes a session", async () => {
    const response = await fetch(`${baseUrl}/api/v1/auth/setup`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Origin: baseUrl.replace("http://", "https://"),
        "X-Forwarded-Proto": "https",
      },
      body: JSON.stringify({
        username: "admin",
        password: "integration-password",
        bootstrapCode: "integration-setup-code-0123456789abcdef",
      }),
    });
    assert.equal(response.status, 201);
    const setCookie = response.headers.get("set-cookie") || "";
    assert.match(setCookie, /ludock_session=/);
    assert.match(setCookie, /HttpOnly/i);
    assert.match(setCookie, /SameSite=Strict/i);
    assert.match(setCookie, /Secure/i);
    const sessionCookie = setCookie.split(";")[0];

    const me = await fetch(`${baseUrl}/api/v1/auth/me`, {
      headers: { Cookie: sessionCookie },
    });
    assert.equal(me.status, 200);
    assert.equal(
      ((await me.json()) as { user: { username: string } }).user.username,
      "admin",
    );
  });

  it("supports password login without revealing which credential failed", async () => {
    await setupAdministrator();
    const invalid = await fetch(`${baseUrl}/api/v1/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        username: "admin",
        password: "incorrect-password",
      }),
    });
    assert.equal(invalid.status, 401);
    assert.deepEqual(await invalid.json(), {
      error: "Invalid username or password",
    });

    const valid = await fetch(`${baseUrl}/api/v1/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        username: "admin",
        password: "integration-password",
      }),
    });
    assert.equal(valid.status, 200);
    assert.match(valid.headers.get("set-cookie") || "", /HttpOnly/i);
  });

  it("protects authenticated endpoints", async () => {
    assert.equal((await fetch(`${baseUrl}/api/v1/auth/me`)).status, 401);
    assert.equal(
      (
        await fetch(`${baseUrl}/api/v1/auth/me`, {
          headers: { Authorization: "Bearer wrong" },
        })
      ).status,
      401,
    );
    assert.equal((await authorizedFetch("/api/v1/auth/me")).status, 200);
    const missing = await authorizedFetch("/api/v1/does-not-exist");
    assert.equal(missing.status, 404);
    assert.deepEqual(await missing.json(), { error: "API endpoint not found" });
  });

  it("rejects cross-origin state changes", async () => {
    const response = await authorizedFetch("/api/v1/users", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Origin: "https://attacker.example",
      },
      body: JSON.stringify({
        username: "intruder",
        password: "intruder-password",
        role: "admin",
      }),
    });
    assert.equal(response.status, 403);

    const fetchMetadataResponse = await authorizedFetch("/api/v1/auth/logout", {
      method: "POST",
      headers: { "Sec-Fetch-Site": "cross-site" },
    });
    assert.equal(fetchMetadataResponse.status, 403);
  });

  it("requires an explicit binary media type for uploads", async () => {
    const response = await authorizedFetch(
      `/api/v1/servers/${managedServerId}/files/upload?root=root-0&path=&name=mod.jar`,
      {
        method: "PUT",
        headers: { "Content-Type": "text/plain" },
        body: "not accepted as an upload",
      },
    );
    assert.equal(response.status, 415);
  });

  it("enforces the readable upload limit and reports it before accessing file storage", async () => {
    const upload = (size: number) =>
      authorizedFetch(
        `/api/v1/servers/${managedServerId}/files/upload?root=root-0&path=&name=mod.jar`,
        {
          method: "PUT",
          headers: { "Content-Type": "application/octet-stream" },
          body: new Uint8Array(size),
        },
      );
    const oversized = await upload(1537);
    assert.equal(oversized.status, 413);
    assert.deepEqual(await oversized.json(), {
      error: "File exceeds the upload size limit of 1.5 KiB",
    });

    // The exact limit passes the size gate and reaches this fixture's missing
    // file root, despite MAX_UPLOAD_BYTES being configured as only one byte.
    const boundary = await upload(1536);
    assert.equal(boundary.status, 404);
    assert.deepEqual(await boundary.json(), { error: "File root not found" });
  });

  it("manages users without exposing password hashes", async () => {
    const sessionCookie = await setupAdministrator();
    const created = await fetch(`${baseUrl}/api/v1/users`, {
      method: "POST",
      headers: {
        Cookie: sessionCookie,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        username: "viewer",
        password: "viewer-password",
        role: "viewer",
      }),
    });
    assert.equal(created.status, 201);
    const createdBody = (await created.json()) as {
      user: { id: string; username: string; passwordHash?: string };
    };
    assert.equal(createdBody.user.username, "viewer");
    assert.equal(createdBody.user.passwordHash, undefined);
    await assertAuditEntry(sessionCookie, "user.created", createdBody.user.id);

    const duplicate = await fetch(`${baseUrl}/api/v1/users`, {
      method: "POST",
      headers: {
        Cookie: sessionCookie,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        username: "VIEWER",
        password: "viewer-password",
        role: "viewer",
      }),
    });
    assert.equal(duplicate.status, 409);

    const list = await fetch(`${baseUrl}/api/v1/users`, {
      headers: { Cookie: sessionCookie },
    });
    assert.equal(list.status, 200);
    assert.doesNotMatch(
      JSON.stringify(await list.json()),
      /passwordHash|password_hash/,
    );

    const login = await fetch(`${baseUrl}/api/v1/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        username: "viewer",
        password: "viewer-password",
      }),
    });
    assert.equal(login.status, 200);
    assert.match(login.headers.get("set-cookie") || "", /ludock_session=/);
  });

  it("enforces role permissions and protects the final administrator", async () => {
    const sessionCookie = await setupAdministrator();
    const { id: viewerId, cookie: viewerCookie } = await createViewerSession(sessionCookie);
    const viewerList = await fetch(`${baseUrl}/api/v1/users`, {
      headers: { Cookie: viewerCookie },
    });
    assert.equal(viewerList.status, 403);

    const viewerLogs = await fetch(`${baseUrl}/api/v1/application-logs`, {
      headers: { Cookie: viewerCookie },
    });
    assert.equal(viewerLogs.status, 403);

    const unassigned = await fetch(`${baseUrl}/api/v1/servers`, {
      headers: { Cookie: viewerCookie },
    });
    assert.deepEqual(await unassigned.json(), { servers: [] });
    const unassignedStop = await fetch(
      `${baseUrl}/api/v1/servers/${managedServerId}/stop`,
      { method: "POST", headers: { Cookie: viewerCookie } },
    );
    assert.equal(unassignedStop.status, 404);
    await setServerGrants(viewerId, [{
      serverId: managedServerId,
      capabilities: ["server.view", "logs.read", "files.read"],
    }]);
    const viewerStop = await fetch(
      `${baseUrl}/api/v1/servers/${managedServerId}/stop`,
      { method: "POST", headers: { Cookie: viewerCookie } },
    );
    assert.equal(viewerStop.status, 403);
    const variantStop = await fetch(
      `${baseUrl}/api/v1/servers/${managedServerId}/STOP/`,
      {
        method: "POST",
        headers: { Cookie: viewerCookie },
      },
    );
    assert.equal(variantStop.status, 403);

    const viewerUpload = await fetch(
      `${baseUrl}/api/v1/servers/${managedServerId}/files/upload?root=root-0&path=&name=blocked.jar`,
      {
        method: "PUT",
        headers: {
          Cookie: viewerCookie,
          "Content-Type": "application/octet-stream",
        },
        body: "blocked",
      },
    );
    assert.equal(viewerUpload.status, 403);

    const users = (await (
      await fetch(`${baseUrl}/api/v1/users`, {
        headers: { Cookie: sessionCookie },
      })
    ).json()) as { users: Array<{ id: string; username: string }> };
    const admin = users.users.find((user) => user.username === "admin");
    assert.ok(admin);
    const demote = await fetch(`${baseUrl}/api/v1/users/${admin.id}`, {
      method: "PATCH",
      headers: {
        Cookie: sessionCookie,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ role: "viewer", disabled: false }),
    });
    assert.equal(demote.status, 409);
  });

  it("lets a friend start one server without granting command or file access", async () => {
    const { id: viewerId, cookie: viewerCookie } = await createViewerSession(await setupAdministrator());
    await setServerGrants(viewerId, [{
      serverId: managedServerId,
      capabilities: ["server.view", "logs.read", "files.read"],
    }]);
    const setRole = async (role: string) =>
      authorizedFetch(`/api/v1/users/${viewerId}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ role, disabled: false }),
      });
    assert.equal((await setRole("operator")).status, 200);
    await setServerGrants(viewerId, [{
      serverId: managedServerId,
      capabilities: ["server.view", "server.start", "server.stop"],
    }]);
    const friendFetch = (path: string, method = "GET") =>
      fetch(`${baseUrl}/api/v1${path}`, {
        method,
        headers: { Cookie: viewerCookie },
      });
    const listed = await friendFetch("/servers");
    const servers = (await listed.json()) as {
      servers: Array<{ permissions: string[]; fileRoots: unknown[] }>;
    };
    assert.deepEqual(servers.servers[0].permissions, [
      "server.view",
      "server.start",
      "server.stop",
    ]);
    assert.deepEqual(servers.servers[0].fileRoots, []);
    assert.equal(
      (await friendFetch(`/servers/${managedServerId}/start`, "POST")).status,
      200,
    );
    assert.equal(managedStartCalled, true);
    assert.equal(
      (await friendFetch(`/servers/${managedServerId}/restart`, "POST")).status,
      403,
    );
    assert.equal(
      (await friendFetch(`/servers/${managedServerId}/ReStArT/`, "POST"))
        .status,
      403,
    );
    assert.equal(
      (await friendFetch(`/servers/${managedServerId}/StArT/`, "POST")).status,
      200,
    );
    assert.equal(
      (await friendFetch(`/servers/${managedServerId}/files?root=root-0`))
        .status,
      403,
    );
    assert.equal(
      (
        await friendFetch(
          `/servers/${managedServerId}/FILES/?root=root-0`,
          "HEAD",
        )
      ).status,
      403,
    );
    assert.equal(
      (await friendFetch(`/servers/${managedServerId}/updates`, "POST")).status,
      403,
    );
    for (const feature of ["backups", "schedules", "update-capability"]) {
      assert.equal(
        (await friendFetch(`/servers/${managedServerId}/${feature}`)).status,
        403,
        feature,
      );
    }
    for (const feature of ["operations", "availability"]) {
      assert.equal(
        (await friendFetch(`/servers/${managedServerId}/${feature}`)).status,
        200,
        feature,
      );
    }
    for (const feature of [
      "settings/backups",
      "settings/deployment",
      "notifications",
      "diagnostics",
      "integrations",
    ]) {
      assert.equal((await friendFetch(`/${feature}`)).status, 403, feature);
    }
    const scheduleDenied = await friendFetch(
      `/servers/${managedServerId}/schedules/00000000-0000-4000-8000-000000000001`,
      "DELETE",
    );
    assert.equal(scheduleDenied.status, 403);
    await setServerGrants(viewerId, []);
    assert.equal(
      (await friendFetch(`/servers/${managedServerId}/start`, "POST")).status,
      404,
    );
    assert.equal((await setRole("viewer")).status, 200);
  });

  it("mounts feature routes with their response contracts and scoped schedules", async () => {
    const sessionCookie = await setupAdministrator();
    const expected = [
      ["/settings/backups", "settings"],
      ["/notifications", "configured"],
      ["/integrations", "integrations"],
      [`/servers/${managedServerId}/operations`, "operations"],
      [`/servers/${managedServerId}/backups`, "backups"],
      [`/servers/${managedServerId}/schedules`, "schedules"],
      [`/servers/${managedServerId}/availability`, "policy"],
      [`/servers/${managedServerId}/update-capability`, "capability"],
    ];
    for (const [path, key] of expected) {
      const response = await authorizedFetch(`/api/v1${path}`);
      assert.equal(response.status, 200, path);
      const body = (await response.json()) as Record<string, unknown>;
      assert.ok(key in body, path);
    }
    const invalidProject = await authorizedFetch("/api/v1/compose-projects", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
    assert.equal(invalidProject.status, 404);
    const created = await fetch(
      `${baseUrl}/api/v1/servers/${managedServerId}/schedules`,
      {
        method: "POST",
        headers: { Cookie: sessionCookie, "Content-Type": "application/json" },
        body: JSON.stringify({
          action: "stop",
          enabled: true,
          time: "12:00",
          days: [1],
          timezone: "UTC",
        }),
      },
    );
    assert.equal(created.status, 201);
    const { schedule } = (await created.json()) as {
      schedule: { id: string; serverId: string; ownerId: string };
    };
    assert.equal(schedule.serverId, managedServerId);
    assert.match(schedule.ownerId, /^[a-f0-9-]{36}$/);
    const deleted = await authorizedFetch(
      `/api/v1/servers/${managedServerId}/schedules/${schedule.id}`,
      { method: "DELETE" },
    );
    assert.equal(deleted.status, 200);
    assert.deepEqual(await deleted.json(), { ok: true });
  });

  it("edits and pauses schedules with revision checks and next-run responses", async () => {
    const cookie = await setupAdministrator();
    const collection = `/api/v1/servers/${managedServerId}/schedules`;
    const input = {
      action: "stop", enabled: true, time: "12:00", days: [1], timezone: "UTC",
    };
    const request = (url: string, method: string, body: unknown) => fetch(`${baseUrl}${url}`, {
      method,
      headers: { Cookie: cookie, "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    const created = await request(collection, "POST", input);
    assert.equal(created.status, 201);
    const original = scheduleResponseSchema.parse(await created.json()).schedule;
    assert.equal(original.revision, 1);
    assert.equal(typeof original.nextRunAt, "number");
    const resource = `${collection}/${original.id}`;

    const edited = await request(resource, "PUT", {
      ...input, time: "18:30", days: [2, 4], timezone: "America/Los_Angeles",
      revision: original.revision,
    });
    assert.equal(edited.status, 200);
    const updated = scheduleResponseSchema.parse(await edited.json()).schedule;
    assert.equal(updated.id, original.id);
    assert.equal(updated.ownerId, original.ownerId);
    assert.equal(updated.time, "18:30");
    assert.deepEqual(updated.days, [2, 4]);
    assert.equal(updated.timezone, "America/Los_Angeles");
    assert.equal(updated.revision, original.revision + 1);
    assert.equal(typeof updated.nextRunAt, "number");

    const stale = await request(resource, "PATCH", { enabled: false, revision: original.revision });
    assert.equal(stale.status, 409);
    const pausedResponse = await request(resource, "PATCH", { enabled: false, revision: updated.revision });
    assert.equal(pausedResponse.status, 200);
    const paused = scheduleResponseSchema.parse(await pausedResponse.json()).schedule;
    assert.equal(paused.enabled, false);
    assert.equal(paused.nextRunAt, null);
    assert.equal(paused.revision, updated.revision + 1);
    assert.equal(paused.time, updated.time);

    const resumedResponse = await request(resource, "PATCH", { enabled: true, revision: paused.revision });
    assert.equal(resumedResponse.status, 200);
    const resumed = scheduleResponseSchema.parse(await resumedResponse.json()).schedule;
    assert.equal(resumed.enabled, true);
    assert.equal(resumed.revision, paused.revision + 1);
    assert.equal(typeof resumed.nextRunAt, "number");
    const listed = await authorizedFetch(collection);
    assert.equal(listed.status, 200);
    const schedules = schedulesResponseSchema.parse(await listed.json()).schedules;
    assert.equal(schedules.length, 1);
    assert.equal(schedules[0].revision, resumed.revision);
    await assertAuditEntry(cookie, "schedule.updated", managedServerId);
    await assertAuditEntry(cookie, "schedule.paused", managedServerId);
    await assertAuditEntry(cookie, "schedule.resumed", managedServerId);
    assert.equal(managedStartCalled, false);
    assert.equal(managedStopCalled, false);
  });

  it("rejects unauthenticated and invalid schedule edits without changing the schedule", async () => {
    const cookie = await setupAdministrator();
    const collection = `/api/v1/servers/${managedServerId}/schedules`;
    const input = {
      action: "start", enabled: true, time: "12:00", days: [1], timezone: "UTC",
    };
    const headers = { Cookie: cookie, "Content-Type": "application/json" };
    const created = await fetch(`${baseUrl}${collection}`, {
      method: "POST", headers, body: JSON.stringify(input),
    });
    assert.equal(created.status, 201);
    const original = scheduleResponseSchema.parse(await created.json()).schedule;
    const url = `${baseUrl}${collection}/${original.id}`;
    for (const [method, body] of [
      ["PUT", { ...input, revision: 1 }],
      ["PATCH", { enabled: false, revision: 1 }],
    ] as const) {
      const response = await fetch(url, {
        method, headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
      });
      assert.equal(response.status, 401);
    }
    for (const [method, body] of [
      ["PUT", input],
      ["PUT", { ...input, enabled: undefined, revision: 1 }],
      ["PUT", { ...input, timezone: "Invalid/Timezone", revision: 1 }],
      ["PUT", { ...input, ownerId: crypto.randomUUID(), revision: 1 }],
      ["PATCH", { enabled: false }],
      ["PATCH", { enabled: false, action: "stop", revision: 1 }],
      ["PATCH", { enabled: "false", revision: 1 }],
    ] as const) {
      const response = await fetch(url, { method, headers, body: JSON.stringify(body) });
      assert.equal(response.status, 400);
    }
    const oversized = await fetch(url, {
      method: "PUT", headers,
      body: JSON.stringify({ ...input, revision: 1, padding: "x".repeat(4096) }),
    });
    assert.equal(oversized.status, 413);
    const listed = schedulesResponseSchema.parse(await (await authorizedFetch(collection)).json());
    assert.equal(listed.schedules[0].revision, 1);
    assert.equal(listed.schedules[0].enabled, true);
    assert.equal(listed.schedules[0].action, "start");
  });

  it("creates paused schedules and serves the latest persisted run outcome", async () => {
    const cookie = await setupAdministrator();
    const collection = `/api/v1/servers/${managedServerId}/schedules`;
    const headers = { Cookie: cookie, "Content-Type": "application/json" };
    const due = Date.parse("2026-09-11T12:00:00Z");
    const response = await fetch(`${baseUrl}${collection}`, {
      method: "POST", headers,
      body: JSON.stringify({
        action: "start", enabled: false, time: "12:00", days: [0, 1, 2, 3, 4, 5, 6], timezone: "UTC",
      }),
    });
    assert.equal(response.status, 201);
    const original = scheduleResponseSchema.parse(await response.json()).schedule;
    assert.equal(original.nextRunAt, null);
    assert.equal(original.nextRunUnavailableReason, null);
    assert.equal(original.lastOperation, null);
    assert.equal(original.lastRunAt, null);
    const readSchedule = async () => {
      const listed = await authorizedFetch(collection);
      assert.equal(listed.status, 200);
      return schedulesResponseSchema.parse(await listed.json()).schedules[0];
    };
    runSchedules(due);
    assert.equal((await readSchedule()).lastOperation, null);
    assert.equal(getDatabase().prepare("SELECT COUNT(*) AS count FROM operations").get()?.count, 0);

    const resumed = await fetch(`${baseUrl}${collection}/${original.id}`, {
      method: "PATCH", headers,
      body: JSON.stringify({ enabled: true, revision: original.revision }),
    });
    assert.equal(resumed.status, 200);
    runSchedules(due);
    const queued = await readSchedule();
    assert.equal(queued.lastRunAt, due);
    assert.equal(queued.lastOperation?.status, "queued");
    assert.equal(queued.lastOperation?.serverId, managedServerId);
    assert.ok(queued.lastOperation);
    for (const key of ["actorId", "input", "recovery", "bindingRevision"])
      assert.equal(key in queued.lastOperation, false);
    for (const status of ["running", "succeeded", "failed", "interrupted"] as const) {
      getDatabase().prepare("UPDATE operations SET status=?,phase=?,error=? WHERE id=?")
        .run(status, status, status === "failed" ? "Fixture action failed" : null, queued.lastOperation.id);
      const current = await readSchedule();
      assert.equal(current.lastOperation?.status, status);
      assert.equal(current.lastOperation?.id, queued.lastOperation.id);
      assert.equal(current.lastRunAt, due);
    }
    getDatabase().prepare("UPDATE operations SET status='running' WHERE id=?").run(queued.lastOperation.id);
    runSchedules(due + 86_400_000);
    const skipped = await readSchedule();
    assert.equal(skipped.lastOperation, null);
    assert.equal(skipped.lastRunAt, due + 86_400_000);
    assert.match(skipped.lastResult!, /^Skipped:/);
    assert.equal(managedStartCalled, false);
    assert.equal(managedStopCalled, false);
  });

  it("explains unavailable schedule previews while preserving schedule access boundaries", async () => {
    const { id: ownerId, cookie } = await createViewerSession(await setupAdministrator());
    const changeOwner = (disabled: boolean) => authorizedFetch(`/api/v1/users/${ownerId}`, {
      method: "PATCH", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ role: "operator", disabled }),
    });
    assert.equal((await changeOwner(false)).status, 200);
    await setServerGrants(ownerId, [{
      serverId: managedServerId, capabilities: ["server.view", "schedules.manage", "server.start"],
    }]);
    const collection = `/api/v1/servers/${managedServerId}/schedules`;
    const created = await fetch(`${baseUrl}${collection}`, {
      method: "POST", headers: { Cookie: cookie, "Content-Type": "application/json" },
      body: JSON.stringify({ action: "start", enabled: true, time: "12:00", days: [1], timezone: "UTC" }),
    });
    assert.equal(created.status, 201);
    assert.equal(scheduleResponseSchema.parse(await created.json()).schedule.nextRunUnavailableReason, null);
    const readReason = async () => {
      const response = await authorizedFetch(collection);
      assert.equal(response.status, 200);
      const [schedule] = schedulesResponseSchema.parse(await response.json()).schedules;
      assert.equal(schedule.nextRunAt, null);
      return schedule.nextRunUnavailableReason;
    };
    await setServerGrants(ownerId, [{ serverId: managedServerId, capabilities: ["server.view", "schedules.manage"] }]);
    assert.equal(await readReason(), "action_access_removed");
    await setServerGrants(ownerId, [{ serverId: managedServerId, capabilities: ["server.view"] }]);
    assert.equal(await readReason(), "owner_access_removed");
    assert.equal((await fetch(`${baseUrl}${collection}`, { headers: { Cookie: cookie } })).status, 403);
    assert.equal((await changeOwner(true)).status, 200);
    assert.equal(await readReason(), "owner_disabled");
    assert.equal((await fetch(`${baseUrl}${collection}`, { headers: { Cookie: cookie } })).status, 401);
  });

  it("shows deployment root choices only to administrators without exposing other environment settings", async () => {
    const { cookie: viewerCookie } = await createViewerSession(await setupAdministrator());
    const originalBackupRoots = process.env.LUDOCK_BACKUP_ROOTS;
    const originalComposeRoots = process.env.LUDOCK_COMPOSE_ROOTS;
    try {
      delete process.env.LUDOCK_BACKUP_ROOTS;
      delete process.env.LUDOCK_COMPOSE_ROOTS;
      const defaults = await authorizedFetch("/api/v1/settings/deployment");
      assert.equal(defaults.status, 200);
      assert.deepEqual(await defaults.json(), {
        backupRoots: [], composeRoots: [], composeAvailable: false,
      });

      process.env.LUDOCK_BACKUP_ROOTS = ["/backups", "/archive", "/backups"].join(path.delimiter);
      process.env.LUDOCK_COMPOSE_ROOTS = ["/srv/games", "/srv/games"].join(path.delimiter);
      composeAvailability.mockResolvedValue(true);
      const response = await authorizedFetch("/api/v1/settings/deployment");
      assert.equal(response.status, 200);
      const body = await response.json() as Record<string, unknown>;
      assert.deepEqual(body.backupRoots, ["/backups", "/archive"]);
      assert.deepEqual(body.composeRoots, ["/srv/games"]);
      assert.equal(body.composeAvailable, true);
      assert.deepEqual(Object.keys(body).sort(), ["backupRoots", "composeAvailable", "composeRoots"]);

      for (const [headers, expected] of [
        [{}, 401], [{ Cookie: viewerCookie }, 403],
      ] as const) {
        const denied = await fetch(`${baseUrl}/api/v1/settings/deployment`, { headers });
        assert.equal(denied.status, expected);
        assert.doesNotMatch(JSON.stringify(await denied.json()), /\/backups|\/archive|\/srv\/games/);
      }
    } finally {
      if (originalBackupRoots === undefined) delete process.env.LUDOCK_BACKUP_ROOTS;
      else process.env.LUDOCK_BACKUP_ROOTS = originalBackupRoots;
      if (originalComposeRoots === undefined) delete process.env.LUDOCK_COMPOSE_ROOTS;
      else process.env.LUDOCK_COMPOSE_ROOTS = originalComposeRoots;
    }
  });

  it("keeps a lifecycle lock until Docker settles after the browser disconnects", async () => {
    let finishStop!: () => void;
    stopGate = new Promise<void>((resolve) => {
      finishStop = resolve;
    });
    managedStopCalled = false;
    const controller = new AbortController();
    const first = authorizedFetch(`/api/v1/servers/${managedServerId}/stop`, {
      method: "POST",
      signal: controller.signal,
    }).catch(() => null);
    try {
      for (let i = 0; i < 100 && !managedStopCalled; i++)
        await new Promise((resolve) => setTimeout(resolve, 5));
      assert.equal(managedStopCalled, true);
      controller.abort();
      await first;
      const conflict = await authorizedFetch(
        `/api/v1/servers/${managedServerId}/stop`,
        { method: "POST" },
      );
      assert.equal(conflict.status, 409);
    } finally {
      finishStop();
      stopGate = undefined;
    }
    await new Promise((resolve) => setTimeout(resolve, 20));
    assert.equal(
      (
        await authorizedFetch(`/api/v1/servers/${managedServerId}/stop`, {
          method: "POST",
        })
      ).status,
      200,
    );
  });

  it("serves structured redacted Ludock logs to administrators", async () => {
    const sessionCookie = await setupAdministrator();
    testLogger.warn("diagnostic marker", {
      requestId: "log-test-request",
      apiToken: "must-not-reach-browser",
    });
    const response = await fetch(
      `${baseUrl}/api/v1/application-logs?limit=20`,
      {
        headers: { Cookie: sessionCookie },
      },
    );
    assert.equal(response.status, 200);
    const body = (await response.json()) as {
      generation: string;
      entries: Array<{
        level: string;
        component: string;
        message: string;
        context?: Record<string, unknown>;
      }>;
    };
    assert.ok(body.generation);
    const marker = body.entries.find(
      (entry) =>
        entry.component === "http-test" && entry.message.includes("diagnostic"),
    );
    assert.ok(marker);
    assert.equal(marker.level, "warn");
    assert.equal(marker.context?.apiToken, "[REDACTED]");
    assert.doesNotMatch(JSON.stringify(marker), /must-not-reach-browser/);
    assert.equal(marker.context?.requestId, "log-test-request");
  });

  it("does not create debug log entries for log-viewer polling", async () => {
    const sessionCookie = await setupAdministrator();
    const previousLevel = process.env.LOG_LEVEL;
    process.env.LOG_LEVEL = "debug";
    try {
      await fetch(`${baseUrl}/api/v1/application-logs`, {
        headers: { Cookie: sessionCookie },
      });
      await new Promise((resolve) => setImmediate(resolve));
      const response = await fetch(`${baseUrl}/api/v1/application-logs`, {
        headers: { Cookie: sessionCookie },
      });
      const body = (await response.json()) as {
        entries: Array<{
          component: string;
          message: string;
          context?: Record<string, unknown>;
        }>;
      };
      assert.equal(
        body.entries.some(
          (entry) =>
            entry.component === "api" &&
            entry.message === "HTTP request completed" &&
            entry.context?.path === "/api/v1/application-logs",
        ),
        false,
      );
    } finally {
      if (previousLevel === undefined) delete process.env.LOG_LEVEL;
      else process.env.LOG_LEVEL = previousLevel;
    }
  });

  it("lists and revokes account sessions", async () => {
    const sessionCookie = await setupAdministrator();
    const { cookie: viewerCookie } = await createViewerSession(sessionCookie);
    const response = await fetch(`${baseUrl}/api/v1/account/sessions`, {
      headers: { Cookie: viewerCookie },
    });
    assert.equal(response.status, 200);
    const body = (await response.json()) as {
      sessions: Array<{ id: string; current: boolean }>;
    };
    assert.equal(body.sessions.length, 1);
    assert.equal(body.sessions[0].current, true);

    const revoked = await fetch(
      `${baseUrl}/api/v1/account/sessions/${body.sessions[0].id}`,
      { method: "DELETE", headers: { Cookie: viewerCookie } },
    );
    assert.equal(revoked.status, 200);
    await assertAuditEntry(sessionCookie, "auth.session.revoked", body.sessions[0].id);
    assert.equal(
      (
        await fetch(`${baseUrl}/api/v1/auth/me`, {
          headers: { Cookie: viewerCookie },
        })
      ).status,
      401,
    );
  });

  it("deletes other accounts but not the current account", async () => {
    const sessionCookie = await setupAdministrator();
    const { id: viewerId } = await createViewerSession(sessionCookie);
    const users = (await (
      await fetch(`${baseUrl}/api/v1/users`, {
        headers: { Cookie: sessionCookie },
      })
    ).json()) as { users: Array<{ id: string; username: string }> };
    const admin = users.users.find((user) => user.username === "admin");
    assert.ok(admin);

    const selfDelete = await fetch(`${baseUrl}/api/v1/users/${admin.id}`, {
      method: "DELETE",
      headers: { Cookie: sessionCookie },
    });
    assert.equal(selfDelete.status, 409);

    const deleteViewer = await fetch(`${baseUrl}/api/v1/users/${viewerId}`, {
      method: "DELETE",
      headers: { Cookie: sessionCookie },
    });
    assert.equal(deleteViewer.status, 200);
    await assertAuditEntry(sessionCookie, "user.deleted", viewerId);
  });

  it("changes passwords, revokes old sessions, and preserves the current login", async () => {
    const sessionCookie = await setupAdministrator();
    const response = await fetch(`${baseUrl}/api/v1/account/change-password`, {
      method: "POST",
      headers: {
        Cookie: sessionCookie,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        currentPassword: "integration-password",
        newPassword: "replacement-password",
      }),
    });
    assert.equal(response.status, 200);
    const replacementCookie = (response.headers.get("set-cookie") || "").split(
      ";",
    )[0];
    assert.match(replacementCookie, /ludock_session=/);
    assert.equal(
      (
        await fetch(`${baseUrl}/api/v1/auth/me`, {
          headers: { Cookie: sessionCookie },
        })
      ).status,
      401,
    );
    assert.equal(
      (await fetch(`${baseUrl}/api/v1/auth/me`, {
        headers: { Cookie: replacementCookie },
      })).status,
      200,
    );

    const oldLogin = await fetch(`${baseUrl}/api/v1/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        username: "admin",
        password: "integration-password",
      }),
    });
    assert.equal(oldLogin.status, 401);
    const newLogin = await fetch(`${baseUrl}/api/v1/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        username: "admin",
        password: "replacement-password",
      }),
    });
    assert.equal(newLogin.status, 200);
    await assertAuditEntry(replacementCookie, "auth.password.changed");
  });

  it("lists managed containers without unrelated labels", async () => {
    const response = await authorizedFetch("/api/v1/servers");
    assert.equal(response.status, 200);
    const body = (await response.json()) as {
      servers: Array<{ id: string; labels: Record<string, string> }>;
    };

    assert.equal(body.servers.length, 1);
    assert.equal(body.servers[0].id, managedServerId);
    assert.match(managedServerId, /^[0-9a-f-]{36}$/);
    assert.notEqual(managedServerId, managedInfo.Id);
    assert.deepEqual(body.servers[0].labels, {});
  });

  it("allows managed lifecycle actions", async () => {
    const response = await authorizedFetch(
      `/api/v1/servers/${managedServerId}/stop`,
      { method: "POST" },
    );
    assert.equal(response.status, 200);
    assert.equal(managedStopCalled, true);
  });

  it("rejects unmanaged lifecycle actions", async () => {
    const response = await authorizedFetch(
      "/api/v1/servers/unmanaged-container-id/stop",
      { method: "POST" },
    );
    assert.equal(response.status, 404);
    assert.equal(unmanagedStopCalled, false);
  });

  it("throttles repeated login failures", async () => {
    await setupAdministrator();
    const login = (password: string) =>
      fetch(`${baseUrl}/api/v1/auth/login`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username: "admin", password }),
      });

    assert.equal((await login("definitely-incorrect")).status, 401);
    assert.equal((await login("integration-password")).status, 200);
    for (let attempt = 0; attempt < 5; attempt += 1) {
      assert.equal((await login("definitely-incorrect")).status, 401);
    }
    assert.equal((await login("integration-password")).status, 429);
  });

  it("throttles current-password guessing on password change", async () => {
    const sessionCookie = await setupAdministrator();
    const guess = () =>
      fetch(`${baseUrl}/api/v1/account/change-password`, {
        method: "POST",
        headers: { Cookie: sessionCookie, "Content-Type": "application/json" },
        body: JSON.stringify({
          currentPassword: "not-the-current-password",
          newPassword: "another-replacement-password",
        }),
      });

    for (let attempt = 0; attempt < 5; attempt += 1) {
      assert.equal((await guess()).status, 400);
    }
    assert.equal((await guess()).status, 429);

    const correct = await fetch(`${baseUrl}/api/v1/account/change-password`, {
      method: "POST",
      headers: { Cookie: sessionCookie, "Content-Type": "application/json" },
      body: JSON.stringify({
        currentPassword: "integration-password",
        newPassword: "another-replacement-password",
      }),
    });
    assert.equal(correct.status, 429);
  });
});
