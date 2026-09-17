import { serve } from "bun";
import { expect, afterEach, beforeEach, describe, it } from "bun:test";
import { createApp } from "../src/app.js";
import { createSession } from "../src/auth.js";
import {
  closeDatabase, createUser, deleteUserSessions, findUserById,
  updateUserAccess, type SessionUser,
} from "../src/database.js";
import type { HttpServer } from "../src/routes/request.js";
import { createMountProof } from "../src/mount-proof.js";

process.env.LUDOCK_DB_PATH = ":memory:";
let admin: SessionUser;
let target: SessionUser;
let cookie: string;
beforeEach(() => {
  closeDatabase();
  admin = { id: crypto.randomUUID(), username: "admin", role: "admin" };
  target = { id: crypto.randomUUID(), username: "target", role: "viewer" };
  for (const user of [admin, target])
    createUser({ ...user, passwordHash: "fixture", disabled: false, createdAt: 0 });
  cookie = `ludock_session=${createSession(admin, new Request("http://localhost")).token}`;
});
afterEach(() => closeDatabase());

describe("native HTTP request lifetimes", () => {
  it("returns a safe conflict for an unverifiable data mount", async () => {
    const app = createApp({ frontendDist: false, routes: {
      "/api/v1/mount-proof": {
        GET: async () => {
          await createMountProof([{ Type: "volume", Source: "/invalid/volume", Destination: "/data", RW: true }]);
          expect.unreachable("An invalid named volume cannot produce a proof");
        },
      },
    } });
    const response = await app.fetch(new Request("http://localhost/api/v1/mount-proof", {
      headers: { Cookie: cookie },
    }), { requestIP: () => null, timeout: () => {} });
    expect(response.status).toBe(409);
    const body = await response.json() as { code: string; error: string };
    expect(body.code).toBe("UNVERIFIED_DATA_MOUNT");
    expect(body.error).toMatch(/data mount could not be verified/);
    expect(body.error).not.toMatch(/invalid\/volume/);
  });
  it("serves public case and trailing-slash variants without redirects or broader method access", async () => {
    const server = serve({ ...createApp({ frontendDist: false }), hostname: "127.0.0.1", port: 0 });
    try {
      for (const pathname of ["/API/v1/AuTh/StAtUs/", "/api/v1/auth/status/"]) {
        const response = await fetch(new URL(pathname, server.url), { redirect: "error" });
        expect(response.status).toBe(200);
        expect(response.headers.get("Location")).toBe(null);
        expect((await response.json() as { authenticated: boolean }).authenticated).toBe(false);
        const head = await fetch(new URL(pathname, server.url), { method: "HEAD", redirect: "error" });
        expect(head.status).toBe(200);
        expect(await head.text()).toBe("");
        const unsupported = await fetch(new URL(pathname, server.url), { method: "POST", redirect: "error" });
        expect(unsupported.status).toBe(401);
        await unsupported.body?.cancel();
      }
      const logout = await fetch(new URL("/API/v1/AUTH/LOGOUT/", server.url), { method: "POST", redirect: "error" });
      expect(logout.status).toBe(200);
      expect(await logout.json()).toStrictEqual({ ok: true });
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
      expect(denied.status).toBe(401);
      await denied.body?.cancel();
      expect(calls).toStrictEqual([]);
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
      expect(post.status).toBe(200);
      expect(post.headers.get("Location")).toBe(null);
      expect(await post.json()).toStrictEqual({ value: "MiXeD %2fName", query: "CaSe+Value", body: { unchanged: "JSON body" } });
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
      expect(put.status).toBe(200);
      expect(await put.json()).toStrictEqual({ value: "MiXeD %2fName", body: "binary body\u0000preserved" });
      expect(calls).toStrictEqual(["POST", "PUT"]);
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
      expect(timeouts, "incoming bodies keep their idle deadline").toStrictEqual([30]);
      if (change === "revoke session") deleteUserSessions(admin.id);
      else updateUserAccess(admin.id, "viewer", false);
      finish();
      const result = await response;
      expect(result.status).toBe(change === "revoke session" ? 401 : 403);
      await result.text();
      expect(findUserById(target.id)?.role).toBe("viewer");
      expect(timeouts).toStrictEqual([30, 0]);
    });
  }
});
