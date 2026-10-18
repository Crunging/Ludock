import type { Page, Route } from "@playwright/test";
import {
  backupSettingsResponseSchema,
  backupSettingsSchema,
  backupStorageResponseSchema,
  connectionSettingsSchema,
  deploymentSettingsResponseSchema,
  notificationDeliveriesResponseSchema,
  notificationDeliveryResponseSchema,
  notificationDeliverySchema,
  notificationSettingsResponseSchema,
  okResponseSchema,
  serverGrantsResponseSchema,
  serverGrantsSchema,
  userResponseSchema,
  usersResponseSchema,
  type BackupSettings,
  type NotificationDelivery,
} from "@ludock/shared";
import { test, expect, ADMIN, RUNNING_ID, RUNNING_NAME } from "./fixtures";

const GIB = 1024 ** 3;
const CREATED_AT = Date.UTC(2026, 8, 14, 12);

function held() {
  let release = () => {};
  const released = new Promise<void>((resolve) => { release = resolve; });
  return { released, release };
}

/** Serves the Settings page with saved backup storage and Discord delivery. */
async function mockSettings(page: Page, saved: BackupSettings, deliveries: NotificationDelivery[] = []) {
  let current = saved;
  let statusReads = 0;
  await page.route("**/api/v1/settings/**", async (route) => {
    const path = new URL(route.request().url()).pathname.replace("/api/v1", "");
    if (path === "/settings/deployment") {
      await route.fulfill({ json: deploymentSettingsResponseSchema.parse({ backupRoots: ["/backups"], composeRoots: [], composeAvailable: false }) });
    } else if (path === "/settings/connection") {
      await route.fulfill({ json: connectionSettingsSchema.parse({ host: null }) });
    } else if (path === "/settings/backups/status") {
      statusReads++;
      await route.fulfill({ json: backupStorageResponseSchema.parse({ storage: {
        configured: true, archiveBytes: 2.5 * GIB, maxBytes: current.maxBytes, reserveBytes: current.reserveBytes,
        availableBytes: 24 * GIB, issues: [],
      } }) });
    } else if (path === "/settings/backups" && route.request().method() === "GET") {
      await route.fulfill({ json: backupSettingsResponseSchema.parse({ settings: current }) });
    } else await route.fallback();
  });
  await page.route("**/api/v1/notifications**", async (route) => {
    const path = new URL(route.request().url()).pathname.replace("/api/v1", "");
    if (path === "/notifications") {
      await route.fulfill({ json: notificationSettingsResponseSchema.parse({ configured: true, enabled: true }) });
    } else if (path === "/notifications/deliveries") {
      await route.fulfill({ json: notificationDeliveriesResponseSchema.parse({ deliveries }) });
    } else await route.fallback();
  });
  return {
    statusReads: () => statusReads,
    save: (settings: BackupSettings) => { current = settings; },
  };
}

test("backup storage keeps exact saved limits and newer drafts across refreshes and saves", async ({ app, page }) => {
  // An exact byte limit that whole GiB values in the form cannot express.
  const saved = backupSettingsSchema.parse({ destination: "/backups", retentionCount: 4, maxBytes: 100 * GIB + 17, reserveBytes: 5 * GIB });
  const fixture = await mockSettings(page, saved);
  const writes: unknown[] = [];
  let hold: ReturnType<typeof held> | null = null;
  await page.route("**/api/v1/settings/backups", async (route) => {
    if (route.request().method() !== "PUT") return route.fallback();
    const settings = backupSettingsSchema.parse(route.request().postDataJSON());
    writes.push(settings);
    await hold?.released;
    fixture.save(settings);
    await route.fulfill({ json: backupSettingsResponseSchema.parse({ settings }) });
  });
  await app.open("/settings");
  const storage = page.getByRole("region", { name: "Current storage" });
  const configuredLimit = storage.getByRole("definition").nth(1);
  const destination = page.getByLabel("Mounted destination path", { exact: true });
  const limit = page.getByLabel("Total backup limit (GiB)", { exact: true });
  const save = page.getByRole("button", { name: "Save backup settings", exact: true });
  await expect(page.getByText(/Backups are on\. You can change the destination and limits below\./)).toBeVisible();
  await expect(configuredLimit).toHaveText("100 GiB");

  await destination.fill("/new-backups");
  await save.click();
  await expect(page.getByText("Backup settings saved.")).toBeVisible();
  expect(writes).toEqual([{ ...saved, destination: "/new-backups" }]);
  await expect.poll(fixture.statusReads).toBe(2);

  await limit.fill("75");
  await storage.getByRole("button", { name: "Refresh storage", exact: true }).click();
  await expect.poll(fixture.statusReads).toBe(3);
  await expect(limit).toHaveValue("75");
  await expect(configuredLimit).toHaveText("100 GiB");

  hold = held();
  await save.click();
  await expect.poll(() => writes.length).toBe(2);
  await limit.fill("50");
  hold.release();
  await expect(configuredLimit).toHaveText("75 GiB");
  await expect(limit).toHaveValue("50");
  expect(writes[1]).toEqual({ ...saved, destination: "/new-backups", maxBytes: 75 * GIB });
});

