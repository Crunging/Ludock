import assert from "node:assert/strict";
import { serve } from "bun";
import { afterEach, beforeEach, describe, it } from "bun:test";
import { randomUUID } from "node:crypto";
import { createApp } from "../src/app.js";
import { createSession } from "../src/auth.js";
import {
  closeDatabase, createUser, deleteUserSessions, findUserById,
  updateUserAccess, type SessionUser,
} from "../src/database.js";
import type { HttpServer } from "../src/routes/request.js";

process.env.LUDOCK_DB_PATH = ":memory:";
let admin: SessionUser;
let target: SessionUser;
let cookie: string;
beforeEach(() => {
  closeDatabase();
  admin = { id: randomUUID(), username: "admin", role: "admin" };
  target = { id: randomUUID(), username: "target", role: "viewer" };
  for (const user of [admin, target])
    createUser({ ...user, passwordHash: "fixture", disabled: false, createdAt: 0 });
  cookie = `ludock_session=${createSession(admin, new Request("http://localhost")).token}`;
});
afterEach(() => closeDatabase());

describe("native HTTP request lifetimes", () => {
  it("serves public case and trailing-slash variants without redirects or broader method access", async () => {
    const server = serve({ ...createApp({ frontendDist: false }), hostname: "127.0.0.1", port: 0 });
    try {
      for (const pathname of ["/API/v1/AuTh/StAtUs/", "/api/v1/auth/status/"]) {
        const response = await fetch(new URL(pathname, server.url), { redirect: "error" });
        assert.equal(response.status, 200);
        assert.equal(response.headers.get("Location"), null);
        assert.equal((await response.json() as { authenticated: boolean }).authenticated, false);
        const head = await fetch(new URL(pathname, server.url), { method: "HEAD", redirect: "error" });
        assert.equal(head.status, 200);
        assert.equal(await head.text(), "");
        const unsupported = await fetch(new URL(pathname, server.url), { method: "POST", redirect: "error" });
        assert.equal(unsupported.status, 401);
        await unsupported.body?.cancel();
      }
      const logout = await fetch(new URL("/API/v1/AUTH/LOGOUT/", server.url), { method: "POST", redirect: "error" });
      assert.equal(logout.status, 200);
      assert.deepEqual(await logout.json(), { ok: true });
    } finally { await server.stop(true); }
  });

  it("dispatches authenticated variants once while preserving method, streamed bodies and parameter decoding", async () => {
    const calls: string[] = [];
    const app = createApp({ frontendDist: false, routes: {
      "/api/v1/native-echo/:value": {
        POST: (context) => {
          calls.push(context.request.method);
          return Response.json({ value: context.params.value, query: context.url.searchParams.get("value"), body: context.body });
        },
        PUT: async (context) => {
          calls.push(context.request.method);
          return Response.json({ value: context.params.value, body: await context.request.text() });
        },
      },
    } });
    const server = serve({ ...app, hostname: "127.0.0.1", port: 0 });
    try {
      const url = new URL("/API/v1/NATIVE-ECHO/MiXeD%20%252fName/?value=CaSe%2BValue", server.url);
      const denied = await fetch(url, { method: "POST", redirect: "error" });
      assert.equal(denied.status, 401);
      await denied.body?.cancel();
      assert.deepEqual(calls, []);
      const post = await fetch(url, {
        method: "POST", redirect: "error",
        headers: { Cookie: cookie, "Content-Type": "application/json" },
        body: new ReadableStream<Uint8Array>({
          start(controller) {
            controller.enqueue(new TextEncoder().encode('{"unchanged":'));
            controller.enqueue(new TextEncoder().encode('"JSON body"}'));
            controller.close();
          },
        }),
      });
      assert.equal(post.status, 200);
      assert.equal(post.headers.get("Location"), null);
      assert.deepEqual(await post.json(), { value: "MiXeD %2fName", query: "CaSe+Value", body: { unchanged: "JSON body" } });
      const put = await fetch(url, {
        method: "PUT", redirect: "error",
        headers: { Cookie: cookie, "Content-Type": "application/octet-stream" },
        body: new ReadableStream<Uint8Array>({
          start(controller) {
            controller.enqueue(new TextEncoder().encode("binary body\u0000preserved"));
            controller.close();
          },
        }),
      });
      assert.equal(put.status, 200);
      assert.deepEqual(await put.json(), { value: "MiXeD %2fName", body: "binary body\u0000preserved" });
      assert.deepEqual(calls, ["POST", "PUT"]);
    } finally { await server.stop(true); }
  });

  for (const change of ["revoke session", "demote administrator"] as const) {
    it(`rechecks authorization after a slow body when we ${change}`, async () => {
      let started!: () => void;
      const reading = new Promise<void>((resolve) => { started = resolve; });
      let finish!: () => void;
      const gate = new Promise<void>((resolve) => { finish = resolve; });
      const timeouts: number[] = [];
      const server: HttpServer = {
        requestIP: () => null,
        timeout: (_request, seconds) => { timeouts.push(seconds); },
      };
      const request = Object.assign(new Request(`http://localhost/api/v1/users/${target.id}`, {
        method: "PATCH",
        headers: { Cookie: cookie, "Content-Type": "application/json" },
        body: new ReadableStream<Uint8Array>({
          async pull(controller) {
            started();
            await gate;
            controller.enqueue(new TextEncoder().encode('{"role":"admin","disabled":false}'));
            controller.close();
          },
        }, { highWaterMark: 0 }),
      }), { params: { id: target.id } });
      const response = createApp({ frontendDist: false }).routes["/api/v1/users/:id"].PATCH!(request, server);
      await reading;
      assert.deepEqual(timeouts, [30], "incoming bodies keep their idle deadline");
      if (change === "revoke session") deleteUserSessions(admin.id);
      else updateUserAccess(admin.id, "viewer", false);
      finish();
      const result = await response;
      assert.equal(result.status, change === "revoke session" ? 401 : 403);
      await result.text();
      assert.equal(findUserById(target.id)?.role, "viewer");
      assert.deepEqual(timeouts, [30, 0]);
    });
  }
});
