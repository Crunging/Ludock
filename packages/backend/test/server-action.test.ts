import type { Server } from "bun";
import { expect, afterEach, beforeEach, describe, it } from "bun:test";
import { createApp } from "../src/app.js";
import { createSession } from "../src/auth.js";
import {
  closeDatabase,
  createUser,
  deleteUserSessions,
  type SessionUser,
} from "../src/database.js";
import { setServerGrant } from "./fixtures/grants.js";
import { docker } from "../src/docker-client.js";
import { changeContainerState } from "../src/docker.js";
import { AppError } from "../src/errors.js";
import { listLogicalServers } from "../src/identity.js";
import {
  acquireLocks,
  isServerBusy,
  waitForLocksReleased,
} from "../src/operation-locks.js";
import { serverAction } from "../src/routes/server-action.js";
import { respond } from "../src/routes/request.js";
import { okResponseSchema } from "@ludock/shared";
import { refreshServers } from "../src/servers.js";

process.env.LUDOCK_DB_PATH = ":memory:";
process.env.LUDOCK_API_TOKEN = "route-policy-fixture-token-0123456789abcdef";
const admin: SessionUser = {
  id: "api-token",
  username: "fixture-admin",
  role: "admin",
};
const friend: SessionUser = {
  id: "friend",
  username: "friend",
  role: "operator",
};
const originalList = docker.listContainers.bind(docker);
const originalGet = docker.getContainer.bind(docker);
let http: Server<unknown>;
let baseUrl: string;
let serverId: string;
let cookie: string;
let sessionToken: string;
let calls: string[];
let beforeResponse: Promise<void> | undefined;
let afterResponse: Promise<void> | undefined;
let beforeInspect: Promise<void> | undefined;
let inspectCount: number;
let pauseInspectAt: number | undefined;
let containerName: string;
let output: unknown;
let inspectStarted: () => void;
let bindingStarted: Promise<void>;
let clientClosed: Promise<void>;
const releases: Array<() => void> = [];
function gate(): Promise<void> {
  return new Promise((resolve) => releases.push(resolve));
}
const friendFetch = (path: string, init: RequestInit = {}) =>
  fetch(`${baseUrl}/servers/${serverId}${path}`, {
    ...init,
    headers: { Cookie: cookie, ...init.headers },
  });
