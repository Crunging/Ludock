import { serve, type Server } from "bun";
import { expect, afterEach, beforeEach, describe, it } from "bun:test";
import {
  auditHistoryQuerySchema, auditResponseSchema,
  operationResponseSchema, operationsResponseSchema,
} from "@ludock/shared";
import { createApp } from "../src/app.js";
import { createSession } from "../src/auth.js";
import { setServerGrant } from "./fixtures/grants.js";
import { closeDatabase, createUser, getDatabase, type SessionUser } from "../src/database.js";
import { listAuditHistory } from "../src/history.js";
import { reconcileServers } from "../src/identity.js";
import { dockerId } from "./fixtures/ids.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const admin: SessionUser = { id: crypto.randomUUID(), username: "History Admin", role: "admin" };
const viewer: SessionUser = { id: crypto.randomUUID(), username: "History Viewer", role: "viewer" };
let serverId: string;
let otherServerId: string;
let http: Server<unknown>;
let cookies: Map<string, string>;
const operationId = (index: number) => `00000000-0000-4000-8000-${index.toString().padStart(12, "0")}`;

beforeEach(() => {
  closeDatabase();
  cookies = new Map();
  for (const actor of [admin, viewer]) {
    createUser({ ...actor, passwordHash: "unused-history-fixture", disabled: false, createdAt: 0 });
    cookies.set(actor.id, `ludock_session=${createSession(actor, new Request("http://localhost")).token}`);
  }
  const logical = reconcileServers(["first", "second"].map((name) => ({
    containerId: dockerId(name), name, displayName: name, gameType: "minecraft", mounts: [],
  })));
  serverId = logical.find((server) => server.containerId === "first")!.id;
  otherServerId = logical.find((server) => server.containerId === "second")!.id;
  http = serve({ ...createApp({ frontendDist: false }), hostname: "127.0.0.1", port: 0 });
});

afterEach(async () => {
  await http?.stop(true);
  closeDatabase();
});

function insertOperation(index: number, options: {
  serverId?: string; actorId?: string;
} = {}) {
  const id = operationId(index);
  getDatabase().prepare(`INSERT INTO operations
    (id,server_id,actor_id,kind,status,phase,input_json,recovery_json,binding_revision,created_at,updated_at,result_json)
    VALUES (?,?,?,?,?,?,'{"privateInput":"/private/input"}','{"privateRecovery":"/private/recovery"}',1,?,?,?)`)
    .run(id, options.serverId ?? serverId, options.actorId ?? admin.id, "backup",
      "succeeded", "succeeded", index, index + 1, '{"copied":true}');
  return id;
}

function insertAudit(options: {
  userId?: string; details?: unknown;
} = {}) {
  return Number(getDatabase().prepare(`INSERT INTO audit_log
    (user_id,action,target_type,target_id,details_json,created_at)
    VALUES (?,?,'server',?,?,?)`).run(options.userId ?? null, "server.backup.succeeded",
    serverId, options.details === undefined ? null : JSON.stringify(options.details), 100).lastInsertRowid);
}

function request(path: string, actor: SessionUser | null = admin) {
  return fetch(new URL(path, http.url), { headers: actor ? { Cookie: cookies.get(actor.id)! } : {} });
}

async function operationPage(path: string, actor: SessionUser = admin) {
  const response = await request(path, actor);
  expect(response.status).toBe(200);
  return operationsResponseSchema.parse(await response.json());
}

