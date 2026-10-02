import { serve, type Server } from "bun";
import { expect, afterAll, afterEach, beforeAll, beforeEach, describe, it, spyOn } from "bun:test";
import type { ServerGrantInput } from "@ludock/shared";

process.env.LUDOCK_DB_PATH = ":memory:";
process.env.LUDOCK_API_TOKEN = "integration-api-secret-0123456789abcdef";
process.env.MAX_UPLOAD_SIZE = "1.5 KiB";
process.env.LUDOCK_SETUP_CODE = "integration-setup-code-0123456789abcdef";

const [{ createApp }, { docker }, { createLogger }, { closeDatabase }, { SetupWindow }, compose] =
  await Promise.all([
    import("../src/app.js"),
    import("../src/docker-client.js"),
    import("../src/logger.js"),
    import("../src/database.js"),
    import("../src/auth.js"),
    import("../src/compose.js"),
  ]);

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
  docker.ping = async () => {};
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

    // The exact limit passes the size gate and reaches this fixture's missing file root.
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

    const list = await fetch(`${baseUrl}/api/v1/users`, {
      headers: { Cookie: sessionCookie },
    });
    expect(list.status).toBe(200);
    expect(JSON.stringify(await list.json())).not.toMatch(/passwordHash|password_hash/);


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
    const admin = users.users.find((user) => user.username === "admin")!;
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
    )!;
    expect(marker).toBeTruthy();
    expect(marker.level).toBe("warn");
    expect(marker.context?.apiToken).toBe("[REDACTED]");
    expect(JSON.stringify(marker)).not.toMatch(/must-not-reach-browser/);
    expect(marker.context?.requestId).toBe("log-test-request");
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
