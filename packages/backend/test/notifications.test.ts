import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { notificationDeliveriesResponseSchema } from "@ludock/shared";
import { expect, afterEach, beforeEach, describe, it } from "bun:test";
import { closeDatabase, getDatabase } from "../src/database.js";
import {
  configureNotifications,
  deliverNotifications,
  listNotificationDeliveries,
  notificationConfiguration,
  notifyEvent,
  queueTestNotification,
  retryNotificationDelivery,
} from "../src/notifications.js";
import type { SQLQueryBindings } from "bun:sqlite";

process.env.LUDOCK_DB_PATH = ":memory:";
const webhook = "https://discord.com/api/webhooks/123456/fake-secret-for-tests";
beforeEach(() => {
  closeDatabase();
  process.env.LUDOCK_DB_PATH = ":memory:";
});
afterEach(() => closeDatabase());
function deliveries() {
  return getDatabase().prepare<Record<string, unknown>, SQLQueryBindings[]>("SELECT * FROM notification_deliveries").all() as Record<string, unknown>[];
}
function makeQueuedDeliveriesDue() {
  getDatabase().prepare("UPDATE notification_deliveries SET next_attempt_at=0 WHERE state='queued'").run();
}

describe("Discord notification delivery", () => {
  it("keeps secrets write-only and disables arbitrary webhook destinations", () => {
    configureNotifications(true, webhook);
    expect(notificationConfiguration()).toStrictEqual({
      configured: true,
      enabled: true,
    });
    configureNotifications(false);
    expect(notificationConfiguration()).toStrictEqual({
      configured: true,
      enabled: false,
    });
    for (const url of [
      "http://discord.com/api/webhooks/123456/secret",
      "https://discord.com.evil.test/api/webhooks/123456/secret",
      "https://discord.com/api/webhooks/123456/secret?wait=true",
      "https://user:secret@discord.com/api/webhooks/123456/secret",
      "https://127.0.0.1/api/webhooks/123456/secret",
    ])
      expect(() => configureNotifications(true, url)).toThrow(/Discord HTTPS/);
  });
  it("does not queue disabled notifications and deduplicates event retries", () => {
    notifyEvent("disabled", "No delivery");
    expect(deliveries().length).toBe(0);
    configureNotifications(true, webhook);
    notifyEvent("outage:1", "Server @everyone failed");
    notifyEvent("outage:1", "Server @everyone failed");
    expect(deliveries().length).toBe(1);
    expect(JSON.parse(deliveries()[0].payload_json as string).allowed_mentions).toStrictEqual({ parse: [] });
  });
  it("uses bounded retries and never persists a fetch exception or webhook credential", async () => {
    configureNotifications(true, webhook);
    notifyEvent("failure", "Backup failed.");
    let calls = 0;
    const fetcher = (async (
      url: string | URL | Request,
      init?: RequestInit,
    ) => {
      calls++;
      expect(url).toBe(`${webhook}?wait=true`);
      expect(init?.redirect).toBe("error");
      expect(init?.signal).toBeTruthy();
      throw new Error(`network error ${webhook}`);
    });
    await deliverNotifications(fetcher);
    expect(deliveries()[0].attempts).toBe(1);
    expect(deliveries()[0].state).toBe("queued");
    await deliverNotifications(fetcher);
    expect(calls).toBe(1);
    for (let count = 0; count < 4; count++) {
      getDatabase()
        .prepare("UPDATE notification_deliveries SET next_attempt_at=0")
        .run();
      await deliverNotifications(fetcher);
    }
    expect(deliveries()[0].attempts).toBe(5);
    expect(deliveries()[0].state).toBe("failed");
    expect(JSON.stringify(deliveries()).includes("fake-secret-for-tests")).toBe(false);
  });
  it("marks successful delivery once and prevents overlapping delivery loops", async () => {
    configureNotifications(true, webhook);
    notifyEvent("success", "Recovered.");
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    let calls = 0;
    const fetcher = (async () => {
      calls++;
      await gate;
      return new Response(null, { status: 204 });
    });
    const first = deliverNotifications(fetcher);
    await deliverNotifications(fetcher);
    expect(calls).toBe(1);
    release();
    await first;
    expect(deliveries()[0].state).toBe("delivered");
    await deliverNotifications(fetcher);
    expect(calls).toBe(1);
  });
  it("stops the current batch when notifications are disabled during delivery", async () => {
    configureNotifications(true, webhook);
    notifyEvent("first", "First event.");
    notifyEvent("second", "Second event.");
    let calls = 0;
    await deliverNotifications((async () => {
      calls++;
      configureNotifications(false);
      return new Response(null, { status: 204 });
    }));
    expect(calls).toBe(1);
    expect(deliveries().map((row) => [row.state, row.attempts]).sort()).toStrictEqual([
        ["delivered", 1],
        ["queued", 0],
      ]);
  });
  it("uses a replaced webhook for the remaining queued deliveries", async () => {
    const replacement =
      "https://discord.com/api/webhooks/789012/replaced-test-secret";
    configureNotifications(true, webhook);
    notifyEvent("first", "First event.");
    notifyEvent("second", "Second event.");
    const destinations: unknown[] = [];
    await deliverNotifications((async (url) => {
      destinations.push(url);
      configureNotifications(true, replacement);
      return new Response(null, { status: 204 });
    }));
    expect(destinations).toStrictEqual([`${webhook}?wait=true`, `${replacement}?wait=true`]);
    expect(deliveries().every((row) => row.state === "delivered")).toBe(true);
  });
});

