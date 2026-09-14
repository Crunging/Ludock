import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";
import {
  notificationDeliveriesResponseSchema,
  notificationDeliveryResponseSchema,
} from "@ludock/shared";
import { createApp } from "../src/app.js";
import { createSession } from "../src/auth.js";
import { closeDatabase, createUser, getDatabase, listAuditLog } from "../src/database.js";
import {
  configureNotifications,
  deliverNotifications,
  notifyEvent,
  queueTestNotification,
} from "../src/notifications.js";
import type { HttpServer } from "../src/routes/request.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const webhook = "https://discord.com/api/webhooks/123456/route-fixture-secret";
const roles = ["admin", "operator", "viewer"] as const;
type Role = typeof roles[number];
let cookies: Record<Role, string>;
let app: ReturnType<typeof createApp>;
let outbound: ReturnType<typeof spyOn<typeof globalThis, "fetch">>;
const server: HttpServer = { requestIP: () => null, timeout: () => {} };

beforeEach(() => {
  closeDatabase();
  cookies = { admin: "", operator: "", viewer: "" };
  for (const role of roles) {
    const user = { id: crypto.randomUUID(), username: role, role };
    createUser({ ...user, disabled: false, passwordHash: "unused-fixture", createdAt: 1 });
    cookies[role] = `ludock_session=${createSession(user, new Request("http://localhost")).token}`;
  }
  app = createApp({ frontendDist: false });
  outbound = spyOn(globalThis, "fetch").mockRejectedValue(new Error("Unexpected outbound request"));
});

afterEach(() => {
  const calls = outbound.mock.calls.length;
  mock.restore();
  closeDatabase();
  assert.equal(calls, 0, "notification routes only enqueue work; tests never contact Discord");
});

