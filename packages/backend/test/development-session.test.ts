import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { serve } from "bun";
import { afterAll as after, it } from "bun:test";

process.env.NODE_ENV = "development";
process.env.LUDOCK_DEV_INSTANCE = "012345abcdef";
process.env.LUDOCK_DB_PATH = ":memory:";

const { createUser, closeDatabase } = await import("../src/database.js");
const {
  setSessionCookie,
  clearSessionCookie,
  createSession,
  getRequestSession,
} = await import("../src/auth.js");
const { createApp } = await import("../src/app.js");
after(closeDatabase);

it("keeps sessions separate when development checkouts share a browser host", () => {
  const user = {
    id: randomUUID(),
    username: "developer",
    passwordHash: "unused-test-hash",
    role: "admin" as const,
    createdAt: Date.now(),
    disabled: false,
  };
  createUser(user);
  const request = new Request("http://127.0.0.1/");
  const { token } = createSession(user, request, "127.0.0.1");
  const headers = new Headers();
  setSessionCookie(headers, request, token);
  const cookie = headers.get("Set-Cookie")!;
  const cookieName = cookie.split("=", 1)[0];
  assert.ok(cookie.startsWith(`${cookieName}=${token};`));
  assert.match(cookie, /; HttpOnly(?:;|$)/i);
  assert.match(cookie, /; SameSite=Strict(?:;|$)/i);
  assert.equal(cookieName, "ludock_session_012345abcdef");
  assert.equal(
    getRequestSession(new Request(request, {
      headers: {
        cookie: `ludock_session=${token}; ludock_session_fedcba543210=${token}`,
      },
    })),
    null,
  );
  assert.equal(
    getRequestSession(new Request(request, { headers: { cookie: `${cookieName}=${token}` } }))?.user
      .id,
    user.id,
  );
  const cleared = new Headers();
  clearSessionCookie(cleared);
  assert.ok(cleared.get("Set-Cookie")?.startsWith(`${cookieName}=;`));
  assert.match(cleared.get("Set-Cookie")!, /; Max-Age=0(?:;|$)/i);
});

it("rejects another checkout before processing its HTTP request", async () => {
  const server = serve({ ...createApp({ frontendDist: false }), hostname: "127.0.0.1", port: 0 });
  const origin = server.url.origin;
  try {
    const response = await fetch(`${origin}/api/v1/auth/logout`, {
      method: "POST",
      headers: { "X-Ludock-Dev-Instance": "fedcba543210" },
    });
    assert.equal(response.status, 409);
    assert.equal(response.headers.get("x-ludock-dev-instance"), "012345abcdef");
    assert.equal(response.headers.get("set-cookie"), null);
    assert.equal(response.headers.get("clear-site-data"), null);
    assert.deepEqual(await response.json(), {
      error: "This request belongs to a different development checkout.",
    });

    for (const headers of [{}, { "X-Ludock-Dev-Instance": "012345abcdef" }]) {
      const status = await fetch(`${origin}/api/v1/auth/status`, { headers });
      assert.equal(status.status, 200);
      assert.equal(status.headers.get("x-ludock-dev-instance"), "012345abcdef");
      await status.body?.cancel();
    }
  } finally {
    await server.stop(true);
  }
});

it("logs out one checkout without clearing cookies for its peers", async () => {
  const server = serve({ ...createApp({ frontendDist: false }), hostname: "127.0.0.1", port: 0 });
  const origin = server.url.origin;
  try {
    const response = await fetch(`${origin}/api/v1/auth/logout`, {
      method: "POST",
      headers: { "X-Ludock-Dev-Instance": "012345abcdef", Origin: origin },
    });
    assert.equal(response.status, 200);
    assert.equal(response.headers.get("clear-site-data"), '"cache", "storage"');
    assert.match(
      response.headers.get("set-cookie") || "",
      /^ludock_session_012345abcdef=;/,
    );
    assert.deepEqual(await response.json(), { ok: true });
  } finally {
    await server.stop(true);
  }
});
