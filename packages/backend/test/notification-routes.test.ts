import { expect, afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";
import {
  notificationDeliveriesResponseSchema,
  notificationDeliveryResponseSchema,
} from "@ludock/shared";
import { createApp } from "../src/app.js";
import { createSession } from "../src/auth.js";
import { closeDatabase, createUser, getDatabase } from "../src/database.js";
import { listAuditHistory } from "../src/history.js";
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
  expect(calls, "notification routes only enqueue work; tests never contact Discord").toBe(0);
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
        expect(response.status).toBe(role === null ? 401 : 403);
        expect(await response.text()).not.toMatch(new RegExp(`${delivery.id}|route-fixture-secret`));
      }
    }
    const stored = getDatabase().prepare("SELECT state,attempts FROM notification_deliveries").all();
    expect(stored).toStrictEqual([{ state: "failed", attempts: 5 }]);
    expect(listAuditHistory({ limit: 10 }).entries).toStrictEqual([]);
  });

  it("queues distinct test notifications and audits only their delivery IDs", async () => {
    configureNotifications(true, webhook);
    const ids = new Set<string>();
    for (let index = 0; index < 2; index++) {
      const response = await request("/api/v1/notifications/test", "POST");
      expect(response.status).toBe(202);
      const { delivery } = notificationDeliveryResponseSchema.parse(await response.json());
      expect(delivery.kind).toBe("test");
      expect(delivery.state).toBe("queued");
      expect(delivery.attempts).toBe(0);
      expect(delivery.lastAttemptAt).toBe(null);
      expect(delivery.deliveredAt).toBe(null);
      expect(delivery.lastFailure).toBe(null);
      expect(delivery.retryable).toBe(false);
      expect(delivery.nextAttemptAt).toBeTruthy();
      ids.add(delivery.id);
    }
    expect(ids.size).toBe(2);
    const audit = listAuditHistory({ limit: 10 }).entries;
    expect(audit.length).toBe(2);
    for (const entry of audit) {
      expect(entry.action).toBe("notifications.test_queued");
      expect(entry.username).toBe("admin");
      expect(entry.targetType).toBe("settings");
      expect(entry.targetId).toBe(null);
      expect(ids.has((entry.details as { deliveryId: string }).deliveryId)).toBeTruthy();
      expect(Object.keys(entry.details as object)).toStrictEqual(["deliveryId"]);
    }
    expect(JSON.stringify(audit)).not.toMatch(/route-fixture-secret|payload|webhook|event_key/);
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
    expect(response.status).toBe(200);
    expect(response.headers.get("Cache-Control")).toBe("no-store");
    const raw = await response.text();
    expect(raw).not.toMatch(/route-fixture-secret|private-event-key|Private|payload|webhook|event_key/i);
    const { deliveries } = notificationDeliveriesResponseSchema.parse(JSON.parse(raw));
    expect(deliveries.length).toBe(3);
    const failure = deliveries.find((delivery) => delivery.kind === "event")!;
    expect(failure.state).toBe("failed");
    expect(failure.attempts).toBe(5);
    expect(failure.lastFailure).toBeTruthy();
    expect(failure.lastAttemptAt).toBeTruthy();
    expect(failure.nextAttemptAt).toBe(null);
    expect(failure.retryable).toBe(true);
    expect(deliveries.find((delivery) => delivery.id === delivered.id)?.deliveredAt).toBeTruthy();
    const retry = deliveries.find((delivery) => delivery.id === pending.id)!;
    expect(retry.state).toBe("queued");
    expect(retry.attempts).toBe(1);
    expect(retry.lastFailure).toBeTruthy();
    expect(retry.nextAttemptAt).toBeTruthy();
  });

  it("validates retry IDs and rejects missing or nonretryable deliveries", async () => {
    configureNotifications(true, webhook);
    const malformed = await request("/api/v1/notifications/deliveries/not-a-uuid/retry", "POST");
    expect(malformed.status).toBe(400);
    expect((await malformed.json() as { code: string }).code).toBe("INVALID_REQUEST");
    const missing = await request(`/api/v1/notifications/deliveries/${crypto.randomUUID()}/retry`, "POST");
    expect(missing.status).toBe(404);
    await missing.body?.cancel();
    const delivery = queueTestNotification();
    const queued = await request(`/api/v1/notifications/deliveries/${delivery.id}/retry`, "POST");
    expect(queued.status).toBe(409);
    await queued.body?.cancel();
    await deliverNotifications((async () => new Response(null, { status: 204 })) as typeof fetch);
    const delivered = await request(`/api/v1/notifications/deliveries/${delivery.id}/retry`, "POST");
    expect(delivered.status).toBe(409);
    await delivered.body?.cancel();
    expect(listAuditHistory({ limit: 10 }).entries).toStrictEqual([]);
  });

  it("requires saved enabled settings for tests and retries", async () => {
    const unconfigured = await request("/api/v1/notifications/test", "POST");
    expect(unconfigured.status).toBe(400);
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
      expect(response.status).toBe(400);
      expect(await response.text()).not.toMatch(/route-fixture-secret/);
    }
    expect(listAuditHistory({ limit: 10 }).entries).toStrictEqual([]);
  });

  it("requeues a failed delivery after configuration is fixed and prevents duplicate retries", async () => {
    configureNotifications(true, webhook);
    const delivery = queueTestNotification();
    await failDelivery();
    const settings = await request("/api/v1/notifications", "PUT", "admin", {
      enabled: true,
      webhookUrl: "https://discord.com/api/webhooks/789012/replacement-fixture-secret",
    });
    expect(settings.status).toBe(200);
    await settings.body?.cancel();
    const response = await request(`/api/v1/notifications/deliveries/${delivery.id}/retry`, "POST");
    expect(response.status).toBe(202);
    const retried = notificationDeliveryResponseSchema.parse(await response.json()).delivery;
    expect(retried.id).toBe(delivery.id);
    expect(retried.state).toBe("queued");
    expect(retried.attempts).toBe(5);
    expect(retried.retryable).toBe(false);
    expect(retried.nextAttemptAt).toBeTruthy();
    const repeated = await request(`/api/v1/notifications/deliveries/${delivery.id}/retry`, "POST");
    expect(repeated.status).toBe(409);
    await repeated.body?.cancel();
    const entries = listAuditHistory({ limit: 10 }).entries.filter((entry) => entry.action === "notifications.retry_queued");
    expect(entries.length).toBe(1);
    expect(entries[0].targetId).toBe(null);
    expect(entries[0].details).toStrictEqual({ deliveryId: delivery.id });
    expect(JSON.stringify(entries)).not.toMatch(/fixture-secret|payload|webhook|event_key/);
  });
});