function request(pathname: string, method = "GET", role: Role | null = "admin", body?: unknown) {
  return app.fetch(new Request(`http://localhost${pathname}`, {
    method,
    headers: {
      ...(role ? { Cookie: cookies[role] } : {}),
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  }), server);
}

async function failDelivery() {
  for (let attempt = 0; attempt < 5; attempt++) {
    getDatabase().prepare("UPDATE notification_deliveries SET next_attempt_at=0 WHERE state='queued'").run();
    await deliverNotifications((async () => {
      throw new Error(`Private provider error: ${webhook}`);
    }) as typeof fetch);
  }
}

describe("notification troubleshooting routes", () => {
  it("requires an administrator to list, test, or retry notifications", async () => {
    configureNotifications(true, webhook);
    const delivery = queueTestNotification();
    await failDelivery();
    const endpoints = [
      ["/api/v1/notifications/deliveries", "GET"],
      ["/api/v1/notifications/test", "POST"],
      [`/api/v1/notifications/deliveries/${delivery.id}/retry`, "POST"],
    ];
    for (const role of [null, "operator", "viewer"] as const) {
      for (const [pathname, method] of endpoints) {
        const response = await request(pathname, method, role);
        assert.equal(response.status, role === null ? 401 : 403);
        assert.doesNotMatch(await response.text(), new RegExp(`${delivery.id}|route-fixture-secret`));
      }
    }
    const stored = getDatabase().prepare("SELECT state,attempts FROM notification_deliveries").all();
    assert.deepEqual(stored, [{ state: "failed", attempts: 5 }]);
    assert.deepEqual(listAuditLog(10), []);
  });

  it("queues distinct test notifications and audits only their delivery IDs", async () => {
    configureNotifications(true, webhook);
    const ids = new Set<string>();
    for (let index = 0; index < 2; index++) {
      const response = await request("/api/v1/notifications/test", "POST");
      assert.equal(response.status, 202);
      const { delivery } = notificationDeliveryResponseSchema.parse(await response.json());
      assert.equal(delivery.kind, "test");
      assert.equal(delivery.state, "queued");
      assert.equal(delivery.attempts, 0);
      assert.equal(delivery.lastAttemptAt, null);
      assert.equal(delivery.deliveredAt, null);
      assert.equal(delivery.lastFailure, null);
      assert.equal(delivery.retryable, false);
      assert.ok(delivery.nextAttemptAt);
      ids.add(delivery.id);
    }
    assert.equal(ids.size, 2);
    const audit = listAuditLog(10);
    assert.equal(audit.length, 2);
    for (const entry of audit) {
      assert.equal(entry.action, "notifications.test_queued");
      assert.equal(entry.username, "admin");
      assert.equal(entry.targetType, "settings");
      assert.equal(entry.targetId, null);
      assert.ok(ids.has((entry.details as { deliveryId: string }).deliveryId));
      assert.deepEqual(Object.keys(entry.details as object), ["deliveryId"]);
    }
    assert.doesNotMatch(JSON.stringify(audit), /route-fixture-secret|payload|webhook|event_key/);
  });

  it("returns delivery status and sanitized failure reasons without messages or event keys", async () => {
    configureNotifications(true, webhook);
    notifyEvent("private-event-key", "Private server event payload");
    await failDelivery();
    const delivered = queueTestNotification();
    await deliverNotifications((async () => new Response(null, { status: 204 })) as typeof fetch);
    const pending = queueTestNotification();
    await deliverNotifications((async () => new Response("Private Discord response", { status: 429 })) as typeof fetch);
    const response = await request("/api/v1/notifications/deliveries");
    assert.equal(response.status, 200);
    assert.equal(response.headers.get("Cache-Control"), "no-store");
    const raw = await response.text();
    assert.doesNotMatch(raw, /route-fixture-secret|private-event-key|Private|payload|webhook|event_key/i);
    const { deliveries } = notificationDeliveriesResponseSchema.parse(JSON.parse(raw));
    assert.equal(deliveries.length, 3);
    const failure = deliveries.find((delivery) => delivery.kind === "event")!;
    assert.equal(failure.state, "failed");
    assert.equal(failure.attempts, 5);
    assert.ok(failure.lastFailure);
    assert.ok(failure.lastAttemptAt);
    assert.equal(failure.nextAttemptAt, null);
    assert.equal(failure.retryable, true);
    assert.ok(deliveries.find((delivery) => delivery.id === delivered.id)?.deliveredAt);
    const retry = deliveries.find((delivery) => delivery.id === pending.id)!;
    assert.equal(retry.state, "queued");
    assert.equal(retry.attempts, 1);
    assert.ok(retry.lastFailure);
    assert.ok(retry.nextAttemptAt);
  });

  it("validates retry IDs and rejects missing or nonretryable deliveries", async () => {
    configureNotifications(true, webhook);
    const malformed = await request("/api/v1/notifications/deliveries/not-a-uuid/retry", "POST");
    assert.equal(malformed.status, 400);
    assert.equal((await malformed.json() as { code: string }).code, "INVALID_REQUEST");
    const missing = await request(`/api/v1/notifications/deliveries/${crypto.randomUUID()}/retry`, "POST");
    assert.equal(missing.status, 404);
    await missing.body?.cancel();
    const delivery = queueTestNotification();
    const queued = await request(`/api/v1/notifications/deliveries/${delivery.id}/retry`, "POST");
    assert.equal(queued.status, 409);
    await queued.body?.cancel();
    await deliverNotifications((async () => new Response(null, { status: 204 })) as typeof fetch);
    const delivered = await request(`/api/v1/notifications/deliveries/${delivery.id}/retry`, "POST");
    assert.equal(delivered.status, 409);
    await delivered.body?.cancel();
    assert.deepEqual(listAuditLog(10), []);
  });

  it("requires saved enabled settings for tests and retries", async () => {
    const unconfigured = await request("/api/v1/notifications/test", "POST");
    assert.equal(unconfigured.status, 400);
    await unconfigured.body?.cancel();
    configureNotifications(true, webhook);
    const delivery = queueTestNotification();
    await failDelivery();
    configureNotifications(false);
    for (const pathname of [
      "/api/v1/notifications/test",
      `/api/v1/notifications/deliveries/${delivery.id}/retry`,
    ]) {
      const response = await request(pathname, "POST");
      assert.equal(response.status, 400);
      assert.doesNotMatch(await response.text(), /route-fixture-secret/);
    }
    assert.deepEqual(listAuditLog(10), []);
  });

  it("requeues a failed delivery after configuration is fixed and prevents duplicate retries", async () => {
    configureNotifications(true, webhook);
    const delivery = queueTestNotification();
    await failDelivery();
    const settings = await request("/api/v1/notifications", "PUT", "admin", {
      enabled: true,
      webhookUrl: "https://discord.com/api/webhooks/789012/replacement-fixture-secret",
    });
    assert.equal(settings.status, 200);
    await settings.body?.cancel();
    const response = await request(`/api/v1/notifications/deliveries/${delivery.id}/retry`, "POST");
    assert.equal(response.status, 202);
    const retried = notificationDeliveryResponseSchema.parse(await response.json()).delivery;
    assert.equal(retried.id, delivery.id);
    assert.equal(retried.state, "queued");
    assert.equal(retried.attempts, 5);
    assert.equal(retried.retryable, false);
    assert.ok(retried.nextAttemptAt);
    const repeated = await request(`/api/v1/notifications/deliveries/${delivery.id}/retry`, "POST");
    assert.equal(repeated.status, 409);
    await repeated.body?.cancel();
    const entries = listAuditLog(10).filter((entry) => entry.action === "notifications.retry_queued");
    assert.equal(entries.length, 1);
    assert.equal(entries[0].targetId, null);
    assert.deepEqual(entries[0].details, { deliveryId: delivery.id });
    assert.doesNotMatch(JSON.stringify(entries), /fixture-secret|payload|webhook|event_key/);
  });
});
