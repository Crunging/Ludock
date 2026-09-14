import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { serve, type Server } from "bun";
import { afterEach, beforeEach, describe, it } from "bun:test";
import {
  auditHistoryQuerySchema, auditResponseSchema, operationHistoryQuerySchema,
  operationResponseSchema, operationsResponseSchema, type OperationStatus,
} from "@ludock/shared";
import { createApp } from "../src/app.js";
import { createSession } from "../src/auth.js";
import { setServerGrant } from "../src/authorization.js";
import { closeDatabase, createUser, deleteUser, getDatabase, updateUserAccess, type SessionUser } from "../src/database.js";
import { listAuditHistory, listOperationHistory } from "../src/history.js";
import { reconcileServers } from "../src/identity.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const admin: SessionUser = { id: randomUUID(), username: "History Admin", role: "admin" };
const viewer: SessionUser = { id: randomUUID(), username: "History Viewer", role: "viewer" };
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
    containerId: name, name, displayName: name, gameType: "minecraft", mounts: [],
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
  serverId?: string; actorId?: string; kind?: string; status?: OperationStatus; createdAt?: number;
} = {}) {
  const id = operationId(index);
  getDatabase().prepare(`INSERT INTO operations
    (id,server_id,actor_id,kind,status,phase,input_json,recovery_json,binding_revision,created_at,updated_at,result_json)
    VALUES (?,?,?,?,?,?,'{"privateInput":"/private/input"}','{"privateRecovery":"/private/recovery"}',1,?,?,?)`)
    .run(id, options.serverId ?? serverId, options.actorId ?? admin.id, options.kind ?? "backup",
      options.status ?? "succeeded", options.status ?? "succeeded", options.createdAt ?? index,
      (options.createdAt ?? index) + 1, '{"copied":true}');
  return id;
}

function insertAudit(options: {
  userId?: string; action?: string; targetId?: string; createdAt?: number; details?: unknown; rawDetails?: string;
} = {}) {
  return Number(getDatabase().prepare(`INSERT INTO audit_log
    (user_id,action,target_type,target_id,details_json,created_at)
    VALUES (?,?,'server',?,?,?)`).run(options.userId ?? null, options.action ?? "server.backup.succeeded",
    options.targetId ?? serverId, options.rawDetails ?? (options.details === undefined ? null : JSON.stringify(options.details)),
    options.createdAt ?? 100).lastInsertRowid);
}

function request(path: string, actor: SessionUser | null = admin) {
  return fetch(new URL(path, http.url), { headers: actor ? { Cookie: cookies.get(actor.id)! } : {} });
}

async function operationPage(path: string, actor: SessionUser = admin) {
  const response = await request(path, actor);
  assert.equal(response.status, 200);
  return operationsResponseSchema.parse(await response.json());
}

