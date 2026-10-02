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
});

describe("Discord notification troubleshooting", () => {
  it("keeps provider response details out of delivery history", async () => {
    configureNotifications(true, webhook);
    queueTestNotification();
    await deliverNotifications((async () => new Response(`untrusted-body ${webhook}`, {
      status: 503,
      statusText: `untrusted-status ${webhook}`,
    })));
    const delivery = listNotificationDeliveries()[0];
    expect(delivery.lastFailure!).toMatch(/temporarily unavailable/);
    expect(delivery.state).toBe("queued");
    expect(delivery.attempts).toBe(1);
    expect(delivery.deliveredAt).toBe(null);
    expect(delivery.nextAttemptAt! > delivery.lastAttemptAt!).toBeTruthy();
    expect(delivery.retryable).toBe(true);
    for (const value of [deliveries(), listNotificationDeliveries()]) {
      expect(JSON.stringify(value)).not.toMatch(/fake-secret-for-tests|untrusted-body|untrusted-status/);
    }
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
});
