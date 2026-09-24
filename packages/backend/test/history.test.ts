import { serve, type Server } from "bun";
import { expect, afterEach, beforeEach, describe, it } from "bun:test";
import {
  auditHistoryQuerySchema, auditResponseSchema, operationHistoryQuerySchema,
  operationResponseSchema, operationsResponseSchema, type OperationStatus,
} from "@ludock/shared";
import { createApp } from "../src/app.js";
import { createSession } from "../src/auth.js";
import { setServerGrant } from "./fixtures/grants.js";
import { closeDatabase, createUser, deleteUser, getDatabase, updateUserAccess, type SessionUser } from "../src/database.js";
import { listAuditHistory, listOperationHistory } from "../src/history.js";
import { reconcileServers } from "../src/identity.js";

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
  expect(response.status).toBe(200);
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
        expect(pageSizes.length <= 3, "pagination must finish without repeating a page").toBeTruthy();
      } while (cursor);
      expect(pageSizes).toStrictEqual([50, 50, 30]);
      expect(seen).toStrictEqual(Array.from({ length: 130 }, (_, index) => operationId(130 - index)));
    }
  });

  it("pages tied timestamps deterministically without repeating newer insertions", async () => {
    for (let index = 1; index <= 5; index++) insertOperation(index, { createdAt: index === 1 ? 99 : 100 });
    const first = await operationPage("/api/v1/operations?limit=2");
    expect(first.operations.map((row) => row.id)).toStrictEqual([operationId(5), operationId(4)]);
    expect(first.nextCursor).toBeTruthy();
    insertOperation(6, { createdAt: 101 });
    const second = await operationPage(`/api/v1/operations?limit=2&cursor=${encodeURIComponent(first.nextCursor)}`);
    expect(second.operations.map((row) => row.id)).toStrictEqual([operationId(3), operationId(2)]);
    expect(second.nextCursor).toBeTruthy();
    const final = await operationPage(`/api/v1/operations?limit=2&cursor=${encodeURIComponent(second.nextCursor)}`);
    expect(final.operations.map((row) => row.id)).toStrictEqual([operationId(1)]);
    expect(final.nextCursor).toBe(null);
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
    expect(first.operations.map((row) => row.id)).toStrictEqual([operationId(2)]);
    expect(first.nextCursor).toBeTruthy();
    const second = listOperationHistory(admin, { ...query, cursor: first.nextCursor });
    expect(second.operations.map((row) => row.id)).toStrictEqual([operationId(1)]);
    expect(second.nextCursor).toBe(null);
    expect(listOperationHistory(admin, operationHistoryQuerySchema.parse({ actor: admin.id })).operations.length).toBe(6);
    expect(listOperationHistory(admin, operationHistoryQuerySchema.parse({ action: "REST" })).operations[0].kind).toBe("restore");
    expect(listOperationHistory(admin, operationHistoryQuerySchema.parse({ actor: "%' OR 1=1 --" })).operations).toStrictEqual([]);
    insertOperation(8, { kind: "literal_%_action" });
    expect(listOperationHistory(admin, operationHistoryQuerySchema.parse({ action: "_%_" })).operations.map((row) => row.id)).toStrictEqual([operationId(8)]);
  });

  it("applies current server grants before pagination and never builds cursors from hidden rows", async () => {
    setServerGrant(viewer.id, serverId, ["server.view"], admin);
    insertOperation(1);
    insertOperation(2);
    insertOperation(3, { serverId: otherServerId });
    insertOperation(4, { serverId: otherServerId });
    const first = await operationPage("/api/v1/operations?limit=1", viewer);
    expect(first.operations.map((row) => row.id)).toStrictEqual([operationId(2)]);
    expect(first.nextCursor).toBeTruthy();
    const second = await operationPage(`/api/v1/operations?limit=1&cursor=${encodeURIComponent(first.nextCursor)}`, viewer);
    expect(second.operations.map((row) => row.id)).toStrictEqual([operationId(1)]);
    expect(second.nextCursor).toBe(null);
    const hidden = await operationPage(`/api/v1/operations?serverId=${otherServerId}&limit=1`, viewer);
    expect(hidden).toStrictEqual({ operations: [], nextCursor: null });
    setServerGrant(viewer.id, serverId, [], admin);
    expect(await operationPage(`/api/v1/operations?limit=1&cursor=${encodeURIComponent(first.nextCursor)}`, viewer)).toStrictEqual({
      operations: [], nextCursor: null,
    });
  });

  it("hides history when bindings become unavailable or an account loses its role", async () => {
    setServerGrant(viewer.id, serverId, ["server.view"], admin);
    insertOperation(1);
    reconcileServers([]);
    expect(await operationPage("/api/v1/operations", viewer)).toStrictEqual({ operations: [], nextCursor: null });
    expect((await operationPage("/api/v1/operations", admin)).operations.length).toBe(1);
    updateUserAccess(admin.id, "viewer", false);
    expect(await operationPage("/api/v1/operations", admin)).toStrictEqual({ operations: [], nextCursor: null });
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
    insertAudit({ ...match, createdAt: 150, details: { operationId: crypto.randomUUID() } });
    const query = auditHistoryQuerySchema.parse({
      serverId, operationId: linkedOperation, actor: "ADMIN", action: "ACK", status: "succeeded", from: 100, to: 200, limit: 1,
    });
    const first = listAuditHistory(query);
    expect(first.entries.map((row) => row.id)).toStrictEqual([newer]);
    expect(first.entries[0].status, "audit outcome does not track the operation's later status").toBe("succeeded");
    expect(first.nextCursor).toBeTruthy();
    insertAudit({ ...match, createdAt: 200 });
    const second = listAuditHistory({ ...query, cursor: first.nextCursor });
    expect(second.entries.map((row) => row.id)).toStrictEqual([older]);
    expect(second.nextCursor).toBe(null);
    const tied = listAuditHistory(auditHistoryQuerySchema.parse({ from: 200, to: 200, limit: 1 }));
    expect(tied.nextCursor).toBeTruthy();
    expect(listAuditHistory(auditHistoryQuerySchema.parse({
      from: 200, to: 200, limit: 1, cursor: tied.nextCursor,
    })).entries.map((row) => row.id)).toStrictEqual([newer]);
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
    expect(byId(direct).actor).toStrictEqual({ id: admin.id, name: admin.username });
    expect(byId(fallback).actor).toStrictEqual({ id: viewer.id, name: viewer.username });
    expect(byId(recorded).actor).toStrictEqual({ id: viewer.id, name: viewer.username });
    expect(byId(wrongServer).actor).toBe(null);
    expect(byId(fallback).operationId).toBe(linked);
    expect(byId(invalidLink).operationId).toBe(null);
    expect(byId(invalidDetails).details).toBe(null);
    expect(byId(invalidDetails).actor).toBe(null);
    expect(byId(failedLogin).status).toBe("failed");
    expect(listAuditHistory(auditHistoryQuerySchema.parse({ actor: "VIEWER" })).entries.map((row) => row.id)).toStrictEqual([recorded, fallback]);
  });

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

  it("retains recorded actor identifiers after the account has been deleted", () => {
    const linked = insertOperation(1, { actorId: viewer.id });
    const queued = insertAudit({ userId: viewer.id, action: "server.backup.queued", details: { operationId: linked } });
    const finished = insertAudit({ userId: viewer.id, details: { actorId: viewer.id } });
    deleteUser(viewer.id);
    const operations = listOperationHistory(admin, operationHistoryQuerySchema.parse({ actor: viewer.id }));
    expect(operations.operations.map((row) => row.actor)).toStrictEqual([{ id: viewer.id, name: null }]);
    const audit = listAuditHistory(auditHistoryQuerySchema.parse({ actor: viewer.id }));
    expect(audit.entries.map((row) => row.id)).toStrictEqual([finished, queued]);
    expect(audit.entries.every((entry) => entry.actor?.id === viewer.id && entry.actor.name === null)).toBeTruthy();
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

describe("history query validation", () => {
  it("rejects invalid limits, timestamps, statuses, identifiers, and cursor payloads", async () => {
    for (const path of ["/api/v1/audit", "/api/v1/operations", `/api/v1/servers/${serverId}/operations`]) {
      for (const query of ["limit=0", "limit=251", "limit=1.5", "from=-1", "to=tomorrow", "from=200&to=100",
        "status=unknown", "serverId=invalid", "actor=", "cursor=invalid", "cursor=e30"]) {
        const response = await request(`${path}?${query}`);
        expect(response.status, `${path}?${query}`).toBe(400);
        await response.body?.cancel();
      }
    }
    expect((await request("/api/v1/audit?operationId=invalid")).status).toBe(400);
  });

  it("rejects cursors reused for different filters or history kinds", async () => {
    insertOperation(1);
    insertOperation(2);
    const first = await operationPage("/api/v1/operations?limit=1");
    expect(first.nextCursor).toBeTruthy();
    const cursor = encodeURIComponent(first.nextCursor);
    for (const path of [`/api/v1/operations?action=restore&cursor=${cursor}`, `/api/v1/audit?cursor=${cursor}`]) {
      const response = await request(path);
      expect(response.status).toBe(400);
      expect((await response.json() as { code: string }).code).toBe("INVALID_HISTORY_CURSOR");
    }
  });

  it("accepts equivalent server filters regardless of query field order", async () => {
    insertOperation(1);
    insertOperation(2);
    const first = await operationPage(`/api/v1/servers/${serverId}/operations?limit=1&action=backup`);
    expect(first.nextCursor).toBeTruthy();
    const second = await operationPage(`/api/v1/servers/${serverId}/operations?serverId=${serverId}&action=backup&limit=1&cursor=${encodeURIComponent(first.nextCursor)}`);
    expect(second.operations.map((operation) => operation.id)).toStrictEqual([operationId(1)]);
    expect(second.nextCursor).toBe(null);
  });
});
