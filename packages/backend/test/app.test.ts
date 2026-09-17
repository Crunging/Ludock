import { serve, type Server } from "bun";
import { expect, afterAll, afterEach, beforeAll, beforeEach, describe, it, spyOn } from "bun:test";
import path from "node:path";
import { auditResponseSchema, availabilityResponseSchema, scheduleResponseSchema, schedulesResponseSchema, type ServerGrantInput } from "@ludock/shared";
import { DockerApiError } from "../src/docker-transport.js";

process.env.LUDOCK_DB_PATH = ":memory:";
process.env.LUDOCK_API_TOKEN = "integration-api-secret-0123456789abcdef";
process.env.MAX_UPLOAD_SIZE = "1.5 KiB";
process.env.MAX_UPLOAD_BYTES = "1";
process.env.LUDOCK_SETUP_CODE = "integration-setup-code-0123456789abcdef";

const [{ createApp }, { getDockerInstance }, { createLogger }, { closeDatabase, getDatabase }, { SetupWindow }, compose, { runSchedules }, { setIntentionalStop }] =
  await Promise.all([
    import("../src/app.js"),
    import("../src/docker.js"),
    import("../src/logger.js"),
    import("../src/database.js"),
    import("../src/auth.js"),
    import("../src/compose.js"),
    import("../src/schedules.js"),
    import("../src/monitoring.js"),
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
let lifecycleError: DockerApiError | undefined;
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
        if (lifecycleError) throw lifecycleError;
      },
      stop: async () => {
        if (managed) managedStopCalled = true;
        else unmanagedStopCalled = true;
        if (stopGate) await stopGate;
        if (lifecycleError) throw lifecycleError;
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
  lifecycleError = undefined;
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
  expect(response.status).toBe(201);
  const cookie = (response.headers.get("set-cookie") || "").split(";")[0];
  expect(cookie).toMatch(/ludock_session=/);
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
  expect(created.status).toBe(201);
  const { user } = await created.json() as { user: { id: string } };
  const login = await fetch(`${baseUrl}/api/v1/auth/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username: "viewer", password: "viewer-password" }),
  });
  expect(login.status).toBe(200);
  const cookie = (login.headers.get("set-cookie") || "").split(";")[0];
  expect(cookie).toMatch(/ludock_session=/);
  return { id: user.id, cookie };
}

async function setServerGrants(userId: string, grants: ServerGrantInput[]) {
  const response = await authorizedFetch(`/api/v1/users/${userId}/server-grants`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ grants }),
  });
  expect(response.status).toBe(200);
}

async function assertAuditEntry(cookie: string, action: string, targetId?: string) {
  const response = await fetch(`${baseUrl}/api/v1/audit`, {
    headers: { Cookie: cookie },
  });
  expect(response.status).toBe(200);
  const { entries } = await response.json() as {
    entries: Array<{ action: string; targetId: string | null }>;
  };
  expect(entries.some((entry) =>
    entry.action === action && (targetId === undefined || entry.targetId === targetId),
  ), `Missing ${action} audit entry`).toBeTruthy();
}

describe("HTTP application", () => {
  it("serves public status and health endpoints", async () => {
    const authStatus = await fetch(`${baseUrl}/api/v1/auth/status`);
    expect(authStatus.status).toBe(200);
    const status = (await authStatus.json()) as {
      setupRequired: boolean;
      setupLocked: boolean;
      setupExpiresAt: number | null;
      setupRemainingMs: number | null;
      authenticated: boolean;
      user: unknown;
    };
    expect({
        ...status,
        setupExpiresAt: typeof status.setupExpiresAt,
        setupRemainingMs: typeof status.setupRemainingMs,
      }).toStrictEqual({
        setupRequired: true,
        setupLocked: false,
        setupExpiresAt: "number",
        setupRemainingMs: "number",
        authenticated: false,
        user: null,
      });
    expect(authStatus.headers.get("cache-control")).toBe("no-store");

    const proxiedStatus = await fetch(`${baseUrl}/api/v1/auth/status`, {
      headers: {
        Cookie: "authelia_session=opaque%token; unrelated=value",
      },
    });
    expect(proxiedStatus.status).toBe(200);
    expect(((await proxiedStatus.json()) as { setupRequired: boolean })
        .setupRequired).toBe(true);

    const health = await fetch(`${baseUrl}/api/v1/health`);
    expect(health.status).toBe(200);
    expect(await health.json()).toStrictEqual({
      status: "ok",
      docker: "connected",
      database: "connected",
    });
    expect(health.headers.get("x-content-type-options")).toBe("nosniff");
    expect(health.headers.get("x-frame-options")).toBe("DENY");
    expect(health.headers.get("cross-origin-opener-policy")).toBe("same-origin");
    expect(health.headers.get("content-security-policy") || "").toMatch(/default-src 'self'/);

    const proxiedHealth = await fetch(`${baseUrl}/api/v1/health`, {
      headers: { "X-Forwarded-Proto": "https" },
    });
    expect(proxiedHealth.headers.get("strict-transport-security")).toBe("max-age=31536000; includeSubDomains");
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
    expect(response.status).toBe(201);
    const setCookie = response.headers.get("set-cookie") || "";
    expect(setCookie).toMatch(/ludock_session=/);
    expect(setCookie).toMatch(/HttpOnly/i);
    expect(setCookie).toMatch(/SameSite=Strict/i);
    expect(setCookie).toMatch(/Secure/i);
    const sessionCookie = setCookie.split(";")[0];

    const me = await fetch(`${baseUrl}/api/v1/auth/me`, {
      headers: { Cookie: sessionCookie },
    });
    expect(me.status).toBe(200);
    expect(((await me.json()) as { user: { username: string } }).user.username).toBe("admin");
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
    expect(invalid.status).toBe(401);
    expect(await invalid.json()).toStrictEqual({
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
    expect(valid.status).toBe(200);
    expect(valid.headers.get("set-cookie") || "").toMatch(/HttpOnly/i);
  });

  it("protects authenticated endpoints", async () => {
    expect((await fetch(`${baseUrl}/api/v1/auth/me`)).status).toBe(401);
    expect((
        await fetch(`${baseUrl}/api/v1/auth/me`, {
          headers: { Authorization: "Bearer wrong" },
        })
      ).status).toBe(401);
    expect((await authorizedFetch("/api/v1/auth/me")).status).toBe(200);
    const missing = await authorizedFetch("/api/v1/does-not-exist");
    expect(missing.status).toBe(404);
    expect(await missing.json()).toStrictEqual({ error: "API endpoint not found" });
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
    expect(response.status).toBe(403);

    const fetchMetadataResponse = await authorizedFetch("/api/v1/auth/logout", {
      method: "POST",
      headers: { "Sec-Fetch-Site": "cross-site" },
    });
    expect(fetchMetadataResponse.status).toBe(403);
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
    expect(response.status).toBe(415);
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
    expect(oversized.status).toBe(413);
    expect(await oversized.json()).toStrictEqual({
      error: "File exceeds the upload size limit of 1.5 KiB",
    });

    // The exact limit passes the size gate and reaches this fixture's missing
    // file root, despite MAX_UPLOAD_BYTES being configured as only one byte.
    const boundary = await upload(1536);
    expect(boundary.status).toBe(404);
    expect(await boundary.json()).toStrictEqual({ error: "File root not found", code: "ROOT_NOT_FOUND" });
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
    expect(created.status).toBe(201);
    const createdBody = (await created.json()) as {
      user: { id: string; username: string; passwordHash?: string };
    };
    expect(createdBody.user.username).toBe("viewer");
    expect(createdBody.user.passwordHash).toBe(undefined);
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
    expect(duplicate.status).toBe(409);

    const list = await fetch(`${baseUrl}/api/v1/users`, {
      headers: { Cookie: sessionCookie },
    });
    expect(list.status).toBe(200);
    expect(JSON.stringify(await list.json())).not.toMatch(/passwordHash|password_hash/);

    const login = await fetch(`${baseUrl}/api/v1/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        username: "viewer",
        password: "viewer-password",
      }),
    });
    expect(login.status).toBe(200);
    expect(login.headers.get("set-cookie") || "").toMatch(/ludock_session=/);
  });

  it("enforces role permissions and protects the final administrator", async () => {
    const sessionCookie = await setupAdministrator();
    const { id: viewerId, cookie: viewerCookie } = await createViewerSession(sessionCookie);
    const viewerList = await fetch(`${baseUrl}/api/v1/users`, {
      headers: { Cookie: viewerCookie },
    });
    expect(viewerList.status).toBe(403);

    const viewerLogs = await fetch(`${baseUrl}/api/v1/application-logs`, {
      headers: { Cookie: viewerCookie },
    });
    expect(viewerLogs.status).toBe(403);

    const unassigned = await fetch(`${baseUrl}/api/v1/servers`, {
      headers: { Cookie: viewerCookie },
    });
    expect(await unassigned.json()).toStrictEqual({ servers: [] });
    const unassignedStop = await fetch(
      `${baseUrl}/api/v1/servers/${managedServerId}/stop`,
      { method: "POST", headers: { Cookie: viewerCookie } },
    );
    expect(unassignedStop.status).toBe(404);
    await setServerGrants(viewerId, [{
      serverId: managedServerId,
      capabilities: ["server.view", "logs.read", "files.read"],
    }]);
    const viewerStop = await fetch(
      `${baseUrl}/api/v1/servers/${managedServerId}/stop`,
      { method: "POST", headers: { Cookie: viewerCookie } },
    );
    expect(viewerStop.status).toBe(403);
    const variantStop = await fetch(
      `${baseUrl}/api/v1/servers/${managedServerId}/STOP/`,
      {
        method: "POST",
        headers: { Cookie: viewerCookie },
      },
    );
    expect(variantStop.status).toBe(403);

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
    expect(viewerUpload.status).toBe(403);

    const users = (await (
      await fetch(`${baseUrl}/api/v1/users`, {
        headers: { Cookie: sessionCookie },
      })
    ).json()) as { users: Array<{ id: string; username: string }> };
    const admin = users.users.find((user) => user.username === "admin");
    expect(admin).toBeTruthy();
    const demote = await fetch(`${baseUrl}/api/v1/users/${admin.id}`, {
      method: "PATCH",
      headers: {
        Cookie: sessionCookie,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ role: "viewer", disabled: false }),
    });
    expect(demote.status).toBe(409);
  });

  for (const action of ["start", "stop"] as const) {
    for (const statusCode of [304, 500]) {
      it(`records monitoring and audit state for HTTP ${action} after Docker returns ${statusCode}`, async () => {
        const initiallyStopped = action === "start";
        setIntentionalStop(managedServerId, initiallyStopped);
        lifecycleError = new DockerApiError(statusCode);

        const response = await authorizedFetch(`/api/v1/servers/${managedServerId}/${action}`, {
          method: "POST",
        });
        expect(response.status).toBe(statusCode === 304 ? 200 : 500);
        expect(await response.json()).toStrictEqual(statusCode === 304
          ? { ok: true }
          : { error: `Failed to ${action} container` });
        expect(action === "start" ? managedStartCalled : managedStopCalled).toBe(true);

        const availability = await authorizedFetch(`/api/v1/servers/${managedServerId}/availability`);
        expect(availability.status).toBe(200);
        expect(availabilityResponseSchema.parse(await availability.json()).state.intentionallyStopped)
          .toBe(statusCode === 304 ? action === "stop" : initiallyStopped);

        const audit = await authorizedFetch("/api/v1/audit");
        expect(audit.status).toBe(200);
        const entries = auditResponseSchema.parse(await audit.json()).entries.filter((entry) =>
          entry.action === `server.${action}` && entry.targetId === managedServerId);
        expect(entries).toHaveLength(statusCode === 304 ? 1 : 0);
      });
    }
  }

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
    expect((await setRole("operator")).status).toBe(200);
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
    expect(servers.servers[0].permissions).toStrictEqual([
      "server.view",
      "server.start",
      "server.stop",
    ]);
    expect(servers.servers[0].fileRoots).toStrictEqual([]);
    expect((await friendFetch(`/servers/${managedServerId}/start`, "POST")).status).toBe(200);
    expect(managedStartCalled).toBe(true);
    expect((await friendFetch(`/servers/${managedServerId}/restart`, "POST")).status).toBe(403);
    expect((await friendFetch(`/servers/${managedServerId}/ReStArT/`, "POST"))
        .status).toBe(403);
    expect((await friendFetch(`/servers/${managedServerId}/StArT/`, "POST")).status).toBe(200);
    expect((await friendFetch(`/servers/${managedServerId}/files?root=root-0`))
        .status).toBe(403);
    expect((
        await friendFetch(
          `/servers/${managedServerId}/FILES/?root=root-0`,
          "HEAD",
        )
      ).status).toBe(403);
    expect((await friendFetch(`/servers/${managedServerId}/updates`, "POST")).status).toBe(403);
    for (const feature of ["backups", "schedules", "update-capability"]) {
      expect((await friendFetch(`/servers/${managedServerId}/${feature}`)).status, feature).toBe(403);
    }
    for (const feature of ["operations", "availability"]) {
      expect((await friendFetch(`/servers/${managedServerId}/${feature}`)).status, feature).toBe(200);
    }
    for (const feature of [
      "settings/backups",
      "settings/deployment",
      "notifications",
      "diagnostics",
      "integrations",
    ]) {
      expect((await friendFetch(`/${feature}`)).status, feature).toBe(403);
    }
    const scheduleDenied = await friendFetch(
      `/servers/${managedServerId}/schedules/00000000-0000-4000-8000-000000000001`,
      "DELETE",
    );
    expect(scheduleDenied.status).toBe(403);
    await setServerGrants(viewerId, []);
    expect((await friendFetch(`/servers/${managedServerId}/start`, "POST")).status).toBe(404);
    expect((await setRole("viewer")).status).toBe(200);
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
      expect(response.status, path).toBe(200);
      const body = (await response.json()) as Record<string, unknown>;
      expect(key in body, path).toBeTruthy();
    }
    const invalidProject = await authorizedFetch("/api/v1/compose-projects", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
    expect(invalidProject.status).toBe(404);
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
    expect(created.status).toBe(201);
    const { schedule } = (await created.json()) as {
      schedule: { id: string; serverId: string; ownerId: string };
    };
    expect(schedule.serverId).toBe(managedServerId);
    expect(schedule.ownerId).toMatch(/^[a-f0-9-]{36}$/);
    const deleted = await authorizedFetch(
      `/api/v1/servers/${managedServerId}/schedules/${schedule.id}`,
      { method: "DELETE" },
    );
    expect(deleted.status).toBe(200);
    expect(await deleted.json()).toStrictEqual({ ok: true });
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
    expect(created.status).toBe(201);
    const original = scheduleResponseSchema.parse(await created.json()).schedule;
    expect(original.revision).toBe(1);
    expect(typeof original.nextRunAt).toBe("number");
    const resource = `${collection}/${original.id}`;

    const edited = await request(resource, "PUT", {
      ...input, time: "18:30", days: [2, 4], timezone: "America/Los_Angeles",
      revision: original.revision,
    });
    expect(edited.status).toBe(200);
    const updated = scheduleResponseSchema.parse(await edited.json()).schedule;
    expect(updated.id).toBe(original.id);
    expect(updated.ownerId).toBe(original.ownerId);
    expect(updated.time).toBe("18:30");
    expect(updated.days).toStrictEqual([2, 4]);
    expect(updated.timezone).toBe("America/Los_Angeles");
    expect(updated.revision).toBe(original.revision + 1);
    expect(typeof updated.nextRunAt).toBe("number");

    const stale = await request(resource, "PATCH", { enabled: false, revision: original.revision });
    expect(stale.status).toBe(409);
    const pausedResponse = await request(resource, "PATCH", { enabled: false, revision: updated.revision });
    expect(pausedResponse.status).toBe(200);
    const paused = scheduleResponseSchema.parse(await pausedResponse.json()).schedule;
    expect(paused.enabled).toBe(false);
    expect(paused.nextRunAt).toBe(null);
    expect(paused.revision).toBe(updated.revision + 1);
    expect(paused.time).toBe(updated.time);

    const resumedResponse = await request(resource, "PATCH", { enabled: true, revision: paused.revision });
    expect(resumedResponse.status).toBe(200);
    const resumed = scheduleResponseSchema.parse(await resumedResponse.json()).schedule;
    expect(resumed.enabled).toBe(true);
    expect(resumed.revision).toBe(paused.revision + 1);
    expect(typeof resumed.nextRunAt).toBe("number");
    const listed = await authorizedFetch(collection);
    expect(listed.status).toBe(200);
    const schedules = schedulesResponseSchema.parse(await listed.json()).schedules;
    expect(schedules.length).toBe(1);
    expect(schedules[0].revision).toBe(resumed.revision);
    await assertAuditEntry(cookie, "schedule.updated", managedServerId);
    await assertAuditEntry(cookie, "schedule.paused", managedServerId);
    await assertAuditEntry(cookie, "schedule.resumed", managedServerId);
    expect(managedStartCalled).toBe(false);
    expect(managedStopCalled).toBe(false);
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
    expect(created.status).toBe(201);
    const original = scheduleResponseSchema.parse(await created.json()).schedule;
    const url = `${baseUrl}${collection}/${original.id}`;
    for (const [method, body] of [
      ["PUT", { ...input, revision: 1 }],
      ["PATCH", { enabled: false, revision: 1 }],
    ] as const) {
      const response = await fetch(url, {
        method, headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
      });
      expect(response.status).toBe(401);
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
      expect(response.status).toBe(400);
    }
    const oversized = await fetch(url, {
      method: "PUT", headers,
      body: JSON.stringify({ ...input, revision: 1, padding: "x".repeat(4096) }),
    });
    expect(oversized.status).toBe(413);
    const listed = schedulesResponseSchema.parse(await (await authorizedFetch(collection)).json());
    expect(listed.schedules[0].revision).toBe(1);
    expect(listed.schedules[0].enabled).toBe(true);
    expect(listed.schedules[0].action).toBe("start");
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
    expect(response.status).toBe(201);
    const original = scheduleResponseSchema.parse(await response.json()).schedule;
    expect(original.nextRunAt).toBe(null);
    expect(original.nextRunUnavailableReason).toBe(null);
    expect(original.lastOperation).toBe(null);
    expect(original.lastRunAt).toBe(null);
    const readSchedule = async () => {
      const listed = await authorizedFetch(collection);
      expect(listed.status).toBe(200);
      return schedulesResponseSchema.parse(await listed.json()).schedules[0];
    };
    runSchedules(due);
    expect((await readSchedule()).lastOperation).toBe(null);
    expect(getDatabase().prepare("SELECT COUNT(*) AS count FROM operations").get()?.count).toBe(0);

    const resumed = await fetch(`${baseUrl}${collection}/${original.id}`, {
      method: "PATCH", headers,
      body: JSON.stringify({ enabled: true, revision: original.revision }),
    });
    expect(resumed.status).toBe(200);
    runSchedules(due);
    const queued = await readSchedule();
    expect(queued.lastRunAt).toBe(due);
    expect(queued.lastOperation?.status).toBe("queued");
    expect(queued.lastOperation?.serverId).toBe(managedServerId);
    expect(queued.lastOperation).toBeTruthy();
    for (const key of ["actorId", "input", "recovery", "bindingRevision"])
      expect(key in queued.lastOperation).toBe(false);
    for (const status of ["running", "succeeded", "failed", "interrupted"] as const) {
      getDatabase().prepare("UPDATE operations SET status=?,phase=?,error=? WHERE id=?")
        .run(status, status, status === "failed" ? "Fixture action failed" : null, queued.lastOperation.id);
      const current = await readSchedule();
      expect(current.lastOperation?.status).toBe(status);
      expect(current.lastOperation?.id).toBe(queued.lastOperation.id);
      expect(current.lastRunAt).toBe(due);
    }
    getDatabase().prepare("UPDATE operations SET status='running' WHERE id=?").run(queued.lastOperation.id);
    runSchedules(due + 86_400_000);
    const skipped = await readSchedule();
    expect(skipped.lastOperation).toBe(null);
    expect(skipped.lastRunAt).toBe(due + 86_400_000);
    expect(skipped.lastResult!).toMatch(/^Skipped:/);
    expect(managedStartCalled).toBe(false);
    expect(managedStopCalled).toBe(false);
  });

  it("explains unavailable schedule previews while preserving schedule access boundaries", async () => {
    const { id: ownerId, cookie } = await createViewerSession(await setupAdministrator());
    const changeOwner = (disabled: boolean) => authorizedFetch(`/api/v1/users/${ownerId}`, {
      method: "PATCH", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ role: "operator", disabled }),
    });
    expect((await changeOwner(false)).status).toBe(200);
    await setServerGrants(ownerId, [{
      serverId: managedServerId, capabilities: ["server.view", "schedules.manage", "server.start"],
    }]);
    const collection = `/api/v1/servers/${managedServerId}/schedules`;
    const created = await fetch(`${baseUrl}${collection}`, {
      method: "POST", headers: { Cookie: cookie, "Content-Type": "application/json" },
      body: JSON.stringify({ action: "start", enabled: true, time: "12:00", days: [1], timezone: "UTC" }),
    });
    expect(created.status).toBe(201);
    expect(scheduleResponseSchema.parse(await created.json()).schedule.nextRunUnavailableReason).toBe(null);
    const readReason = async () => {
      const response = await authorizedFetch(collection);
      expect(response.status).toBe(200);
      const [schedule] = schedulesResponseSchema.parse(await response.json()).schedules;
      expect(schedule.nextRunAt).toBe(null);
      return schedule.nextRunUnavailableReason;
    };
    await setServerGrants(ownerId, [{ serverId: managedServerId, capabilities: ["server.view", "schedules.manage"] }]);
    expect(await readReason()).toBe("action_access_removed");
    await setServerGrants(ownerId, [{ serverId: managedServerId, capabilities: ["server.view"] }]);
    expect(await readReason()).toBe("owner_access_removed");
    expect((await fetch(`${baseUrl}${collection}`, { headers: { Cookie: cookie } })).status).toBe(403);
    expect((await changeOwner(true)).status).toBe(200);
    expect(await readReason()).toBe("owner_disabled");
    expect((await fetch(`${baseUrl}${collection}`, { headers: { Cookie: cookie } })).status).toBe(401);
  });

  it("shows deployment root choices only to administrators without exposing other environment settings", async () => {
    const { cookie: viewerCookie } = await createViewerSession(await setupAdministrator());
    const originalBackupRoots = process.env.LUDOCK_BACKUP_ROOTS;
    const originalComposeRoots = process.env.LUDOCK_COMPOSE_ROOTS;
    try {
      delete process.env.LUDOCK_BACKUP_ROOTS;
      delete process.env.LUDOCK_COMPOSE_ROOTS;
      const defaults = await authorizedFetch("/api/v1/settings/deployment");
      expect(defaults.status).toBe(200);
      expect(await defaults.json()).toStrictEqual({
        backupRoots: [], composeRoots: [], composeAvailable: false,
      });

      process.env.LUDOCK_BACKUP_ROOTS = [" /backups ", "/archive", "/backups", " "].join(path.delimiter);
      process.env.LUDOCK_COMPOSE_ROOTS = ["/srv/games", "/srv/games"].join(path.delimiter);
      composeAvailability.mockResolvedValue(true);
      const response = await authorizedFetch("/api/v1/settings/deployment");
      expect(response.status).toBe(200);
      const body = await response.json() as Record<string, unknown>;
      expect(body.backupRoots).toStrictEqual(["/backups", "/archive"]);
      expect(body.composeRoots).toStrictEqual(["/srv/games"]);
      expect(body.composeAvailable).toBe(true);
      expect(Object.keys(body).sort()).toStrictEqual(["backupRoots", "composeAvailable", "composeRoots"]);

      for (const [headers, expected] of [
        [{}, 401], [{ Cookie: viewerCookie }, 403],
      ] as const) {
        const denied = await fetch(`${baseUrl}/api/v1/settings/deployment`, { headers });
        expect(denied.status).toBe(expected);
        expect(JSON.stringify(await denied.json())).not.toMatch(/\/backups|\/archive|\/srv\/games/);
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
      expect(managedStopCalled).toBe(true);
      controller.abort();
      await first;
      const conflict = await authorizedFetch(
        `/api/v1/servers/${managedServerId}/stop`,
        { method: "POST" },
      );
      expect(conflict.status).toBe(409);
    } finally {
      finishStop();
      stopGate = undefined;
    }
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect((
        await authorizedFetch(`/api/v1/servers/${managedServerId}/stop`, {
          method: "POST",
        })
      ).status).toBe(200);
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
    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      generation: string;
      entries: Array<{
        level: string;
        component: string;
        message: string;
        context?: Record<string, unknown>;
      }>;
    };
    expect(body.generation).toBeTruthy();
    const marker = body.entries.find(
      (entry) =>
        entry.component === "http-test" && entry.message.includes("diagnostic"),
    );
    expect(marker).toBeTruthy();
    expect(marker.level).toBe("warn");
    expect(marker.context?.apiToken).toBe("[REDACTED]");
    expect(JSON.stringify(marker)).not.toMatch(/must-not-reach-browser/);
    expect(marker.context?.requestId).toBe("log-test-request");
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
      expect(body.entries.some(
          (entry) =>
            entry.component === "api" &&
            entry.message === "HTTP request completed" &&
            entry.context?.path === "/api/v1/application-logs",
        )).toBe(false);
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
    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      sessions: Array<{ id: string; current: boolean }>;
    };
    expect(body.sessions.length).toBe(1);
    expect(body.sessions[0].current).toBe(true);

    const revoked = await fetch(
      `${baseUrl}/api/v1/account/sessions/${body.sessions[0].id}`,
      { method: "DELETE", headers: { Cookie: viewerCookie } },
    );
    expect(revoked.status).toBe(200);
    await assertAuditEntry(sessionCookie, "auth.session.revoked", body.sessions[0].id);
    expect((
        await fetch(`${baseUrl}/api/v1/auth/me`, {
          headers: { Cookie: viewerCookie },
        })
      ).status).toBe(401);
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
    expect(admin).toBeTruthy();

    const selfDelete = await fetch(`${baseUrl}/api/v1/users/${admin.id}`, {
      method: "DELETE",
      headers: { Cookie: sessionCookie },
    });
    expect(selfDelete.status).toBe(409);

    const deleteViewer = await fetch(`${baseUrl}/api/v1/users/${viewerId}`, {
      method: "DELETE",
      headers: { Cookie: sessionCookie },
    });
    expect(deleteViewer.status).toBe(200);
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
    expect(response.status).toBe(200);
    const replacementCookie = (response.headers.get("set-cookie") || "").split(
      ";",
    )[0];
    expect(replacementCookie).toMatch(/ludock_session=/);
    expect((
        await fetch(`${baseUrl}/api/v1/auth/me`, {
          headers: { Cookie: sessionCookie },
        })
      ).status).toBe(401);
    expect((await fetch(`${baseUrl}/api/v1/auth/me`, {
        headers: { Cookie: replacementCookie },
      })).status).toBe(200);

    const oldLogin = await fetch(`${baseUrl}/api/v1/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        username: "admin",
        password: "integration-password",
      }),
    });
    expect(oldLogin.status).toBe(401);
    const newLogin = await fetch(`${baseUrl}/api/v1/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        username: "admin",
        password: "replacement-password",
      }),
    });
    expect(newLogin.status).toBe(200);
    await assertAuditEntry(replacementCookie, "auth.password.changed");
  });

  it("lists managed containers without unrelated labels", async () => {
    const response = await authorizedFetch("/api/v1/servers");
    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      servers: Array<{ id: string; labels: Record<string, string> }>;
    };

    expect(body.servers.length).toBe(1);
    expect(body.servers[0].id).toBe(managedServerId);
    expect(managedServerId).toMatch(/^[0-9a-f-]{36}$/);
    expect(managedServerId).not.toBe(managedInfo.Id);
    expect(body.servers[0].labels).toStrictEqual({});
  });

  it("allows managed lifecycle actions", async () => {
    const response = await authorizedFetch(
      `/api/v1/servers/${managedServerId}/stop`,
      { method: "POST" },
    );
    expect(response.status).toBe(200);
    expect(managedStopCalled).toBe(true);
  });

  it("rejects unmanaged lifecycle actions", async () => {
    const response = await authorizedFetch(
      "/api/v1/servers/unmanaged-container-id/stop",
      { method: "POST" },
    );
    expect(response.status).toBe(404);
    expect(unmanagedStopCalled).toBe(false);
  });

  it("throttles repeated login failures", async () => {
    await setupAdministrator();
    const login = (password: string) =>
      fetch(`${baseUrl}/api/v1/auth/login`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username: "admin", password }),
      });

    expect((await login("definitely-incorrect")).status).toBe(401);
    expect((await login("integration-password")).status).toBe(200);
    for (let attempt = 0; attempt < 5; attempt += 1) {
      expect((await login("definitely-incorrect")).status).toBe(401);
    }
    expect((await login("integration-password")).status).toBe(429);
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
      expect((await guess()).status).toBe(400);
    }
    expect((await guess()).status).toBe(429);

    const correct = await fetch(`${baseUrl}/api/v1/account/change-password`, {
      method: "POST",
      headers: { Cookie: sessionCookie, "Content-Type": "application/json" },
      body: JSON.stringify({
        currentPassword: "integration-password",
        newPassword: "another-replacement-password",
      }),
    });
    expect(correct.status).toBe(429);
  });
});
