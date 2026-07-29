import assert from "node:assert/strict";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { after, before, describe, it } from "node:test";

process.env.LUDOCK_DB_PATH = ":memory:";
process.env.LUDOCK_API_TOKEN = "integration-api-secret-0123456789abcdef";

const [{ createApp }, { getDockerInstance }] = await Promise.all([
  import("../src/app.js"),
  import("../src/docker.js"),
]);

const docker = getDockerInstance();
const originalPing = docker.ping.bind(docker);
const originalListContainers = docker.listContainers.bind(docker);
const originalGetContainer = docker.getContainer.bind(docker);

let server: Server;
let baseUrl: string;
let managedStopCalled = false;
let unmanagedStopCalled = false;
let sessionCookie = "";
let viewerCookie = "";
let viewerId = "";

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

before(async () => {
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
      stop: async () => {
        if (managed) managedStopCalled = true;
        else unmanagedStopCalled = true;
      },
    };
  }) as unknown as typeof docker.getContainer;

  server = createServer(createApp({ frontendDist: false }));
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address() as AddressInfo;
  baseUrl = `http://127.0.0.1:${address.port}`;
});

after(async () => {
  docker.ping = originalPing;
  docker.listContainers = originalListContainers;
  docker.getContainer = originalGetContainer;
  await new Promise<void>((resolve, reject) =>
    server.close((error) => (error ? reject(error) : resolve()))
  );
});

function authorizedFetch(path: string, init: RequestInit = {}) {
  const headers = new Headers(init.headers);
  headers.set("Authorization", "Bearer integration-api-secret-0123456789abcdef");
  return fetch(`${baseUrl}${path}`, { ...init, headers });
}

