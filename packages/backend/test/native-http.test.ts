import { serve } from "bun";
import { expect, afterEach, beforeEach, describe, it } from "bun:test";
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
  admin = { id: crypto.randomUUID(), username: "admin", role: "admin" };
  target = { id: crypto.randomUUID(), username: "target", role: "viewer" };
  for (const user of [admin, target])
    createUser({ ...user, passwordHash: "fixture", disabled: false, createdAt: 0 });
  cookie = `ludock_session=${createSession(admin, new Request("http://localhost")).token}`;
});
afterEach(() => closeDatabase());

describe("native HTTP request lifetimes", () => {
  it("dispatches an authenticated request once with its streamed body and decoded parameters", async () => {
    let calls = 0;
    const app = createApp({ frontendDist: false, routes: {
      "/api/v1/native-echo/:value": {
        POST: (context) => {
          calls++;
          return Response.json({ value: context.params.value, query: context.url.searchParams.get("value"), body: context.body });
        },
      },
    } });
    const server = serve({ ...app, hostname: "127.0.0.1", port: 0 });
    try {
      const url = new URL("/API/v1/NATIVE-ECHO/MiXeD%20%252fName/?value=CaSe%2BValue", server.url);
      const denied = await fetch(url, { method: "POST", redirect: "error" });
      expect(denied.status).toBe(401);
      await denied.body?.cancel();
      expect(calls).toBe(0);

      const response = await fetch(url, {
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
      expect(response.status).toBe(200);
      expect(await response.json()).toStrictEqual({ value: "MiXeD %2fName", query: "CaSe+Value", body: { unchanged: "JSON body" } });
      expect(calls).toBe(1);
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