describe("searchable operation history", () => {
  it("reaches operations beyond the old 100-row history limit", async () => {
    for (let index = 1; index <= 130; index++) insertOperation(index);
    for (const path of ["/api/v1/operations", `/api/v1/servers/${serverId}/operations`]) {
      const seen: string[] = [];
      const pageSizes: number[] = [];
      let cursor: string | null = null;
      do {
        const page = await operationPage(`${path}${cursor ? `?cursor=${encodeURIComponent(cursor)}` : ""}`);
        pageSizes.push(page.operations.length);
        seen.push(...page.operations.map((row) => row.id));
        cursor = page.nextCursor;
        assert.ok(pageSizes.length <= 3, "pagination must finish without repeating a page");
      } while (cursor);
      assert.deepEqual(pageSizes, [50, 50, 30]);
      assert.deepEqual(seen, Array.from({ length: 130 }, (_, index) => operationId(130 - index)));
    }
  });

  it("pages tied timestamps deterministically without repeating newer insertions", async () => {
    for (let index = 1; index <= 5; index++) insertOperation(index, { createdAt: index === 1 ? 99 : 100 });
    const first = await operationPage("/api/v1/operations?limit=2");
    assert.deepEqual(first.operations.map((row) => row.id), [operationId(5), operationId(4)]);
    assert.ok(first.nextCursor);
    insertOperation(6, { createdAt: 101 });
    const second = await operationPage(`/api/v1/operations?limit=2&cursor=${encodeURIComponent(first.nextCursor)}`);
    assert.deepEqual(second.operations.map((row) => row.id), [operationId(3), operationId(2)]);
    assert.ok(second.nextCursor);
    const final = await operationPage(`/api/v1/operations?limit=2&cursor=${encodeURIComponent(second.nextCursor)}`);
    assert.deepEqual(final.operations.map((row) => row.id), [operationId(1)]);
    assert.equal(final.nextCursor, null);
  });

  it("combines server, actor, action, status, and inclusive dates before limiting", () => {
    insertOperation(1, { createdAt: 100 });
    insertOperation(2, { createdAt: 200 });
    insertOperation(3, { createdAt: 201 });
    insertOperation(4, { createdAt: 150, serverId: otherServerId });
    insertOperation(5, { createdAt: 150, actorId: viewer.id });
    insertOperation(6, { createdAt: 150, kind: "restore" });
    insertOperation(7, { createdAt: 150, status: "failed" });
    const query = operationHistoryQuerySchema.parse({
      serverId, actor: "ADMIN", action: "ACK", status: "succeeded", from: 100, to: 200, limit: 1,
    });
    const first = listOperationHistory(admin, query);
    assert.deepEqual(first.operations.map((row) => row.id), [operationId(2)]);
    assert.ok(first.nextCursor);
    const second = listOperationHistory(admin, { ...query, cursor: first.nextCursor });
    assert.deepEqual(second.operations.map((row) => row.id), [operationId(1)]);
    assert.equal(second.nextCursor, null);
    assert.equal(listOperationHistory(admin, operationHistoryQuerySchema.parse({ actor: admin.id })).operations.length, 6);
    assert.equal(listOperationHistory(admin, operationHistoryQuerySchema.parse({ action: "REST" })).operations[0].kind, "restore");
    assert.deepEqual(listOperationHistory(admin, operationHistoryQuerySchema.parse({ actor: "%' OR 1=1 --" })).operations, []);
    insertOperation(8, { kind: "literal_%_action" });
    assert.deepEqual(listOperationHistory(admin, operationHistoryQuerySchema.parse({ action: "_%_" })).operations.map((row) => row.id), [operationId(8)]);
  });

  it("applies current server grants before pagination and never builds cursors from hidden rows", async () => {
    setServerGrant(viewer.id, serverId, ["server.view"], admin);
    insertOperation(1);
    insertOperation(2);
    insertOperation(3, { serverId: otherServerId });
    insertOperation(4, { serverId: otherServerId });
    const first = await operationPage("/api/v1/operations?limit=1", viewer);
    assert.deepEqual(first.operations.map((row) => row.id), [operationId(2)]);
    assert.ok(first.nextCursor);
    const second = await operationPage(`/api/v1/operations?limit=1&cursor=${encodeURIComponent(first.nextCursor)}`, viewer);
    assert.deepEqual(second.operations.map((row) => row.id), [operationId(1)]);
    assert.equal(second.nextCursor, null);
    const hidden = await operationPage(`/api/v1/operations?serverId=${otherServerId}&limit=1`, viewer);
    assert.deepEqual(hidden, { operations: [], nextCursor: null });
    setServerGrant(viewer.id, serverId, [], admin);
    assert.deepEqual(await operationPage(`/api/v1/operations?limit=1&cursor=${encodeURIComponent(first.nextCursor)}`, viewer), {
      operations: [], nextCursor: null,
    });
  });

  it("hides history when bindings become unavailable or an account loses its role", async () => {
    setServerGrant(viewer.id, serverId, ["server.view"], admin);
    insertOperation(1);
    reconcileServers([]);
    assert.deepEqual(await operationPage("/api/v1/operations", viewer), { operations: [], nextCursor: null });
    assert.equal((await operationPage("/api/v1/operations", admin)).operations.length, 1);
    updateUserAccess(admin.id, "viewer", false);
    assert.deepEqual(await operationPage("/api/v1/operations", admin), { operations: [], nextCursor: null });
  });

  it("keeps server-specific lists scoped and direct details authorized and public", async () => {
    setServerGrant(viewer.id, serverId, ["server.view"], admin);
    const visibleId = insertOperation(1);
    const hiddenId = insertOperation(2, { serverId: otherServerId });
    const page = await operationPage(`/api/v1/servers/${serverId}/operations`, viewer);
    assert.deepEqual(page.operations.map((row) => row.id), [visibleId]);
    const detail = await request(`/api/v1/operations/${visibleId}`, viewer);
    assert.equal(detail.status, 200);
    const body = operationResponseSchema.parse(await detail.json());
    assert.deepEqual(body.operation.actor, { id: admin.id, name: admin.username });
    assert.equal(JSON.stringify(body).includes("/private/"), false);
    assert.deepEqual(body.operation.result, { copied: true });
    for (const path of [`/api/v1/servers/${otherServerId}/operations`, `/api/v1/operations/${hiddenId}`]) {
      const denied = await request(path, viewer);
      assert.equal(denied.status, 404);
      assert.equal((await denied.text()).includes(hiddenId), false);
    }
    assert.equal((await request(`/api/v1/servers/${serverId}/operations?serverId=${otherServerId}`)).status, 400);
    assert.equal((await request(`/api/v1/operations/${randomUUID()}`)).status, 404);
  });

  it("does not disclose or search API token fingerprints", async () => {
    const privateActor = "api-token:fixture-auth-fingerprint";
    const id = insertOperation(1, { actorId: privateActor });
    for (const actor of ["api-token", "API token"]) {
      const page = await operationPage(`/api/v1/operations?actor=${encodeURIComponent(actor)}`);
      assert.deepEqual(page.operations.map((row) => row.actor), [{ id: "api-token", name: "API token" }]);
      assert.equal(JSON.stringify(page).includes("fixture-auth-fingerprint"), false);
    }
    assert.deepEqual((await operationPage(`/api/v1/operations?actor=${encodeURIComponent(privateActor)}`)).operations, []);
    const detail = await request(`/api/v1/operations/${id}`);
    assert.equal(detail.status, 200);
    const body = await detail.text();
    assert.equal(body.includes("fixture-auth-fingerprint"), false);
    assert.equal(body.includes("/private/"), false);
  });
});

