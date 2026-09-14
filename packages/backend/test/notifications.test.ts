import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { notificationDeliveriesResponseSchema } from "@ludock/shared";
import { afterEach, beforeEach, describe, it } from "bun:test";
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

process.env.LUDOCK_DB_PATH = ":memory:";
const webhook = "https://discord.com/api/webhooks/123456/fake-secret-for-tests";
beforeEach(() => {
  closeDatabase();
  process.env.LUDOCK_DB_PATH = ":memory:";
});
afterEach(() => closeDatabase());
function deliveries() {
  return getDatabase().prepare("SELECT * FROM notification_deliveries").all();
}
function makeQueuedDeliveriesDue() {
  getDatabase().prepare("UPDATE notification_deliveries SET next_attempt_at=0 WHERE state='queued'").run();
}

describe("Discord notification delivery", () => {
  it("keeps secrets write-only and disables arbitrary webhook destinations", () => {
    configureNotifications(true, webhook);
    assert.deepEqual(notificationConfiguration(), {
      configured: true,
      enabled: true,
    });
    configureNotifications(false);
    assert.deepEqual(notificationConfiguration(), {
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
      assert.throws(() => configureNotifications(true, url), /Discord HTTPS/);
  });
  it("does not queue disabled notifications and deduplicates event retries", () => {
    notifyEvent("disabled", "No delivery");
    assert.equal(deliveries().length, 0);
    configureNotifications(true, webhook);
    notifyEvent("outage:1", "Server @everyone failed");
    notifyEvent("outage:1", "Server @everyone failed");
    assert.equal(deliveries().length, 1);
    assert.deepEqual(
      JSON.parse(deliveries()[0].payload_json as string).allowed_mentions,
      { parse: [] },
    );
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
      assert.equal(url, `${webhook}?wait=true`);
      assert.equal(init?.redirect, "error");
      assert.ok(init?.signal);
      throw new Error(`network error ${webhook}`);
    }) as typeof fetch;
    await deliverNotifications(fetcher);
    assert.equal(deliveries()[0].attempts, 1);
    assert.equal(deliveries()[0].state, "queued");
    await deliverNotifications(fetcher);
    assert.equal(calls, 1);
    for (let count = 0; count < 4; count++) {
      getDatabase()
        .prepare("UPDATE notification_deliveries SET next_attempt_at=0")
        .run();
      await deliverNotifications(fetcher);
    }
    assert.equal(deliveries()[0].attempts, 5);
    assert.equal(deliveries()[0].state, "failed");
    assert.equal(
      JSON.stringify(deliveries()).includes("fake-secret-for-tests"),
      false,
    );
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
    }) as typeof fetch;
    const first = deliverNotifications(fetcher);
    await deliverNotifications(fetcher);
    assert.equal(calls, 1);
    release();
    await first;
    assert.equal(deliveries()[0].state, "delivered");
    await deliverNotifications(fetcher);
    assert.equal(calls, 1);
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
    }) as typeof fetch);
    assert.equal(calls, 1);
    assert.deepEqual(
      deliveries().map((row) => [row.state, row.attempts]).sort(),
      [
        ["delivered", 1],
        ["queued", 0],
      ],
    );
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
    }) as typeof fetch);
    assert.deepEqual(destinations, [`${webhook}?wait=true`, `${replacement}?wait=true`]);
    assert.equal(deliveries().every((row) => row.state === "delivered"), true);
  });
});