test("test notifications and retries are queued once and stay current", async ({ app, page }) => {
  const failed = notificationDeliverySchema.parse({
    id: "11111111-1111-4111-8111-111111111111", kind: "event", state: "failed", attempts: 5,
    createdAt: CREATED_AT, lastAttemptAt: CREATED_AT + 10_000, deliveredAt: null, nextAttemptAt: null,
    lastFailure: "The saved Discord webhook no longer exists. Replace it and retry.", retryable: true,
  });
  await mockSettings(page, backupSettingsSchema.parse({ destination: "/backups", retentionCount: 10, maxBytes: 100 * GIB, reserveBytes: 5 * GIB }), [failed]);
  const posts: string[] = [];
  const testHeld = held();
  await page.route("**/api/v1/notifications/test", async (route) => {
    posts.push("test");
    await testHeld.released;
    await route.fulfill({ status: 202, json: notificationDeliveryResponseSchema.parse({ delivery: {
      ...failed, id: "22222222-2222-4222-8222-222222222222", kind: "test", state: "queued", attempts: 0,
      createdAt: CREATED_AT + 20_000, lastAttemptAt: null, lastFailure: null, retryable: false, nextAttemptAt: CREATED_AT + 25_000,
    } }) });
  });
  await page.route(`**/api/v1/notifications/deliveries/${failed.id}/retry`, async (route) => {
    posts.push("retry");
    await route.fulfill({ status: 202, json: notificationDeliveryResponseSchema.parse({ delivery: {
      ...failed, state: "queued", retryable: false, nextAttemptAt: CREATED_AT + 30_000,
    } }) });
  });
  await app.open("/settings");
  const table = page.getByRole("table", { name: "Recent notification deliveries" });
  const failedRow = table.getByRole("row").filter({ hasText: "Server notification" });
  await expect(failedRow).toContainText("Failed");
  await expect(failedRow).toContainText("The saved Discord webhook no longer exists.");

  await page.getByRole("button", { name: "Send test notification", exact: true }).click();
  const queuing = page.getByRole("button", { name: "Queuing test…", exact: true });
  await expect(queuing).toBeDisabled();
  await queuing.click({ force: true });
  await expect(failedRow.getByRole("button", { name: /^Retry notification/ })).toBeDisabled();
  testHeld.release();
  await expect(page.getByText("Test notification queued. Delivery status will update below.")).toBeVisible();
  await expect(table.getByRole("row").filter({ hasText: "Test notification" })).toContainText("Queued");
  expect(posts).toEqual(["test"]);

  // A history read that started before the retry must not replace its result.
  let staleRead: Route | undefined;
  await page.route("**/api/v1/notifications/deliveries", (route) => { staleRead = route; });
  await page.getByRole("button", { name: "Refresh deliveries", exact: true }).click();
  await expect.poll(() => Boolean(staleRead)).toBe(true);
  await failedRow.getByRole("button", { name: /^Retry notification/ }).click();
  await expect(failedRow).toContainText("Pending retry");
  await expect.poll(() => staleRead?.request().failure()).not.toBeNull();
  await staleRead!.abort();
  await expect(failedRow).toContainText("Pending retry");
  await expect(failedRow.getByRole("button", { name: /^Retry notification/ })).toHaveCount(0);
  expect(posts).toEqual(["test", "retry"]);
});