beforeEach(async () => {
  closeDatabase();
  createUser({
    ...friend,
    passwordHash: "fixture",
    disabled: false,
    createdAt: 0,
  });
  sessionToken = createSession(friend, new Request("http://127.0.0.1", {
    headers: { "User-Agent": "fixture" },
  }), "127.0.0.1").token;
  cookie = `ludock_session=${sessionToken}`;
  calls = [];
  beforeResponse = undefined;
  afterResponse = undefined;
  beforeInspect = undefined;
  inspectCount = 0;
  pauseInspectAt = undefined;
  containerName = "/fixture";
  output = { ok: true };
  bindingStarted = new Promise((resolve) => {
    inspectStarted = resolve;
  });
  docker.listContainers = (async () => [
    {
      Id: "physical-fixture",
      Image: "alpine:latest",
      Labels: { "ludock.enable": "true" },
    },
  ]) as unknown as typeof docker.listContainers;
  docker.getContainer = (() => ({
    inspect: async () => {
      inspectCount += 1;
      if (beforeInspect && (pauseInspectAt === undefined || pauseInspectAt === inspectCount)) {
        inspectStarted();
        await beforeInspect;
      }
      return {
        Id: "physical-fixture",
        Name: containerName,
        Config: { Image: "alpine:latest", Labels: { "ludock.enable": "true" } },
        State: { Status: "running" },
        Mounts: [],
        NetworkSettings: { Ports: {} },
        Created: "2026-01-01T00:00:00Z",
      };
    },
    start: async () => { calls.push("start"); },
  })) as unknown as typeof docker.getContainer;
  await refreshServers();
  serverId = listLogicalServers()[0].id;
  inspectCount = 0;
  let disconnected!: () => void;
  clientClosed = new Promise((resolve) => { disconnected = resolve; });
  const watchDisconnect = (action: ReturnType<typeof serverAction>): ReturnType<typeof serverAction> =>
    (ctx) => {
      ctx.request.signal.addEventListener("abort", disconnected, { once: true });
      return action(ctx);
    };
  // An unfamiliar route demonstrates that policy travels with registration;
  // it cannot silently miss a separately maintained path/method allowlist.
  const app = createApp({ frontendDist: false, routes: {
    "/servers/:id/start": {
      POST: serverAction("server.start", async (_ctx, context) => {
        await changeContainerState(context.container.id, "start", context.assertAccess);
        return Response.json({ ok: true });
      }),
    },
    "/servers/:id/new-console-action": {
      POST: watchDisconnect(serverAction("console.execute", async (_ctx, context) => {
        calls.push(context.container.id);
        return Response.json({ ok: true });
      })),
    },
    "/servers/:id/stream": {
      GET: serverAction("logs.read", async (_ctx, context) => {
        calls.push(context.container.id);
        let canceled = false;
        const stream = new ReadableStream<Uint8Array>({
          start(controller) {
            const producing = (async () => {
              controller.enqueue(new TextEncoder().encode("first chunk\n"));
              if (beforeResponse) await beforeResponse;
              if (!canceled) {
                controller.enqueue(new TextEncoder().encode("last chunk\n"));
                controller.close();
              }
              if (afterResponse) await afterResponse;
            })();
            context.waitForCleanup(producing);
          },
          cancel() { canceled = true; },
        });
        return new Response(stream);
      }),
    },
    "/servers/:id/failure": {
      POST: serverAction("server.stop", async () => {
        throw new AppError("FIXTURE_FAILURE", 409, "Fixture failure");
      }),
    },
    "/servers/:id/response": {
      GET: () => {
        // @ts-expect-error This fixture deliberately injects corrupt producer data.
        return respond(okResponseSchema, output);
      },
    },
  } });
  http = Bun.serve({ ...app, hostname: "127.0.0.1", port: 0 });
  baseUrl = `http://127.0.0.1:${http.port}`;
});
afterEach(async () => {
  releases.splice(0).forEach((release) => release());
  await http.stop(true);
  await waitForLocksReleased();
  docker.listContainers = originalList;
  docker.getContainer = originalGet;
  closeDatabase();
});

