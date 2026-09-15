import type { Page } from "@playwright/test";
import {
  notificationDeliveriesResponseSchema,
  notificationDeliveryResponseSchema,
  notificationSettingsRequestSchema,
  notificationSettingsResponseSchema,
  type NotificationDelivery,
  type NotificationSettings,
} from "@ludock/shared";
import { test, expect } from "./fixtures";

const CREATED_AT = Date.UTC(2026, 8, 14, 12);
const DELIVERED_ID = "11111111-1111-4111-8111-111111111111";
const FAILED_ID = "22222222-2222-4222-8222-222222222222";
const PENDING_ID = "33333333-3333-4333-8333-333333333333";
const TEST_ID = "44444444-4444-4444-8444-444444444444";
const FAILURE = "Discord rejected the webhook. Check the saved webhook URL and try again.";

function delivery(id: string, overrides: Partial<NotificationDelivery> = {}): NotificationDelivery {
  return {
    id,
    kind: "event",
    state: "queued",
    attempts: 0,
    createdAt: CREATED_AT,
    lastAttemptAt: null,
    deliveredAt: null,
    nextAttemptAt: CREATED_AT,
    lastFailure: null,
    retryable: false,
    ...overrides,
  };
}

const failedDelivery = () => delivery(FAILED_ID, {
  state: "failed",
  attempts: 5,
  lastAttemptAt: CREATED_AT + 10_000,
  nextAttemptAt: null,
  lastFailure: FAILURE,
  retryable: true,
});

async function mockNotifications(
  page: Page,
  initialDeliveries: NotificationDelivery[],
  settings: NotificationSettings = { configured: true, enabled: true },
) {
  const fixture = {
    deliveries: initialDeliveries.map((item) => ({ ...item, retryable: item.retryable && settings.configured && settings.enabled })),
    settings,
    reads: 0,
    mutations: [] as string[],
  };
  await page.route("**/api/v1/settings/deployment", (route) => route.fulfill({
    json: { backupRoots: ["/backups"], composeRoots: [], composeAvailable: false },
  }));
  await page.route("**/api/v1/settings/backups", (route) => route.fulfill({ json: { settings: null } }));
  await page.route("**/api/v1/notifications**", async (route) => {
    const path = new URL(route.request().url()).pathname.replace("/api/v1", "");
    const method = route.request().method();
    if (path === "/notifications" && method === "GET") {
      return route.fulfill({ json: notificationSettingsResponseSchema.parse(fixture.settings) });
    }
    if (path === "/notifications/deliveries" && method === "GET") {
      fixture.reads += 1;
      return route.fulfill({ json: notificationDeliveriesResponseSchema.parse({ deliveries: fixture.deliveries }) });
    }
    if (path === "/notifications" && method === "PUT") {
      const request = notificationSettingsRequestSchema.parse(route.request().postDataJSON());
      fixture.settings = { enabled: request.enabled, configured: fixture.settings.configured || Boolean(request.webhookUrl) };
      fixture.deliveries = fixture.deliveries.map((item) => ({
        ...item,
        retryable: fixture.settings.enabled && fixture.settings.configured
          && (item.state === "failed" || (item.state === "queued" && item.lastFailure !== null)),
      }));
      fixture.mutations.push(path);
      return route.fulfill({ json: notificationSettingsResponseSchema.parse(fixture.settings) });
    }
    if (path === "/notifications/test" && method === "POST") {
      const queued = delivery(TEST_ID, { kind: "test", createdAt: CREATED_AT + 30_000 });
      fixture.deliveries = [queued, ...fixture.deliveries];
      fixture.mutations.push(path);
      return route.fulfill({ status: 202, json: notificationDeliveryResponseSchema.parse({ delivery: queued }) });
    }
    if (path === `/notifications/deliveries/${FAILED_ID}/retry` && method === "POST") {
      const queued = { ...failedDelivery(), state: "queued" as const, retryable: false, nextAttemptAt: CREATED_AT + 30_000 };
      fixture.deliveries = fixture.deliveries.map((item) => item.id === FAILED_ID ? queued : item);
      fixture.mutations.push(path);
      return route.fulfill({ status: 202, json: notificationDeliveryResponseSchema.parse({ delivery: queued }) });
    }
    return route.fallback();
  });
  return fixture;
}