describe("searchable operation history", () => {
  it("applies current server grants before pagination and never builds cursors from hidden rows", async () => {
    setServerGrant(viewer.id, serverId, ["server.view"], admin);
    insertOperation(1);
    insertOperation(2);
    insertOperation(3, { serverId: otherServerId });
    insertOperation(4, { serverId: otherServerId });
    const first = await operationPage("/api/v1/operations?limit=1", viewer);
    expect(first.operations.map((row) => row.id)).toStrictEqual([operationId(2)]);
    expect(first.nextCursor).toBeTruthy();
    const second = await operationPage(`/api/v1/operations?limit=1&cursor=${encodeURIComponent(first.nextCursor!)}`, viewer);
    expect(second.operations.map((row) => row.id)).toStrictEqual([operationId(1)]);
    expect(second.nextCursor).toBe(null);
    const hidden = await operationPage(`/api/v1/operations?serverId=${otherServerId}&limit=1`, viewer);
    expect(hidden).toStrictEqual({ operations: [], nextCursor: null });
    setServerGrant(viewer.id, serverId, [], admin);
    expect(await operationPage(`/api/v1/operations?limit=1&cursor=${encodeURIComponent(first.nextCursor!)}`, viewer)).toStrictEqual({
      operations: [], nextCursor: null,
    });
  });

  it("keeps server-specific lists scoped and direct details authorized and public", async () => {
    setServerGrant(viewer.id, serverId, ["server.view"], admin);
    const visibleId = insertOperation(1);
    const hiddenId = insertOperation(2, { serverId: otherServerId });
    const page = await operationPage(`/api/v1/servers/${serverId}/operations`, viewer);
    expect(page.operations.map((row) => row.id)).toStrictEqual([visibleId]);
    const detail = await request(`/api/v1/operations/${visibleId}`, viewer);
    expect(detail.status).toBe(200);
    const body = operationResponseSchema.parse(await detail.json());
    expect(body.operation.actor).toStrictEqual({ id: admin.id, name: admin.username });
    expect(JSON.stringify(body).includes("/private/")).toBe(false);
    expect(body.operation.result).toStrictEqual({ copied: true });
    for (const path of [`/api/v1/servers/${otherServerId}/operations`, `/api/v1/operations/${hiddenId}`]) {
      const denied = await request(path, viewer);
      expect(denied.status).toBe(404);
      expect((await denied.text()).includes(hiddenId)).toBe(false);
    }
    expect((await request(`/api/v1/servers/${serverId}/operations?serverId=${otherServerId}`)).status).toBe(400);
    expect((await request(`/api/v1/operations/${crypto.randomUUID()}`)).status).toBe(404);
  });

  it("does not disclose or search API token fingerprints", async () => {
    const privateActor = "api-token:fixture-auth-fingerprint";
    const id = insertOperation(1, { actorId: privateActor });
    for (const actor of ["api-token", "API token"]) {
      const page = await operationPage(`/api/v1/operations?actor=${encodeURIComponent(actor)}`);
      expect(page.operations.map((row) => row.actor)).toStrictEqual([{ id: "api-token", name: "API token" }]);
      expect(JSON.stringify(page).includes("fixture-auth-fingerprint")).toBe(false);
    }
    expect((await operationPage(`/api/v1/operations?actor=${encodeURIComponent(privateActor)}`)).operations).toStrictEqual([]);
    const detail = await request(`/api/v1/operations/${id}`);
    expect(detail.status).toBe(200);
    const body = await detail.text();
    expect(body.includes("fixture-auth-fingerprint")).toBe(false);
    expect(body.includes("/private/")).toBe(false);
  });
});

describe("searchable audit history", () => {
  it("sanitizes token actors from both stored details and linked operations", () => {
    const privateActor = "api-token:fixture-auth-fingerprint";
    const linked = insertOperation(1, { actorId: privateActor });
    insertAudit({ details: { actorId: privateActor } });
    insertAudit({ details: { operationId: linked } });
    const page = listAuditHistory(auditHistoryQuerySchema.parse({ actor: "API token" }));
    expect(page.entries.length).toBe(2);
    expect(page.entries.every((entry) => entry.actor?.id === "api-token" && entry.actor.name === "API token")).toBeTruthy();
    expect(JSON.stringify(page).includes("fixture-auth-fingerprint")).toBe(false);
    expect(listAuditHistory(auditHistoryQuerySchema.parse({ actor: privateActor })).entries).toStrictEqual([]);
  });

  it("requires administrator access and returns the paginated audit contract", async () => {
    insertAudit({ userId: admin.id });
    insertAudit({ userId: admin.id });
    const response = await request("/api/v1/audit?limit=1");
    expect(response.status).toBe(200);
    const body = auditResponseSchema.parse(await response.json());
    expect(body.entries.length).toBe(1);
    expect(body.nextCursor).toBeTruthy();
    expect((await request("/api/v1/audit", viewer)).status).toBe(403);
    for (const path of ["/api/v1/audit", "/api/v1/operations"]) {
      expect((await request(path, null)).status).toBe(401);
    }
  });
});