describe("Discord notification troubleshooting", () => {
  it("queues distinct safe test messages through the saved webhook and records delivery times", async () => {
    configureNotifications(true, webhook);
    const before = Date.now();
    const first = queueTestNotification();
    const second = queueTestNotification();
    assert.notEqual(first.id, second.id);
    assert.equal(first.kind, "test");
    assert.equal(first.state, "queued");
    assert.equal(first.attempts, 0);
    assert.equal(first.lastAttemptAt, null);
    assert.equal(first.deliveredAt, null);
    assert.equal(first.lastFailure, null);
    assert.equal(first.retryable, false);
    assert.ok(first.createdAt >= before);
    assert.equal(first.nextAttemptAt, first.createdAt);
    const messages: unknown[] = [];
    await deliverNotifications((async (url, init) => {
      assert.equal(url, `${webhook}?wait=true`);
      assert.equal(init?.method, "POST");
      assert.equal(init?.redirect, "error");
      messages.push(JSON.parse(String(init?.body)));
      return new Response(null, { status: 204 });
    }) as typeof fetch);
    assert.equal(messages.length, 2);
    for (const message of messages) {
      assert.deepEqual((message as { allowed_mentions: unknown }).allowed_mentions, { parse: [] });
      assert.match((message as { content: string }).content, /Ludock test notification/);
    }
    const history = listNotificationDeliveries();
    notificationDeliveriesResponseSchema.parse({ deliveries: history });
    for (const delivery of history) {
      assert.equal(delivery.state, "delivered");
      assert.equal(delivery.attempts, 1);
      assert.ok(delivery.lastAttemptAt! >= delivery.createdAt);
      assert.ok(delivery.deliveredAt! >= delivery.lastAttemptAt!);
      assert.ok(delivery.deliveredAt! <= Date.now());
      assert.equal(delivery.nextAttemptAt, null);
      assert.equal(delivery.lastFailure, null);
      assert.equal(delivery.retryable, false);
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
    })) as typeof fetch);
    const delivery = listNotificationDeliveries()[0];
    assert.match(delivery.lastFailure!, reason);
    assert.equal(delivery.state, "queued");
    assert.equal(delivery.attempts, 1);
    assert.equal(delivery.deliveredAt, null);
    assert.ok(delivery.nextAttemptAt! > delivery.lastAttemptAt!);
    assert.equal(delivery.retryable, true);
    for (const value of [deliveries(), listNotificationDeliveries()]) {
      assert.doesNotMatch(JSON.stringify(value), /fake-secret-for-tests|untrusted-body|untrusted-status/);
    }
  });

  it("keeps thrown network details out of storage and delivery history", async () => {
    configureNotifications(true, webhook);
    queueTestNotification();
    await deliverNotifications((async () => {
      throw new Error(`untrusted-exception ${webhook}`);
    }) as typeof fetch);
    assert.match(listNotificationDeliveries()[0].lastFailure!, /Unable to reach Discord/);
    assert.doesNotMatch(JSON.stringify(deliveries()), /untrusted-exception|fake-secret-for-tests/);
    assert.doesNotMatch(JSON.stringify(listNotificationDeliveries()), /untrusted-exception|fake-secret-for-tests/);
  });

  it("gives each explicit retry five attempts while preserving lifetime history and using the replacement webhook", async () => {
    configureNotifications(true, webhook);
    const delivery = queueTestNotification();
    const failure = (async () => new Response(null, { status: 404 })) as typeof fetch;
    for (let attempt = 0; attempt < 5; attempt++) {
      makeQueuedDeliveriesDue();
      await deliverNotifications(failure);
    }
    const failed = listNotificationDeliveries()[0];
    assert.equal(failed.state, "failed");
    assert.equal(failed.attempts, 5);
    assert.equal(failed.nextAttemptAt, null);
    assert.equal(failed.retryable, true);
    const retried = retryNotificationDelivery(delivery.id);
    assert.equal(retried.state, "queued");
    assert.equal(retried.attempts, 5);
    assert.equal(retried.createdAt, delivery.createdAt);
    assert.equal(retried.lastAttemptAt, failed.lastAttemptAt);
    assert.equal(retried.lastFailure, failed.lastFailure);
    assert.equal(retried.retryable, false);
    for (let attempt = 0; attempt < 5; attempt++) {
      makeQueuedDeliveriesDue();
      await deliverNotifications(failure);
      assert.equal(listNotificationDeliveries()[0].state, attempt === 4 ? "failed" : "queued");
    }
    assert.equal(listNotificationDeliveries()[0].attempts, 10);
    const replacement = "https://discord.com/api/webhooks/789012/replaced-test-secret";
    configureNotifications(true, replacement);
    retryNotificationDelivery(delivery.id);
    await deliverNotifications((async (url) => {
      assert.equal(url, `${replacement}?wait=true`);
      return new Response(null, { status: 204 });
    }) as typeof fetch);
    const delivered = listNotificationDeliveries()[0];
    assert.equal(delivered.id, delivery.id);
    assert.equal(delivered.state, "delivered");
    assert.equal(delivered.attempts, 11);
    assert.equal(delivered.lastFailure, null);
    assert.equal(delivered.retryable, false);
    assert.equal(deliveries().length, 1);
  });

  it("rejects disabled, unknown, fresh, delivered, and duplicate retry requests", async () => {
    assert.throws(() => queueTestNotification(), { code: "NOTIFICATIONS_DISABLED" });
    assert.throws(() => retryNotificationDelivery(crypto.randomUUID()), { code: "NOTIFICATIONS_DISABLED" });
    configureNotifications(true, webhook);
    assert.throws(() => retryNotificationDelivery(crypto.randomUUID()), { code: "NOTIFICATION_NOT_FOUND" });
    const queued = queueTestNotification();
    assert.throws(() => retryNotificationDelivery(queued.id), { code: "NOTIFICATION_NOT_RETRYABLE" });
    await deliverNotifications((async () => new Response(null, { status: 503 })) as typeof fetch);
    configureNotifications(false);
    assert.equal(listNotificationDeliveries()[0].retryable, false);
    assert.throws(() => retryNotificationDelivery(queued.id), { code: "NOTIFICATIONS_DISABLED" });
    assert.throws(() => queueTestNotification(), { code: "NOTIFICATIONS_DISABLED" });
    configureNotifications(true);
    const retried = retryNotificationDelivery(queued.id);
    assert.equal(retried.attempts, 1);
    assert.equal(retried.retryable, false);
    assert.throws(() => retryNotificationDelivery(queued.id), { code: "NOTIFICATION_NOT_RETRYABLE" });
    await deliverNotifications((async () => new Response(null, { status: 204 })) as typeof fetch);
    assert.throws(() => retryNotificationDelivery(queued.id), { code: "NOTIFICATION_NOT_RETRYABLE" });
    assert.equal(deliveries()[0].attempts, 2);
  });

  it("rejects retries of a delivery already being sent", async () => {
    configureNotifications(true, webhook);
    const queued = queueTestNotification();
    await deliverNotifications((async () => new Response(null, { status: 503 })) as typeof fetch);
    makeQueuedDeliveriesDue();
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const pending = deliverNotifications((async () => {
      await gate;
      return new Response(null, { status: 204 });
    }) as typeof fetch);
    try {
      assert.equal(listNotificationDeliveries()[0].retryable, false);
      assert.throws(() => retryNotificationDelivery(queued.id), { code: "NOTIFICATION_NOT_RETRYABLE" });
    } finally {
      release();
      await pending;
    }
    assert.equal(listNotificationDeliveries()[0].attempts, 2);
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
        assert.equal(retryNotificationDelivery(second.id).attempts, 9);
        return new Response(null, { status: 204 });
      }
      return new Response(null, { status: 503 });
    }) as typeof fetch);
    assert.equal(calls, 2);
    const retried = listNotificationDeliveries().find(({ id }) => id === second.id)!;
    assert.equal(retried.attempts, 10);
    assert.equal(retried.state, "queued");
    assert.equal(retried.retryable, true);
  });

  it("limits history to the newest fifty deliveries and exposes only safe diagnostic fields", () => {
    configureNotifications(true, webhook);
    const ids: string[] = [];
    for (let index = 0; index < 55; index++) {
      notifyEvent(`private-event-${index}`, `private-payload-${index}`);
      const row = getDatabase().prepare("SELECT id FROM notification_deliveries WHERE event_key=?")
        .get(`private-event-${index}`)!;
      ids.push(row.id as string);
      getDatabase().prepare("UPDATE notification_deliveries SET created_at=? WHERE id=?").run(index, row.id);
    }
    getDatabase().prepare(`UPDATE notification_deliveries
      SET state='failed',attempts=5,retry_attempts=5,failure_code=? WHERE id=?`).run(webhook, ids[54]);
    const history = listNotificationDeliveries();
    assert.deepEqual(history.map(({ id }) => id), ids.slice(5).reverse());
    assert.match(history[0].lastFailure!, /details are unavailable/);
    assert.equal(history[0].retryable, true);
    notificationDeliveriesResponseSchema.parse({ deliveries: history });
    assert.deepEqual(Object.keys(history[0]).sort(), [
      "id", "kind", "state", "attempts", "createdAt", "lastAttemptAt", "deliveredAt",
      "nextAttemptAt", "lastFailure", "retryable",
    ].sort());
    assert.doesNotMatch(JSON.stringify(history), /private-event|private-payload|fake-secret-for-tests/);
  });

  it("preserves diagnostics and pending explicit retries across database reopen", async () => {
    const directory = fs.mkdtempSync(path.join(os.tmpdir(), "ludock-notifications-"));
    process.env.LUDOCK_DB_PATH = path.join(directory, "notifications.db");
    try {
      configureNotifications(true, webhook);
      const queued = queueTestNotification();
      await deliverNotifications((async () => new Response(null, { status: 404 })) as typeof fetch);
      const failureHistory = listNotificationDeliveries();
      closeDatabase();
      assert.deepEqual(listNotificationDeliveries(), failureHistory);
      assert.deepEqual(notificationConfiguration(), { configured: true, enabled: true });
      retryNotificationDelivery(queued.id);
      const retryHistory = listNotificationDeliveries();
      closeDatabase();
      assert.deepEqual(listNotificationDeliveries(), retryHistory);
      await deliverNotifications((async (url) => {
        assert.equal(url, `${webhook}?wait=true`);
        return new Response(null, { status: 204 });
      }) as typeof fetch);
      const deliveredHistory = listNotificationDeliveries();
      assert.equal(deliveredHistory[0].state, "delivered");
      assert.equal(deliveredHistory[0].attempts, 2);
      closeDatabase();
      assert.deepEqual(listNotificationDeliveries(), deliveredHistory);
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
    await deliverNotifications((async () => new Response(null, { status: 204 })) as typeof fetch);
    const history = listNotificationDeliveries();
    assert.equal(history.length, 1);
    assert.equal(history[0].id, oldRetry.id);
    assert.equal(history[0].state, "delivered");
    assert.equal(history[0].createdAt, old);
    assert.ok(history[0].deliveredAt! > old);
  });
});
