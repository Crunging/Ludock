import assert from "node:assert/strict";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { afterEach, beforeEach, describe, it } from "node:test";
import express, {
  type NextFunction,
  type Request,
  type Response,
} from "express";
import { authMiddleware, createSession } from "../src/auth.js";
import {
  closeDatabase,
  createUser,
  deleteUserSessions,
  type SessionUser,
} from "../src/database.js";
import { setServerGrant, AuthorizationError } from "../src/authorization.js";
import { getDockerInstance } from "../src/docker.js";
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
const docker = getDockerInstance();
const originalList = docker.listContainers.bind(docker);
const originalGet = docker.getContainer.bind(docker);
let http: Server;
let baseUrl: string;
let serverId: string;
let cookie: string;
let sessionToken: string;
let calls: string[];
let beforeResponse: Promise<void> | undefined;
let afterResponse: Promise<void> | undefined;
let beforeInspect: Promise<void> | undefined;
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
  sessionToken = createSession(friend, {
    ip: "127.0.0.1",
    get: () => "fixture",
  } as unknown as Request).token;
  cookie = `ludock_session=${sessionToken}`;
  calls = [];
  beforeResponse = undefined;
  afterResponse = undefined;
  beforeInspect = undefined;
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
      if (beforeInspect) {
        inspectStarted();
        await beforeInspect;
      }
      return {
        Id: "physical-fixture",
        Name: "/fixture",
        Config: { Image: "alpine:latest", Labels: { "ludock.enable": "true" } },
        State: { Status: "running" },
        Mounts: [],
        NetworkSettings: { Ports: {} },
        Created: "2026-01-01T00:00:00Z",
      };
    },
  })) as unknown as typeof docker.getContainer;
  await refreshServers();
  serverId = listLogicalServers()[0].id;
  const app = express();
  app.use(authMiddleware);
  let disconnected!: () => void;
  clientClosed = new Promise((resolve) => {
    disconnected = resolve;
  });
  app.use((_req, res, next) => {
    res.once("close", () => {
      if (!res.writableFinished) disconnected();
    });
    next();
  });
  // An unfamiliar route demonstrates that policy travels with registration;
  // it cannot silently miss a separately maintained path/method allowlist.
  app.post(
    "/servers/:id/new-console-action",
    serverAction("console.execute", async (_req, res, context) => {
      calls.push(context.container.id);
      res.json({ ok: true });
    }),
  );
  app.get(
    "/servers/:id/stream",
    serverAction("logs.read", async (_req, res, context) => {
      calls.push(context.container.id);
      res.write("first chunk\n");
      if (beforeResponse) await beforeResponse;
      res.end("last chunk\n");
      if (afterResponse) await afterResponse;
    }),
  );
  app.post(
    "/servers/:id/failure",
    serverAction("server.stop", async () => {
      throw new AppError("FIXTURE_FAILURE", 409, "Fixture failure");
    }),
  );
  app.get("/servers/:id/response", (_req, res) => {
    // @ts-expect-error This fixture deliberately injects corrupt producer data.
    respond(res, okResponseSchema, output);
  });
  app.use(
    (error: unknown, _req: Request, res: Response, next: NextFunction) => {
      if (res.headersSent) {
        next(error);
        return;
      }
      const status =
        error instanceof AppError || error instanceof AuthorizationError
          ? error.statusCode
          : 500;
      res
        .status(status)
        .json({ error: error instanceof Error ? error.message : "Failure" });
    },
  );
  http = createServer(app);
  await new Promise<void>((resolve) => http.listen(0, "127.0.0.1", resolve));
  baseUrl = `http://127.0.0.1:${(http.address() as AddressInfo).port}`;
});
afterEach(async () => {
  releases.splice(0).forEach((release) => release());
  await new Promise<void>((resolve, reject) =>
    http.close((error) => (error ? reject(error) : resolve())),
  );
  await waitForLocksReleased();
  docker.listContainers = originalList;
  docker.getContainer = originalGet;
  closeDatabase();
});