test("user access changes are applied one at a time and server access follows the role", async ({ app, page }) => {
  let friend = { id: "44444444-4444-4444-8444-444444444444", username: "friend", role: "operator", disabled: false, createdAt: 0 };
  const writes: { method: string; path: string; body: unknown }[] = [];
  let hold: ReturnType<typeof held> | null = null;
  await page.route("**/api/v1/users**", async (route) => {
    const request = route.request();
    const method = request.method();
    const path = new URL(request.url()).pathname.replace("/api/v1", "");
    if (path === "/users" && method === "GET") {
      await route.fulfill({ json: usersResponseSchema.parse({ users: [{ ...ADMIN, disabled: false, createdAt: 0 }, friend] }) });
      return;
    }
    if (path === `/users/${friend.id}/server-grants` && method === "GET") {
      await route.fulfill({ json: serverGrantsResponseSchema.parse({ grants: [] }) });
      return;
    }
    const body = request.postDataJSON();
    writes.push({ method, path, body });
    await hold?.released;
    if (path === `/users/${friend.id}` && method === "PATCH") {
      friend = { ...friend, ...body };
      await route.fulfill({ json: userResponseSchema.parse({ user: friend }) });
    } else if (path === `/users/${friend.id}/server-grants` && method === "PUT") {
      const { grants } = serverGrantsSchema.parse(body);
      await route.fulfill({ json: serverGrantsResponseSchema.parse({ grants: grants.map((grant) => ({ ...grant, updatedAt: 0 })) }) });
    } else if (path === `/users/${friend.id}/reset-password` && method === "POST") {
      await route.fulfill({ json: okResponseSchema.parse({ ok: true }) });
    } else await route.fallback();
  });
  await app.open("/users");
  const row = page.locator(".user-row").filter({ hasText: "friend" });
  const disable = row.getByRole("button", { name: "Disable", exact: true });

  // Disabling waits for the role change and keeps the role it returned.
  hold = held();
  await row.getByRole("combobox", { name: "Role for friend" }).selectOption("viewer");
  await expect(disable).toBeDisabled();
  await disable.click({ force: true });
  expect(writes).toHaveLength(1);
  hold.release();
  hold = null;
  await expect(page.getByText("Updated access for friend.")).toBeVisible();

  await row.getByRole("button", { name: "Server access", exact: true }).click();
  const server = row.getByRole("group", { name: RUNNING_NAME });
  await expect(server.getByRole("button", { name: "Start and stop" })).toHaveCount(0);
  await expect(server.getByRole("checkbox", { name: "Start", exact: true })).toHaveCount(0);
  await server.getByRole("button", { name: "Status only", exact: true }).click();
  await server.getByRole("checkbox", { name: "Read logs", exact: true }).check();
  await row.getByRole("button", { name: "Save server access", exact: true }).click();
  await expect(row.getByText("Server access saved for friend.")).toBeVisible();

  hold = held();
  const password = row.getByLabel("New password for friend");
  const reset = row.getByRole("button", { name: "Reset password", exact: true });
  await password.fill("first-password-123");
  await reset.click();
  await expect(reset).toBeDisabled();
  await reset.click({ force: true });
  await password.fill("second-password-123");
  hold.release();
  hold = null;
  await expect(page.getByText("Password reset for friend. Share the new password securely.")).toBeVisible();
  await expect(password).toHaveValue("second-password-123");

  await disable.click();
  await expect(row).toContainText("Disabled");
  expect(writes).toEqual([
    { method: "PATCH", path: `/users/${friend.id}`, body: { role: "viewer", disabled: false } },
    { method: "PUT", path: `/users/${friend.id}/server-grants`, body: { grants: [{ serverId: RUNNING_ID, capabilities: ["server.view", "logs.read"] }] } },
    { method: "POST", path: `/users/${friend.id}/reset-password`, body: { password: "first-password-123" } },
    { method: "PATCH", path: `/users/${friend.id}`, body: { role: "viewer", disabled: true } },
  ]);
});