describe("searchable audit history", () => {
  it("pages tied events and retains inclusive filter conjunction before limiting", () => {
    const linkedOperation = insertOperation(1, { status: "failed" });
    const match = { userId: admin.id, details: { operationId: linkedOperation } };
    const older = insertAudit({ ...match, createdAt: 100 });
    const newer = insertAudit({ ...match, createdAt: 200 });
    insertAudit({ ...match, createdAt: 201 });
    insertAudit({ ...match, createdAt: 150, targetId: otherServerId });
    insertAudit({ ...match, createdAt: 150, userId: viewer.id });
    insertAudit({ ...match, createdAt: 150, action: "server.backup.failed" });
    insertAudit({ ...match, createdAt: 150, action: "server.restore.succeeded" });
    insertAudit({ ...match, createdAt: 150, details: { operationId: randomUUID() } });
    const query = auditHistoryQuerySchema.parse({
      serverId, operationId: linkedOperation, actor: "ADMIN", action: "ACK", status: "succeeded", from: 100, to: 200, limit: 1,
    });
    const first = listAuditHistory(query);
    assert.deepEqual(first.entries.map((row) => row.id), [newer]);
    assert.equal(first.entries[0].status, "succeeded", "audit outcome does not track the operation's later status");
    assert.ok(first.nextCursor);
    insertAudit({ ...match, createdAt: 200 });
    const second = listAuditHistory({ ...query, cursor: first.nextCursor });
    assert.deepEqual(second.entries.map((row) => row.id), [older]);
    assert.equal(second.nextCursor, null);
    const tied = listAuditHistory(auditHistoryQuerySchema.parse({ from: 200, to: 200, limit: 1 }));
    assert.ok(tied.nextCursor);
    assert.deepEqual(listAuditHistory(auditHistoryQuerySchema.parse({
      from: 200, to: 200, limit: 1, cursor: tied.nextCursor,
    })).entries.map((row) => row.id), [newer]);
  });

  it("returns recorded actors and safe operation links while preserving incomplete older events", () => {
    const linked = insertOperation(1, { actorId: viewer.id });
    const direct = insertAudit({ userId: admin.id, details: { actorId: viewer.id, operationId: linked } });
    const fallback = insertAudit({ details: { operationId: linked } });
    const recorded = insertAudit({ details: { actorId: viewer.id } });
    const wrongServer = insertAudit({ targetId: otherServerId, details: { operationId: linked } });
    const invalidLink = insertAudit({ details: { operationId: "not-an-operation" } });
    const invalidDetails = insertAudit({ rawDetails: "{invalid-json" });
    const failedLogin = insertAudit({ action: "auth.login.failed" });
    const entries = listAuditHistory(auditHistoryQuerySchema.parse({})).entries;
    const byId = (id: number) => entries.find((entry) => entry.id === id)!;
    assert.deepEqual(byId(direct).actor, { id: admin.id, name: admin.username });
    assert.deepEqual(byId(fallback).actor, { id: viewer.id, name: viewer.username });
    assert.deepEqual(byId(recorded).actor, { id: viewer.id, name: viewer.username });
    assert.equal(byId(wrongServer).actor, null);
    assert.equal(byId(fallback).operationId, linked);
    assert.equal(byId(invalidLink).operationId, null);
    assert.equal(byId(invalidDetails).details, null);
    assert.equal(byId(invalidDetails).actor, null);
    assert.equal(byId(failedLogin).status, "failed");
    assert.deepEqual(listAuditHistory(auditHistoryQuerySchema.parse({ actor: "VIEWER" })).entries.map((row) => row.id), [recorded, fallback]);
  });

  it("sanitizes token actors from both stored details and linked operations", () => {
    const privateActor = "api-token:fixture-auth-fingerprint";
    const linked = insertOperation(1, { actorId: privateActor });
    insertAudit({ details: { actorId: privateActor } });
    insertAudit({ details: { operationId: linked } });
    const page = listAuditHistory(auditHistoryQuerySchema.parse({ actor: "API token" }));
    assert.equal(page.entries.length, 2);
    assert.ok(page.entries.every((entry) => entry.actor?.id === "api-token" && entry.actor.name === "API token"));
    assert.equal(JSON.stringify(page).includes("fixture-auth-fingerprint"), false);
    assert.deepEqual(listAuditHistory(auditHistoryQuerySchema.parse({ actor: privateActor })).entries, []);
  });

  it("retains recorded actor identifiers after the account has been deleted", () => {
    const linked = insertOperation(1, { actorId: viewer.id });
    const queued = insertAudit({ userId: viewer.id, action: "server.backup.queued", details: { operationId: linked } });
    const finished = insertAudit({ userId: viewer.id, details: { actorId: viewer.id } });
    deleteUser(viewer.id);
    const operations = listOperationHistory(admin, operationHistoryQuerySchema.parse({ actor: viewer.id }));
    assert.deepEqual(operations.operations.map((row) => row.actor), [{ id: viewer.id, name: null }]);
    const audit = listAuditHistory(auditHistoryQuerySchema.parse({ actor: viewer.id }));
    assert.deepEqual(audit.entries.map((row) => row.id), [finished, queued]);
    assert.ok(audit.entries.every((entry) => entry.actor?.id === viewer.id && entry.actor.name === null));
  });

  it("requires administrator access and returns the paginated audit contract", async () => {
    insertAudit({ userId: admin.id });
    insertAudit({ userId: admin.id });
    const response = await request("/api/v1/audit?limit=1");
    assert.equal(response.status, 200);
    const body = auditResponseSchema.parse(await response.json());
    assert.equal(body.entries.length, 1);
    assert.ok(body.nextCursor);
    assert.equal((await request("/api/v1/audit", viewer)).status, 403);
    for (const path of ["/api/v1/audit", "/api/v1/operations"]) {
      assert.equal((await request(path, null)).status, 401);
    }
  });
});