describe("explicit server action policies", () => {
  it("rejects an external rename seen only by the final lifecycle inspect", async () => {
    setServerGrant(friend.id, serverId, ["server.view", "server.start"], admin);
    pauseInspectAt = 3;
    beforeInspect = gate();
    const result = friendFetch("/start", { method: "POST" });
    await bindingStarted;
    containerName = "/different-server";
    releases.splice(0).forEach((release) => release());
    expect((await result).status).toBe(409);
    expect(calls).toStrictEqual([]);
    await waitForLocksReleased();
    expect(isServerBusy(serverId)).toBe(false);
  });
  for (const pauseAt of [2, 3]) {
    for (const revoke of ["grant", "session"] as const) {
      it(`does not start the container after ${revoke} revocation during Docker inspection ${pauseAt}`, async () => {
        setServerGrant(friend.id, serverId, ["server.view", "server.start"], admin);
        pauseInspectAt = pauseAt;
        beforeInspect = gate();
        const result = friendFetch("/start", { method: "POST" });
        await bindingStarted;
        if (revoke === "grant") setServerGrant(friend.id, serverId, ["server.view"], admin);
        else deleteUserSessions(friend.id);
        releases.splice(0).forEach((release) => release());
        const response = await result;
        expect(response.status).toBe(revoke === "grant" ? 403 : 401);
        expect(calls).toStrictEqual([]);
        await waitForLocksReleased();
        expect(isServerBusy(serverId)).toBe(false);
      });
    }
  }
  it("enforces the declared capability on new paths and passes only a resolved binding to the action", async () => {
    setServerGrant(
      friend.id,
      serverId,
      ["server.view", "server.start", "server.stop"],
      admin,
    );
    expect((await friendFetch("/new-console-action", { method: "POST" })).status).toBe(403);
    expect(calls).toStrictEqual([]);
    setServerGrant(
      friend.id,
      serverId,
      ["server.view", "console.execute"],
      admin,
    );
    expect((await friendFetch("/NEW-CONSOLE-ACTION/", { method: "POST" })).status).toBe(200);
    expect(calls).toStrictEqual(["physical-fixture"]);
  });
  it("uses the same capability for GET, HEAD, case and trailing-slash variants", async () => {
    setServerGrant(friend.id, serverId, ["server.view"], admin);
    for (const method of ["GET", "HEAD"])
      expect((await friendFetch("/StReAm/", { method })).status).toBe(403);
    expect(calls).toStrictEqual([]);
    setServerGrant(friend.id, serverId, ["server.view", "logs.read"], admin);
    const head = await friendFetch("/StReAm/", { method: "HEAD" });
    expect(head.status).toBe(200);
    expect(await head.text()).toBe("");
    expect(calls).toStrictEqual(["physical-fixture"]);
  });
  it("retains locks after the response finishes until handler cleanup finishes", async () => {
    setServerGrant(friend.id, serverId, ["server.view", "logs.read"], admin);
    afterResponse = gate();
    const response = await friendFetch("/stream");
    expect(await response.text()).toBe("first chunk\nlast chunk\n");
    expect(isServerBusy(serverId)).toBe(true);
    expect(() => acquireLocks([`server:${serverId}`])).toThrow(/conflicting/);
    releases.splice(0).forEach((release) => release());
    await waitForLocksReleased();
    expect(isServerBusy(serverId)).toBe(false);
  });
  for (const revoke of ["grant", "session"] as const) {
    it(`closes a stream after ${revoke} revocation and retains its lock through cleanup`, async () => {
      setServerGrant(friend.id, serverId, ["server.view", "logs.read"], admin);
      beforeResponse = gate();
      const response = await friendFetch("/stream");
      const reader = response.body!.getReader();
      expect((await reader.read()).done).toBe(false);
      if (revoke === "grant")
        setServerGrant(friend.id, serverId, ["server.view"], admin);
      else deleteUserSessions(friend.id);
      await expect(reader.read()).rejects.toThrow(/terminated|aborted|socket|closed|connection/i);
      expect(isServerBusy(serverId)).toBe(true);
      releases.splice(0).forEach((release) => release());
      await waitForLocksReleased();
      expect(isServerBusy(serverId)).toBe(false);
    });
  }
  it("releases locks when an action fails before writing a response", async () => {
    setServerGrant(friend.id, serverId, ["server.view", "server.stop"], admin);
    const response = await friendFetch("/failure", { method: "POST" });
    expect(response.status).toBe(409);
    await response.text();
    await waitForLocksReleased();
    expect(isServerBusy(serverId)).toBe(false);
  });
  it("validates response contracts without leaking producer data or reporting a client error", async () => {
    output = { ok: true, privateToken: "fixture-private-value" };
    const valid = await friendFetch("/response");
    expect(valid.status).toBe(200);
    expect(await valid.json()).toStrictEqual({ ok: true });
    output = { ok: "true", privateToken: "fixture-private-value" };
    const invalid = await friendFetch("/response");
    expect(invalid.status).toBe(500);
    const body = await invalid.text();
    expect(body).toMatch(/could not produce a valid response/);
    expect(body).not.toMatch(/fixture-private-value|ZodError|invalid_type/);
  });
  it("does not run an action after its client disconnects during authorization", async () => {
    setServerGrant(
      friend.id,
      serverId,
      ["server.view", "console.execute"],
      admin,
    );
    beforeInspect = gate();
    const controller = new AbortController();
    const request = friendFetch("/new-console-action", {
      method: "POST",
      signal: controller.signal,
    }).catch(() => null);
    // Confirm the request reached Docker binding resolution before disconnecting.
    await bindingStarted;
    controller.abort();
    await request;
    await clientClosed;
    releases.splice(0).forEach((release) => release());
    await refreshServers();
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(calls).toStrictEqual([]);
    expect(isServerBusy(serverId)).toBe(false);
  });
});