describe("Discord notification troubleshooting", () => {
  it("queues distinct safe test messages through the saved webhook and records delivery times", async () => {
    configureNotifications(true, webhook);
    const before = Date.now();
    const first = queueTestNotification();
    const second = queueTestNotification();
    expect(first.id).not.toBe(second.id);
    expect(first.kind).toBe("test");
    expect(first.state).toBe("queued");
    expect(first.attempts).toBe(0);
    expect(first.lastAttemptAt).toBe(null);
    expect(first.deliveredAt).toBe(null);
    expect(first.lastFailure).toBe(null);
    expect(first.retryable).toBe(false);
    expect(first.createdAt >= before).toBeTruthy();
    expect(first.nextAttemptAt).toBe(first.createdAt);
    const messages: unknown[] = [];
    await deliverNotifications((async (url, init) => {
      expect(url).toBe(`${webhook}?wait=true`);
      expect(init?.method).toBe("POST");
      expect(init?.redirect).toBe("error");
      messages.push(JSON.parse(String(init?.body)));
      return new Response(null, { status: 204 });
    }));
    expect(messages.length).toBe(2);
    for (const message of messages) {
      expect((message as { allowed_mentions: unknown }).allowed_mentions).toStrictEqual({ parse: [] });
      expect((message as { content: string }).content).toMatch(/Ludock test notification/);
    }
    const history = listNotificationDeliveries();
    notificationDeliveriesResponseSchema.parse({ deliveries: history });
    for (const delivery of history) {
      expect(delivery.state).toBe("delivered");
      expect(delivery.attempts).toBe(1);
      expect(delivery.lastAttemptAt! >= delivery.createdAt).toBeTruthy();
      expect(delivery.deliveredAt! >= delivery.lastAttemptAt!).toBeTruthy();
      expect(delivery.deliveredAt! <= Date.now()).toBeTruthy();
      expect(delivery.nextAttemptAt).toBe(null);
      expect(delivery.lastFailure).toBe(null);
      expect(delivery.retryable).toBe(false);
    }
  });

  it.each([
    [400, /rejected the notification/],
    [401, /refused webhook access/],
    [403, /refused webhook access/],
    [404, /could not find the webhook/],
    [429, /rate limited/],
    [503, /temporarily unavailable/],
    [418, /rejected delivery/],
  ] as const)("reports a safe reason for HTTP %s without exposing Discord response content", async (status, reason) => {
    configureNotifications(true, webhook);
    queueTestNotification();
    await deliverNotifications((async () => new Response(`untrusted-body ${webhook}`, {
      status,
      statusText: `untrusted-status ${webhook}`,
    })));
    const delivery = listNotificationDeliveries()[0];
    expect(delivery.lastFailure!).toMatch(reason);
    expect(delivery.state).toBe("queued");
    expect(delivery.attempts).toBe(1);
    expect(delivery.deliveredAt).toBe(null);
    expect(delivery.nextAttemptAt! > delivery.lastAttemptAt!).toBeTruthy();
    expect(delivery.retryable).toBe(true);
    for (const value of [deliveries(), listNotificationDeliveries()]) {
      expect(JSON.stringify(value)).not.toMatch(/fake-secret-for-tests|untrusted-body|untrusted-status/);
    }
  });

  it("keeps thrown network details out of storage and delivery history", async () => {
    configureNotifications(true, webhook);
    queueTestNotification();
    await deliverNotifications((async () => {
      throw new Error(`untrusted-exception ${webhook}`);
    }));
    expect(listNotificationDeliveries()[0].lastFailure!).toMatch(/Unable to reach Discord/);
    expect(JSON.stringify(deliveries())).not.toMatch(/untrusted-exception|fake-secret-for-tests/);
    expect(JSON.stringify(listNotificationDeliveries())).not.toMatch(/untrusted-exception|fake-secret-for-tests/);
  });

  it("gives each explicit retry five attempts while preserving lifetime history and using the replacement webhook", async () => {
    configureNotifications(true, webhook);
    const delivery = queueTestNotification();
    const failure = (async () => new Response(null, { status: 404 }));
    for (let attempt = 0; attempt < 5; attempt++) {
      makeQueuedDeliveriesDue();
      await deliverNotifications(failure);
    }
    const failed = listNotificationDeliveries()[0];
    expect(failed.state).toBe("failed");
    expect(failed.attempts).toBe(5);
    expect(failed.nextAttemptAt).toBe(null);
    expect(failed.retryable).toBe(true);
    const retried = retryNotificationDelivery(delivery.id);
    expect(retried.state).toBe("queued");
    expect(retried.attempts).toBe(5);
    expect(retried.createdAt).toBe(delivery.createdAt);
    expect(retried.lastAttemptAt).toBe(failed.lastAttemptAt);
    expect(retried.lastFailure).toBe(failed.lastFailure);
    expect(retried.retryable).toBe(false);
    for (let attempt = 0; attempt < 5; attempt++) {
      makeQueuedDeliveriesDue();
      await deliverNotifications(failure);
      expect(listNotificationDeliveries()[0].state).toBe(attempt === 4 ? "failed" : "queued");
    }
    expect(listNotificationDeliveries()[0].attempts).toBe(10);
    const replacement = "https://discord.com/api/webhooks/789012/replaced-test-secret";
    configureNotifications(true, replacement);
    retryNotificationDelivery(delivery.id);
    await deliverNotifications((async (url) => {
      expect(url).toBe(`${replacement}?wait=true`);
      return new Response(null, { status: 204 });
    }));
    const delivered = listNotificationDeliveries()[0];
    expect(delivered.id).toBe(delivery.id);
    expect(delivered.state).toBe("delivered");
    expect(delivered.attempts).toBe(11);
    expect(delivered.lastFailure).toBe(null);
    expect(delivered.retryable).toBe(false);
    expect(deliveries().length).toBe(1);
  });

  it("rejects disabled, unknown, fresh, delivered, and duplicate retry requests", async () => {
    expect(() => queueTestNotification()).toThrow(expect.objectContaining({ code: "NOTIFICATIONS_DISABLED" }));
    expect(() => retryNotificationDelivery(crypto.randomUUID())).toThrow(expect.objectContaining({ code: "NOTIFICATIONS_DISABLED" }));
    configureNotifications(true, webhook);
    expect(() => retryNotificationDelivery(crypto.randomUUID())).toThrow(expect.objectContaining({ code: "NOTIFICATION_NOT_FOUND" }));
    const queued = queueTestNotification();
    expect(() => retryNotificationDelivery(queued.id)).toThrow(expect.objectContaining({ code: "NOTIFICATION_NOT_RETRYABLE" }));
    await deliverNotifications((async () => new Response(null, { status: 503 })));
    configureNotifications(false);
    expect(listNotificationDeliveries()[0].retryable).toBe(false);
    expect(() => retryNotificationDelivery(queued.id)).toThrow(expect.objectContaining({ code: "NOTIFICATIONS_DISABLED" }));
    expect(() => queueTestNotification()).toThrow(expect.objectContaining({ code: "NOTIFICATIONS_DISABLED" }));
    configureNotifications(true);
    const retried = retryNotificationDelivery(queued.id);
    expect(retried.attempts).toBe(1);
    expect(retried.retryable).toBe(false);
    expect(() => retryNotificationDelivery(queued.id)).toThrow(expect.objectContaining({ code: "NOTIFICATION_NOT_RETRYABLE" }));
    await deliverNotifications((async () => new Response(null, { status: 204 })));
    expect(() => retryNotificationDelivery(queued.id)).toThrow(expect.objectContaining({ code: "NOTIFICATION_NOT_RETRYABLE" }));
    expect(deliveries()[0].attempts).toBe(2);
  });

  it("rejects retries of a delivery already being sent", async () => {
    configureNotifications(true, webhook);
    const queued = queueTestNotification();
    await deliverNotifications((async () => new Response(null, { status: 503 })));
    makeQueuedDeliveriesDue();
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const pending = deliverNotifications((async () => {
      await gate;
      return new Response(null, { status: 204 });
    }));
    try {
      expect(listNotificationDeliveries()[0].retryable).toBe(false);
      expect(() => retryNotificationDelivery(queued.id)).toThrow(expect.objectContaining({ code: "NOTIFICATION_NOT_RETRYABLE" }));
    } finally {
      release();
      await pending;
    }
    expect(listNotificationDeliveries()[0].attempts).toBe(2);
  });

  it("uses fresh retry counters when another queued delivery is retried during an earlier send", async () => {
    configureNotifications(true, webhook);
    const first = queueTestNotification();
    const second = queueTestNotification();
    getDatabase().prepare("UPDATE notification_deliveries SET created_at=1 WHERE id=?").run(first.id);
    getDatabase().prepare(`UPDATE notification_deliveries
      SET created_at=2,attempts=9,retry_attempts=4,failure_code='unavailable' WHERE id=?`).run(second.id);
    let calls = 0;
    await deliverNotifications((async () => {
      calls++;
      if (calls === 1) {
        expect(retryNotificationDelivery(second.id).attempts).toBe(9);
        return new Response(null, { status: 204 });
      }
      return new Response(null, { status: 503 });
    }));
    expect(calls).toBe(2);
    const retried = listNotificationDeliveries().find(({ id }) => id === second.id)!;
    expect(retried.attempts).toBe(10);
    expect(retried.state).toBe("queued");
    expect(retried.retryable).toBe(true);
  });

  it("limits history to the newest fifty deliveries and exposes only safe diagnostic fields", () => {
    configureNotifications(true, webhook);
    const ids: string[] = [];
    for (let index = 0; index < 55; index++) {
      notifyEvent(`private-event-${index}`, `private-payload-${index}`);
      const row = getDatabase().prepare<Record<string, unknown>, SQLQueryBindings[]>("SELECT id FROM notification_deliveries WHERE event_key=?")
        .get(`private-event-${index}`) as { id: string };
      ids.push(row.id as string);
      getDatabase().prepare("UPDATE notification_deliveries SET created_at=? WHERE id=?").run(index, row.id);
    }
    getDatabase().prepare(`UPDATE notification_deliveries
      SET state='failed',attempts=5,retry_attempts=5,failure_code=? WHERE id=?`).run(webhook, ids[54]);
    const history = listNotificationDeliveries();
    expect(history.map(({ id }) => id)).toStrictEqual(ids.slice(5).reverse());
    expect(history[0].lastFailure!).toMatch(/details are unavailable/);
    expect(history[0].retryable).toBe(true);
    notificationDeliveriesResponseSchema.parse({ deliveries: history });
    expect(Object.keys(history[0]).sort()).toStrictEqual([
      "id", "kind", "state", "attempts", "createdAt", "lastAttemptAt", "deliveredAt",
      "nextAttemptAt", "lastFailure", "retryable",
    ].sort());
    expect(JSON.stringify(history)).not.toMatch(/private-event|private-payload|fake-secret-for-tests/);
  });

  it("preserves diagnostics and pending explicit retries across database reopen", async () => {
    const directory = fs.mkdtempSync(path.join(os.tmpdir(), "ludock-notifications-"));
    process.env.LUDOCK_DB_PATH = path.join(directory, "notifications.db");
    try {
      configureNotifications(true, webhook);
      const queued = queueTestNotification();
      await deliverNotifications((async () => new Response(null, { status: 404 })));
      const failureHistory = listNotificationDeliveries();
      closeDatabase();
      expect(listNotificationDeliveries()).toStrictEqual(failureHistory);
      expect(notificationConfiguration()).toStrictEqual({ configured: true, enabled: true });
      retryNotificationDelivery(queued.id);
      const retryHistory = listNotificationDeliveries();
      closeDatabase();
      expect(listNotificationDeliveries()).toStrictEqual(retryHistory);
      await deliverNotifications((async (url) => {
        expect(url).toBe(`${webhook}?wait=true`);
        return new Response(null, { status: 204 });
      }));
      const deliveredHistory = listNotificationDeliveries();
      expect(deliveredHistory[0].state).toBe("delivered");
      expect(deliveredHistory[0].attempts).toBe(2);
      closeDatabase();
      expect(listNotificationDeliveries()).toStrictEqual(deliveredHistory);
    } finally {
      closeDatabase();
      process.env.LUDOCK_DB_PATH = ":memory:";
      fs.rmSync(directory, { recursive: true, force: true });
    }
  });

  it("retains newly delivered old retries and expires legacy delivered history", async () => {
    configureNotifications(true, webhook);
    const oldRetry = queueTestNotification();
    const legacyDelivered = queueTestNotification();
    const old = Date.now() - 31 * 86400_000;
    getDatabase().prepare(`UPDATE notification_deliveries
      SET created_at=?,state='failed',attempts=5,retry_attempts=5 WHERE id=?`).run(old, oldRetry.id);
    getDatabase().prepare(`UPDATE notification_deliveries
      SET created_at=?,state='delivered',attempts=1,retry_attempts=1 WHERE id=?`).run(old, legacyDelivered.id);
    retryNotificationDelivery(oldRetry.id);
    await deliverNotifications((async () => new Response(null, { status: 204 })));
    const history = listNotificationDeliveries();
    expect(history.length).toBe(1);
    expect(history[0].id).toBe(oldRetry.id);
    expect(history[0].state).toBe("delivered");
    expect(history[0].createdAt).toBe(old);
    expect(history[0].deliveredAt! > old).toBeTruthy();
  });
});