describe("history query validation", () => {
  it("rejects invalid limits, timestamps, statuses, identifiers, and cursor payloads", async () => {
    for (const path of ["/api/v1/audit", "/api/v1/operations", `/api/v1/servers/${serverId}/operations`]) {
      for (const query of ["limit=0", "limit=251", "limit=1.5", "from=-1", "to=tomorrow", "from=200&to=100",
        "status=unknown", "serverId=invalid", "actor=", "cursor=invalid", "cursor=e30"]) {
        const response = await request(`${path}?${query}`);
        assert.equal(response.status, 400, `${path}?${query}`);
        await response.body?.cancel();
      }
    }
    assert.equal((await request("/api/v1/audit?operationId=invalid")).status, 400);
  });

  it("rejects cursors reused for different filters or history kinds", async () => {
    insertOperation(1);
    insertOperation(2);
    const first = await operationPage("/api/v1/operations?limit=1");
    assert.ok(first.nextCursor);
    const cursor = encodeURIComponent(first.nextCursor);
    for (const path of [`/api/v1/operations?action=restore&cursor=${cursor}`, `/api/v1/audit?cursor=${cursor}`]) {
      const response = await request(path);
      assert.equal(response.status, 400);
      assert.equal((await response.json() as { code: string }).code, "INVALID_HISTORY_CURSOR");
    }
  });

  it("accepts equivalent server filters regardless of query field order", async () => {
    insertOperation(1);
    insertOperation(2);
    const first = await operationPage(`/api/v1/servers/${serverId}/operations?limit=1&action=backup`);
    assert.ok(first.nextCursor);
    const second = await operationPage(`/api/v1/servers/${serverId}/operations?serverId=${serverId}&action=backup&limit=1&cursor=${encodeURIComponent(first.nextCursor)}`);
    assert.deepEqual(second.operations.map((operation) => operation.id), [operationId(1)]);
    assert.equal(second.nextCursor, null);
  });
});