describe("HTTP application", () => {
  it("serves public status and health endpoints", async () => {
    const authStatus = await fetch(`${baseUrl}/api/auth/status`);
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
      }
    );
    assert.equal(authStatus.headers.get("cache-control"), "no-store");

    const health = await fetch(`${baseUrl}/api/health`);
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
      "same-origin"
    );
    assert.match(
      health.headers.get("content-security-policy") || "",
      /default-src 'self'/
    );

    const proxiedHealth = await fetch(`${baseUrl}/api/health`, {
      headers: { "X-Forwarded-Proto": "https" },
    });
    assert.equal(
      proxiedHealth.headers.get("strict-transport-security"),
      "max-age=31536000; includeSubDomains"
    );
  });

  it("completes initial setup without a token and establishes a session", async () => {
    const response = await fetch(`${baseUrl}/api/auth/setup`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Origin: baseUrl.replace("http://", "https://"),
        "X-Forwarded-Proto": "https",
      },
      body: JSON.stringify({
        username: "admin",
        password: "integration-password",
      }),
    });
    assert.equal(response.status, 201);
    const setCookie = response.headers.get("set-cookie") || "";
    assert.match(setCookie, /ludock_session=/);
    assert.match(setCookie, /HttpOnly/i);
    assert.match(setCookie, /SameSite=Strict/i);
    assert.match(setCookie, /Secure/i);
    sessionCookie = setCookie.split(";")[0];

    const me = await fetch(`${baseUrl}/api/auth/me`, {
      headers: { Cookie: sessionCookie },
    });
    assert.equal(me.status, 200);
    assert.equal(((await me.json()) as { user: { username: string } }).user.username, "admin");
  });

  it("supports password login without revealing which credential failed", async () => {
    const invalid = await fetch(`${baseUrl}/api/auth/login`, {
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

    const valid = await fetch(`${baseUrl}/api/auth/login`, {
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
    assert.equal((await fetch(`${baseUrl}/api/auth/me`)).status, 401);
    assert.equal(
      (
        await fetch(`${baseUrl}/api/auth/me`, {
          headers: { Authorization: "Bearer wrong" },
        })
      ).status,
      401
    );
    assert.equal((await authorizedFetch("/api/auth/me")).status, 200);
    const missing = await authorizedFetch("/api/does-not-exist");
    assert.equal(missing.status, 404);
    assert.deepEqual(await missing.json(), { error: "API endpoint not found" });
  });

  it("rejects cross-origin state changes", async () => {
    const response = await authorizedFetch("/api/users", {
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

    const fetchMetadataResponse = await authorizedFetch("/api/auth/logout", {
      method: "POST",
      headers: { "Sec-Fetch-Site": "cross-site" },
    });
    assert.equal(fetchMetadataResponse.status, 403);
  });

  it("requires an explicit binary media type for uploads", async () => {
    const response = await authorizedFetch(
      `/api/servers/${managedInfo.Id}/files/upload?root=root-0&path=&name=mod.jar`,
      {
        method: "PUT",
        headers: { "Content-Type": "text/plain" },
        body: "not accepted as an upload",
      }
    );
    assert.equal(response.status, 415);
  });

  it("manages users without exposing password hashes", async () => {
    const created = await fetch(`${baseUrl}/api/users`, {
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
    viewerId = createdBody.user.id;
    assert.equal(createdBody.user.username, "viewer");
    assert.equal(createdBody.user.passwordHash, undefined);

    const duplicate = await fetch(`${baseUrl}/api/users`, {
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

    const list = await fetch(`${baseUrl}/api/users`, {
      headers: { Cookie: sessionCookie },
    });
    assert.equal(list.status, 200);
    assert.doesNotMatch(JSON.stringify(await list.json()), /passwordHash|password_hash/);

    const login = await fetch(`${baseUrl}/api/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        username: "viewer",
        password: "viewer-password",
      }),
    });
    assert.equal(login.status, 200);
    viewerCookie = (login.headers.get("set-cookie") || "").split(";")[0];
  });

  it("enforces role permissions and protects the final administrator", async () => {
    const viewerList = await fetch(`${baseUrl}/api/users`, {
      headers: { Cookie: viewerCookie },
    });
    assert.equal(viewerList.status, 403);

    const viewerStop = await fetch(
      `${baseUrl}/api/servers/${managedInfo.Id}/stop`,
      { method: "POST", headers: { Cookie: viewerCookie } }
    );
    assert.equal(viewerStop.status, 403);

    const viewerUpload = await fetch(
      `${baseUrl}/api/servers/${managedInfo.Id}/files/upload?root=root-0&path=&name=blocked.jar`,
      {
        method: "PUT",
        headers: {
          Cookie: viewerCookie,
          "Content-Type": "application/octet-stream",
        },
        body: "blocked",
      }
    );
    assert.equal(viewerUpload.status, 403);

    const users = (await (
      await fetch(`${baseUrl}/api/users`, { headers: { Cookie: sessionCookie } })
    ).json()) as { users: Array<{ id: string; username: string }> };
    const admin = users.users.find((user) => user.username === "admin");
    assert.ok(admin);
    const demote = await fetch(`${baseUrl}/api/users/${admin.id}`, {
      method: "PATCH",
      headers: {
        Cookie: sessionCookie,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ role: "viewer", disabled: false }),
    });
    assert.equal(demote.status, 409);
  });

  it("lists and revokes account sessions", async () => {
    const response = await fetch(`${baseUrl}/api/account/sessions`, {
      headers: { Cookie: viewerCookie },
    });
    assert.equal(response.status, 200);
    const body = (await response.json()) as {
      sessions: Array<{ id: string; current: boolean }>;
    };
    assert.equal(body.sessions.length, 1);
    assert.equal(body.sessions[0].current, true);

    const revoked = await fetch(
      `${baseUrl}/api/account/sessions/${body.sessions[0].id}`,
      { method: "DELETE", headers: { Cookie: viewerCookie } }
    );
    assert.equal(revoked.status, 200);
    assert.equal(
      (
        await fetch(`${baseUrl}/api/auth/me`, {
          headers: { Cookie: viewerCookie },
        })
      ).status,
      401
    );
  });

  it("deletes other accounts but not the current account", async () => {
    const users = (await (
      await fetch(`${baseUrl}/api/users`, { headers: { Cookie: sessionCookie } })
    ).json()) as { users: Array<{ id: string; username: string }> };
    const admin = users.users.find((user) => user.username === "admin");
    assert.ok(admin);

    const selfDelete = await fetch(`${baseUrl}/api/users/${admin.id}`, {
      method: "DELETE",
      headers: { Cookie: sessionCookie },
    });
    assert.equal(selfDelete.status, 409);

    const deleteViewer = await fetch(`${baseUrl}/api/users/${viewerId}`, {
      method: "DELETE",
      headers: { Cookie: sessionCookie },
    });
    assert.equal(deleteViewer.status, 200);
  });

  it("changes passwords, revokes old sessions, and preserves the current login", async () => {
    const response = await fetch(`${baseUrl}/api/account/change-password`, {
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
    const replacementCookie = (response.headers.get("set-cookie") || "").split(";")[0];
    assert.match(replacementCookie, /ludock_session=/);
    assert.equal(
      (
        await fetch(`${baseUrl}/api/auth/me`, {
          headers: { Cookie: sessionCookie },
        })
      ).status,
      401
    );
    sessionCookie = replacementCookie;

    const oldLogin = await fetch(`${baseUrl}/api/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        username: "admin",
        password: "integration-password",
      }),
    });
    assert.equal(oldLogin.status, 401);
  });

  it("records administrative and authentication activity", async () => {
    const response = await fetch(`${baseUrl}/api/audit`, {
      headers: { Cookie: sessionCookie },
    });
    assert.equal(response.status, 200);
    const body = (await response.json()) as {
      entries: Array<{ action: string; targetId: string | null }>;
    };
    const actions = body.entries.map((entry) => entry.action);
    assert.ok(actions.includes("user.created"));
    assert.ok(actions.includes("user.deleted"));
    assert.ok(actions.includes("auth.password.changed"));
    assert.ok(actions.includes("auth.session.revoked"));
    assert.ok(body.entries.some((entry) => entry.targetId === viewerId));
  });

  it("lists managed containers without unrelated labels", async () => {
    const response = await authorizedFetch("/api/servers");
    assert.equal(response.status, 200);
    const body = (await response.json()) as {
      servers: Array<{ id: string; labels: Record<string, string> }>;
    };

    assert.equal(body.servers.length, 1);
    assert.equal(body.servers[0].id, managedInfo.Id);
    assert.deepEqual(body.servers[0].labels, {
      "ludock.enable": "true",
      "ludock.name": "Managed Fixture",
    });
  });

  it("allows managed lifecycle actions", async () => {
    const response = await authorizedFetch(
      `/api/servers/${managedInfo.Id}/stop`,
      { method: "POST" }
    );
    assert.equal(response.status, 200);
    assert.equal(managedStopCalled, true);
  });

  it("rejects unmanaged lifecycle actions", async () => {
    const response = await authorizedFetch(
      "/api/servers/unmanaged-container-id/stop",
      { method: "POST" }
    );
    assert.equal(response.status, 403);
    assert.equal(unmanagedStopCalled, false);
  });

  it("throttles repeated login failures", async () => {
    const clearPriorFailures = await fetch(`${baseUrl}/api/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        username: "admin",
        password: "replacement-password",
      }),
    });
    assert.equal(clearPriorFailures.status, 200);

    for (let attempt = 0; attempt < 5; attempt += 1) {
      const failed = await fetch(`${baseUrl}/api/auth/login`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          username: "admin",
          password: "definitely-incorrect",
        }),
      });
      assert.equal(failed.status, 401);
    }

    const blocked = await fetch(`${baseUrl}/api/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        username: "admin",
        password: "replacement-password",
      }),
    });
    assert.equal(blocked.status, 429);
  });

  it("throttles current-password guessing on password change", async () => {
    const guess = () =>
      fetch(`${baseUrl}/api/account/change-password`, {
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

    const correct = await fetch(`${baseUrl}/api/account/change-password`, {
      method: "POST",
      headers: { Cookie: sessionCookie, "Content-Type": "application/json" },
      body: JSON.stringify({
        currentPassword: "replacement-password",
        newPassword: "another-replacement-password",
      }),
    });
    assert.equal(correct.status, 429);
  });
});
