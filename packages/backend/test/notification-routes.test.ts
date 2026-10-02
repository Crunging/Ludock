import { expect, afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";
import { notificationDeliveriesResponseSchema } from "@ludock/shared";
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
import type { SQLQueryBindings } from "bun:sqlite";

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
    }));
  }
}

describe("notification troubleshooting routes", () => {
  it("requeues a failed delivery through HTTP after updating its webhook without duplicating it", async () => {
    configureNotifications(true, webhook);
    const delivery = queueTestNotification();
    await failDelivery();
    const replacement = "https://discord.com/api/webhooks/789012/replacement-fixture-secret";
    const settings = await request("/api/v1/notifications", "PUT", "admin", {
      enabled: true, webhookUrl: replacement,
    });
    expect(settings.status).toBe(200);
    await settings.body?.cancel();

    const path = `/api/v1/notifications/deliveries/${delivery.id}/retry`;
    const retried = await request(path, "POST");
    expect(retried.status).toBe(202);
    await retried.body?.cancel();
    const repeated = await request(path, "POST");
    expect(repeated.status).toBe(409);
    await repeated.body?.cancel();
    expect(getDatabase().prepare("SELECT id,state,attempts FROM notification_deliveries").all())
      .toStrictEqual([{ id: delivery.id, state: "queued", attempts: 5 }]);

    await deliverNotifications(async (url) => {
      expect(url).toBe(`${replacement}?wait=true`);
      return new Response(null, { status: 204 });
    });
    expect(getDatabase().prepare("SELECT id,state,attempts FROM notification_deliveries").all())
      .toStrictEqual([{ id: delivery.id, state: "delivered", attempts: 6 }]);
  });

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
    const stored = getDatabase().prepare<Record<string, unknown>, SQLQueryBindings[]>("SELECT state,attempts FROM notification_deliveries").all();
    expect(stored).toStrictEqual([{ state: "failed", attempts: 5 }]);
    expect(listAuditHistory({ limit: 10 }).entries).toStrictEqual([]);
  });

  it("returns delivery status and sanitized failure reasons without messages or event keys", async () => {
    configureNotifications(true, webhook);
    notifyEvent("private-event-key", "Private server event payload");
    await failDelivery();
    const delivered = queueTestNotification();
    await deliverNotifications((async () => new Response(null, { status: 204 })));
    const pending = queueTestNotification();
    await deliverNotifications((async () => new Response("Private Discord response", { status: 429 })));
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
});