describe("explicit server action policies", () => {
  it("enforces the declared capability on new paths and passes only a resolved binding to the action", async () => {
    setServerGrant(
      friend.id,
      serverId,
      ["server.view", "server.start", "server.stop"],
      admin,
    );
    assert.equal(
      (await friendFetch("/new-console-action", { method: "POST" })).status,
      403,
    );
    assert.deepEqual(calls, []);
    setServerGrant(
      friend.id,
      serverId,
      ["server.view", "console.execute"],
      admin,
    );
    assert.equal(
      (await friendFetch("/NEW-CONSOLE-ACTION/", { method: "POST" })).status,
      200,
    );
    assert.deepEqual(calls, ["physical-fixture"]);
  });
  it("uses the same capability for GET, HEAD, case and trailing-slash variants", async () => {
    setServerGrant(friend.id, serverId, ["server.view"], admin);
    for (const method of ["GET", "HEAD"])
      assert.equal((await friendFetch("/StReAm/", { method })).status, 403);
    assert.deepEqual(calls, []);
    setServerGrant(friend.id, serverId, ["server.view", "logs.read"], admin);
    const head = await friendFetch("/StReAm/", { method: "HEAD" });
    assert.equal(head.status, 200);
    assert.equal(await head.text(), "");
    assert.deepEqual(calls, ["physical-fixture"]);
  });
  it("retains locks after the response finishes until handler cleanup finishes", async () => {
    setServerGrant(friend.id, serverId, ["server.view", "logs.read"], admin);
    afterResponse = gate();
    const response = await friendFetch("/stream");
    assert.equal(await response.text(), "first chunk\nlast chunk\n");
    assert.equal(isServerBusy(serverId), true);
    assert.throws(() => acquireLocks([`server:${serverId}`]), /conflicting/);
    releases.splice(0).forEach((release) => release());
    await waitForLocksReleased();
    assert.equal(isServerBusy(serverId), false);
  });
  for (const revoke of ["grant", "session"] as const) {
    it(`closes a stream after ${revoke} revocation and retains its lock through cleanup`, async () => {
      setServerGrant(friend.id, serverId, ["server.view", "logs.read"], admin);
      beforeResponse = gate();
      const response = await friendFetch("/stream");
      const reader = response.body!.getReader();
      assert.equal((await reader.read()).done, false);
      if (revoke === "grant")
        setServerGrant(friend.id, serverId, ["server.view"], admin);
      else deleteUserSessions(friend.id);
      await assert.rejects(reader.read(), /terminated|aborted|socket/i);
      assert.equal(isServerBusy(serverId), true);
      releases.splice(0).forEach((release) => release());
      await waitForLocksReleased();
      assert.equal(isServerBusy(serverId), false);
    });
  }
  it("releases locks when an action fails before writing a response", async () => {
    setServerGrant(friend.id, serverId, ["server.view", "server.stop"], admin);
    const response = await friendFetch("/failure", { method: "POST" });
    assert.equal(response.status, 409);
    await response.text();
    await waitForLocksReleased();
    assert.equal(isServerBusy(serverId), false);
  });
  it("validates response contracts without leaking producer data or reporting a client error", async () => {
    output = { ok: true, privateToken: "fixture-private-value" };
    const valid = await friendFetch("/response");
    assert.equal(valid.status, 200);
    assert.deepEqual(await valid.json(), { ok: true });
    output = { ok: "true", privateToken: "fixture-private-value" };
    const invalid = await friendFetch("/response");
    assert.equal(invalid.status, 500);
    const body = await invalid.text();
    assert.match(body, /could not produce a valid response/);
    assert.doesNotMatch(body, /fixture-private-value|ZodError|invalid_type/);
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
    assert.deepEqual(calls, []);
    assert.equal(isServerBusy(serverId), false);
  });
});
