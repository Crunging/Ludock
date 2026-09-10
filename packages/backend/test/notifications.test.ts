import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it } from "bun:test";
import { closeDatabase, getDatabase } from "../src/database.js";
import {
  configureNotifications,
  deliverNotifications,
  notificationConfiguration,
  notifyEvent,
} from "../src/notifications.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const webhook = "https://discord.com/api/webhooks/123456/fake-secret-for-tests";
beforeEach(() => closeDatabase());
afterEach(() => closeDatabase());
function deliveries() {
  return getDatabase().prepare("SELECT * FROM notification_deliveries").all();
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
      assert.equal(url, webhook);
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
      deliveries().map((row) => [row.state, row.attempts]),
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
    assert.deepEqual(destinations, [webhook, replacement]);
    assert.equal(deliveries().every((row) => row.state === "delivered"), true);
  });
});
