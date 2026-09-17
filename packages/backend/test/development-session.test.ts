import { serve } from "bun";
import { expect, afterAll as after, it } from "bun:test";

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
    id: crypto.randomUUID(),
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
  expect(cookie.startsWith(`${cookieName}=${token};`)).toBeTruthy();
  expect(cookie).toMatch(/; HttpOnly(?:;|$)/i);
  expect(cookie).toMatch(/; SameSite=Strict(?:;|$)/i);
  expect(cookieName).toBe("ludock_session_012345abcdef");
  expect(getRequestSession(new Request(request, {
      headers: {
        cookie: `ludock_session=${token}; ludock_session_fedcba543210=${token}`,
      },
    }))).toBe(null);
  expect(getRequestSession(new Request(request, { headers: { cookie: `${cookieName}=${token}` } }))?.user
      .id).toBe(user.id);
  const cleared = new Headers();
  clearSessionCookie(cleared);
  expect(cleared.get("Set-Cookie")?.startsWith(`${cookieName}=;`)).toBeTruthy();
  expect(cleared.get("Set-Cookie")!).toMatch(/; Max-Age=0(?:;|$)/i);
});

it("rejects another checkout before processing its HTTP request", async () => {
  const server = serve({ ...createApp({ frontendDist: false }), hostname: "127.0.0.1", port: 0 });
  const origin = server.url.origin;
  try {
    const response = await fetch(`${origin}/api/v1/auth/logout`, {
      method: "POST",
      headers: { "X-Ludock-Dev-Instance": "fedcba543210" },
    });
    expect(response.status).toBe(409);
    expect(response.headers.get("x-ludock-dev-instance")).toBe("012345abcdef");
    expect(response.headers.get("set-cookie")).toBe(null);
    expect(response.headers.get("clear-site-data")).toBe(null);
    expect(await response.json()).toStrictEqual({
      error: "This request belongs to a different development checkout.",
    });

    for (const headers of [{}, { "X-Ludock-Dev-Instance": "012345abcdef" }]) {
      const status = await fetch(`${origin}/api/v1/auth/status`, { headers });
      expect(status.status).toBe(200);
      expect(status.headers.get("x-ludock-dev-instance")).toBe("012345abcdef");
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
    expect(response.status).toBe(200);
    expect(response.headers.get("clear-site-data")).toBe('"cache", "storage"');
    expect(response.headers.get("set-cookie") || "").toMatch(/^ludock_session_012345abcdef=;/);
    expect(await response.json()).toStrictEqual({ ok: true });
  } finally {
    await server.stop(true);
  }
});