test("Discord setup explains the webhook and focuses missing configuration before saving", async ({ app, page }) => {
  const fixture = await mockNotifications(page, [], { configured: false, enabled: false });
  await app.open("/settings");
  const section = page.getByRole("region", { name: "Discord notifications", exact: true });
  await expect(section.getByRole("link", { name: "Discord webhook guide" })).toBeVisible();
  await section.getByRole("checkbox", { name: "Enable Discord delivery" }).check();
  await section.getByRole("button", { name: "Save notifications" }).click();
  const webhook = section.getByLabel("Webhook URL", { exact: true });
  await expect(webhook).toBeFocused();
  expect(fixture.mutations).toEqual([]);
  await webhook.fill("https://discord.com/api/webhooks/disposable/fixture-only");
  await section.getByRole("button", { name: "Save notifications" }).click();
  await expect(section.getByText("Notification settings saved.")).toBeVisible();
  await expect(section.getByLabel("Replace webhook URL", { exact: true })).toHaveValue("");
  await expect(section.getByRole("button", { name: "Send test notification" })).toBeEnabled();
  expect(fixture.mutations).toEqual(["/notifications"]);
});

test("recent Discord deliveries show outcomes, retry timing, and safe failure guidance on desktop and mobile", async ({ app, page }, testInfo) => {
  const fixture = await mockNotifications(page, [
    delivery(DELIVERED_ID, {
      kind: "test", state: "delivered", attempts: 1, lastAttemptAt: CREATED_AT + 1_000,
      deliveredAt: CREATED_AT + 1_000, nextAttemptAt: null,
    }),
    failedDelivery(),
    delivery(PENDING_ID, {
      attempts: 1, lastAttemptAt: CREATED_AT + 10_000, nextAttemptAt: CREATED_AT + 60_000,
      lastFailure: "Discord is temporarily unavailable. Delivery will be retried automatically.",
    }),
  ]);
  await app.open("/settings");
  const section = page.getByRole("region", { name: "Discord notifications", exact: true });
  const table = page.getByRole("table", { name: "Recent notification deliveries", exact: true });
  await expect(section.getByRole("heading", { name: "Recent deliveries", exact: true })).toBeVisible();
  await expect(table.getByText("Delivered", { exact: true })).toBeVisible();
  await expect(table.getByText("Pending retry", { exact: true })).toBeVisible();
  await expect(table.getByText("Failed", { exact: true })).toBeVisible();
  await expect(table).toContainText(FAILURE);
  await expect(table).toContainText("Discord is temporarily unavailable.");
  await expect(table).toContainText(new Date(CREATED_AT + 60_000).toLocaleString("en-US", { timeZone: "UTC" }));
  await expect(table).not.toContainText("https://discord.com/api/webhooks/");
  await expect(section.getByLabel("Replace webhook URL", { exact: true })).toHaveValue("");
  const retry = table.getByRole("button", { name: /^Retry/ });
  await expect(retry).toBeEnabled();
  expect(fixture.mutations).toEqual([]);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);

  if (testInfo.project.name === "mobile") {
    for (const control of [
      section.getByRole("button", { name: "Send test notification", exact: true }),
      section.getByRole("button", { name: "Refresh deliveries", exact: true }),
      retry,
    ]) {
      const bounds = await control.boundingBox();
      expect(bounds).not.toBeNull();
      expect(bounds!.height).toBeGreaterThanOrEqual(44);
    }
    await page.setViewportSize({ width: 320, height: 844 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  }
  await page.evaluate(() => {
    if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
    window.scrollTo(0, 0);
  });
  const screenshot = testInfo.outputPath("discord-notification-deliveries.png");
  await page.screenshot({ path: screenshot, fullPage: true });
  await testInfo.attach("Discord notification troubleshooting", { path: screenshot, contentType: "image/png" });
});

test("test notifications and explicit retries stay queued until a confirmed delivery", async ({ app, page }) => {
  const fixture = await mockNotifications(page, [failedDelivery()]);
  await app.open("/settings");
  const section = page.getByRole("region", { name: "Discord notifications", exact: true });
  const table = page.getByRole("table", { name: "Recent notification deliveries", exact: true });
  await section.getByRole("button", { name: "Send test notification", exact: true }).click();
  await expect(table.getByText("Queued", { exact: true })).toBeVisible();
  await expect(table.getByText("Delivered", { exact: true })).toHaveCount(0);
  await expect(section.getByRole("status")).toContainText(/queued/i);
  expect(fixture.mutations).toEqual(["/notifications/test"]);
  const retry = table.getByRole("button", { name: /^Retry/ });
  await retry.focus();
  await page.keyboard.press("Enter");
  await expect(table.getByText("Pending retry", { exact: true })).toBeVisible();
  await expect(table.getByText("Failed", { exact: true })).toHaveCount(0);
  await expect(table.getByText("Delivered", { exact: true })).toHaveCount(0);
  expect(fixture.mutations).toEqual(["/notifications/test", `/notifications/deliveries/${FAILED_ID}/retry`]);

  fixture.deliveries = fixture.deliveries.map((item) => ({
    ...item, state: "delivered", deliveredAt: CREATED_AT + 40_000,
    lastAttemptAt: CREATED_AT + 40_000, nextAttemptAt: null, lastFailure: null, attempts: item.attempts + 1,
  }));
  await section.getByRole("button", { name: "Refresh deliveries", exact: true }).click();
  await expect(table.getByText("Delivered", { exact: true })).toHaveCount(2);
  await expect(table.getByText("Queued", { exact: true })).toHaveCount(0);
  await expect(table.getByText("Pending retry", { exact: true })).toHaveCount(0);
});

test("refresh preserves unsaved notification settings and prevents sending with those drafts", async ({ app, page }) => {
  const fixture = await mockNotifications(page, [failedDelivery()]);
  await app.open("/settings");
  const section = page.getByRole("region", { name: "Discord notifications", exact: true });
  const webhook = section.getByLabel("Replace webhook URL", { exact: true });
  const enabled = section.getByRole("checkbox", { name: "Enable Discord delivery", exact: true });
  const send = section.getByRole("button", { name: "Send test notification", exact: true });
  const retry = section.getByRole("button", { name: /^Retry/ });
  const draft = "https://discord.com/api/webhooks/123/disposable-fixture-token";
  await webhook.fill(draft);
  await expect(send).toBeDisabled();
  await expect(retry).toBeDisabled();
  await enabled.uncheck();
  const reads = fixture.reads;
  await section.getByRole("button", { name: "Refresh deliveries", exact: true }).click();
  await expect.poll(() => fixture.reads).toBeGreaterThan(reads);
  await expect(webhook).toHaveValue(draft);
  await expect(enabled).not.toBeChecked();
  await expect(send).toBeDisabled();
  await expect(retry).toBeDisabled();
  expect(fixture.mutations).toEqual([]);

  await webhook.fill("");
  await expect(send).toBeDisabled();
  await enabled.check();
  await expect(send).toBeEnabled();
  await expect(retry).toBeEnabled();
});

for (const configured of [true, false]) {
  test(`Discord delivery must be saved and enabled before tests or retries (${configured ? "configured" : "unconfigured"})`, async ({ app, page }) => {
    const fixture = await mockNotifications(page, [failedDelivery()], { configured, enabled: false });
    await app.open("/settings");
    const section = page.getByRole("region", { name: "Discord notifications", exact: true });
    const send = section.getByRole("button", { name: "Send test notification", exact: true });
    const retry = section.getByRole("button", { name: /^Retry/ });
    await expect(send).toBeDisabled();
    await expect(retry).toHaveCount(0);
    await section.getByRole("checkbox", { name: "Enable Discord delivery", exact: true }).check();
    if (!configured) await section.getByLabel("Webhook URL", { exact: true }).fill("https://discord.com/api/webhooks/123/disposable-fixture-token");
    await expect(send).toBeDisabled();
    await expect(retry).toHaveCount(0);
    const readsBeforeSave = fixture.reads;
    await section.getByRole("button", { name: "Save notifications", exact: true }).click();
    await expect(section.getByText("Notification settings saved.", { exact: true })).toBeVisible();
    await expect.poll(() => fixture.reads).toBeGreaterThan(readsBeforeSave);
    await expect(send).toBeEnabled();
    await expect(retry).toBeEnabled();
    await expect(section.getByLabel("Replace webhook URL", { exact: true })).toHaveValue("");
    expect(fixture.mutations).toEqual(["/notifications"]);
    await retry.click();
    await expect(section.getByText("Pending retry", { exact: true })).toBeVisible();
    expect(fixture.mutations).toEqual(["/notifications", `/notifications/deliveries/${FAILED_ID}/retry`]);
  });
}

test("disabled Discord delivery pauses polling until saved settings enable delivery", async ({ app, page }) => {
  await page.clock.install();
  const fixture = await mockNotifications(page, [delivery(PENDING_ID, {
    attempts: 1,
    lastAttemptAt: CREATED_AT + 10_000,
    lastFailure: "Discord is temporarily unavailable. Delivery will be retried automatically.",
  })], { configured: true, enabled: false });
  await app.open("/settings");
  const section = page.getByRole("region", { name: "Discord notifications", exact: true });
  await expect(section.getByRole("table", { name: "Recent notification deliveries", exact: true })).toBeVisible();
  const pausedReads = fixture.reads;
  await page.clock.runFor(15_000);
  expect(fixture.reads).toBe(pausedReads);
  await section.getByRole("checkbox", { name: "Enable Discord delivery", exact: true }).check();
  await section.getByRole("button", { name: "Save notifications", exact: true }).click();
  await expect.poll(() => fixture.reads).toBeGreaterThan(pausedReads);
  const enabledReads = fixture.reads;
  await page.clock.runFor(5_100);
  await expect.poll(() => fixture.reads).toBeGreaterThan(enabledReads);
});
