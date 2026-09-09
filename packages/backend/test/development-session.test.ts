import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import type { Request, Response } from "express";
import { after, it } from "node:test";

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
  const request = {
    ip: "127.0.0.1",
    headers: {},
    socket: {},
    get: () => undefined,
  } as unknown as Request;
  const { token } = createSession(user, request);
  let cookieName = "";
  let clearedName = "";
  const response = {
    cookie: (name: string, value: string, options: Record<string, unknown>) => {
      cookieName = name;
      assert.equal(value, token);
      assert.equal(options.httpOnly, true);
      assert.equal(options.sameSite, "strict");
    },
    clearCookie: (name: string) => {
      clearedName = name;
    },
  } as unknown as Response;
  setSessionCookie(response, request, token);
  assert.equal(cookieName, "ludock_session_012345abcdef");
  assert.equal(
    getRequestSession({
      headers: {
        cookie: `ludock_session=${token}; ludock_session_fedcba543210=${token}`,
      },
    }),
    null,
  );
  assert.equal(
    getRequestSession({ headers: { cookie: `${cookieName}=${token}` } })?.user
      .id,
    user.id,
  );
  clearSessionCookie(response);
  assert.equal(clearedName, cookieName);
});

it("rejects another checkout before processing its HTTP request", async () => {
  const server = createServer(createApp({ frontendDist: false }));
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const origin = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
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
    await new Promise<void>((resolve, reject) =>
      server.close((error) => (error ? reject(error) : resolve())),
    );
  }
});

it("logs out one checkout without clearing cookies for its peers", async () => {
  const server = createServer(createApp({ frontendDist: false }));
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const origin = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
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
    await new Promise<void>((resolve, reject) =>
      server.close((error) => (error ? reject(error) : resolve())),
    );
  }
});
