import type { Page } from "@playwright/test";
import {
  attentionResponseSchema,
  availabilityResponseSchema,
  operationResponseSchema,
  savedScheduleSchema,
  schedulesResponseSchema,
  serverResponseSchema,
  type AttentionItem,
} from "@ludock/shared";
import { test, expect, ADMIN, RUNNING_ID, RUNNING_NAME, STOPPED_ID } from "./fixtures";

const OPERATION_ID = "22222222-2222-4222-8222-222222222222";
const SCHEDULE_ID = "77777777-7777-4777-8777-777777777777";
const OUTAGE_START = Date.UTC(2026, 8, 11, 10);
const base = { serverId: RUNNING_ID, serverName: RUNNING_NAME };
const items: AttentionItem[] = [
  { ...base, kind: "operation", id: `operation:${OPERATION_ID}`, operationId: OPERATION_ID,
    operationKind: "backup", status: "failed", updatedAt: OUTAGE_START },
  { ...base, kind: "schedule", id: `schedule:${SCHEDULE_ID}`, scheduleId: SCHEDULE_ID,
    action: "restart", reason: "owner_disabled" },
  { serverId: STOPPED_ID, serverName: "Factorio weekend", kind: "binding", id: `binding:${STOPPED_ID}`,
    bindingStatus: "review_required" },
  { ...base, kind: "availability", id: `availability:${RUNNING_ID}`, state: "unhealthy", outageStartedAt: OUTAGE_START },
];

async function mockAttention(page: Page, current = items) {
  await page.route("**/api/v1/attention", (route) => route.fulfill({
    json: attentionResponseSchema.parse({ items: current, discoveryUnavailable: false }),
  }));
}

async function mockAvailability(page: Page) {
  await page.route(`**/api/v1/servers/${RUNNING_ID}/availability`, (route) => route.fulfill({
    json: availabilityResponseSchema.parse({
      policy: { enabled: true, maintenance: false, graceSeconds: 120 },
      state: { outageStartedAt: OUTAGE_START, notified: true, suppressedUntil: 0,
        intentionallyStopped: false, lastState: "unhealthy" },
    }),
  }));
}

async function mockFailedOperation(page: Page) {
  // This failure is deliberately absent from the recent operation list. The
  // resolution link must retrieve the selected result directly.
  await page.route(`**/api/v1/operations/${OPERATION_ID}`, (route) => route.fulfill({
    json: operationResponseSchema.parse({ operation: {
      id: OPERATION_ID, serverId: RUNNING_ID, kind: "backup", status: "failed",
      phase: "Copying game data", createdAt: OUTAGE_START, updatedAt: OUTAGE_START,
      error: "Backup destination is unavailable.", result: null,
    } }),
  }));
}

test("dashboard attention links open the failed operation, suspended schedule, binding review, and outage", { tag: "@responsive" }, async ({ app, page }) => {
  app.servers[1].bindingStatus = "review_required";
  await mockAttention(page);
  await mockAvailability(page);
  await mockFailedOperation(page);
  const schedule = savedScheduleSchema.parse({
    id: SCHEDULE_ID, serverId: RUNNING_ID, ownerId: ADMIN.id,
    action: "restart", enabled: true, time: "09:00", days: [0, 1, 2, 3, 4, 5, 6], timezone: "UTC",
    lastResult: null, lastOperation: null, lastRunAt: null, lastSlot: null,
    revision: 1, nextRunAt: null, nextRunUnavailableReason: "owner_disabled",
  });
  await page.route(`**/api/v1/servers/${RUNNING_ID}/schedules`, (route) => route.fulfill({
    json: schedulesResponseSchema.parse({ schedules: [schedule] }),
  }));

  await app.open();
  const attention = page.getByRole("region", { name: "Needs attention" });
  await expect(attention.getByRole("listitem")).toHaveCount(4);
  const activityLink = attention.getByRole("link", { name: /^Review activity:/ });
  await activityLink.focus();
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(`/servers/${RUNNING_ID}?tab=activity&operation=${OPERATION_ID}`);
  await expect(page.getByRole("tab", { name: "Activity", exact: true })).toHaveAttribute("aria-selected", "true");
  await expect(page.getByRole("region", { name: "Operation details" })).toContainText("Backup destination is unavailable.");

  await app.open();
  await attention.getByRole("link", { name: /^Review schedule:/ }).click();
  await expect(page).toHaveURL(`/servers/${RUNNING_ID}?tab=schedules&schedule=${SCHEDULE_ID}`);
  await expect(page.getByRole("tab", { name: "Schedules", exact: true })).toHaveAttribute("aria-selected", "true");
  const row = page.locator(`#schedule-${SCHEDULE_ID}`);
  await expect(row).toHaveAttribute("aria-current", "true");
  await expect(row).toBeFocused();
  await expect(row).toContainText("disabled");
  await page.getByRole("link", { name: "Files", exact: true }).click();
  await expect(page.getByText("server.properties", { exact: true })).toBeVisible();
  await page.getByRole("link", { name: "Back to server", exact: true }).click();
  await expect(page.getByRole("tab", { name: "Schedules", selected: true })).toBeVisible();

  await app.open();
  await attention.getByRole("link", { name: /^Review server:/ }).click();
  await expect(page).toHaveURL(`/servers/${STOPPED_ID}`);
  await expect(page.getByText(/This server’s binding is review required/)).toBeVisible();
  await expect(page.getByRole("button", { name: "Accept binding", exact: true })).toBeDisabled();

  await app.open();
  await attention.getByRole("link", { name: /^Check availability:/ }).click();
  await expect(page).toHaveURL(`/servers/${RUNNING_ID}?tab=availability`);
  await expect(page.getByRole("tab", { name: "Availability", exact: true })).toHaveAttribute("aria-selected", "true");
  const availability = page.getByRole("region", { name: "Availability monitoring" });
  await expect(availability).toContainText("Availability problem detected.");
  await expect(availability).toContainText("Outage since");
  await expect(availability).toContainText("unhealthy");
  expect(app.requests.filter((request) => request.method !== "GET")).toEqual([]);
});

test("saved attention links remain useful while Docker is unavailable and controls stay paused", async ({ app, page }) => {
  await page.route("**/api/v1/servers", (route) => route.fulfill({ status: 503, json: { error: "Docker unavailable" } }));
  await page.route("**/api/v1/attention", (route) => route.fulfill({
    json: attentionResponseSchema.parse({
      items: [items[0], { ...items[3], state: "docker_unavailable" }],
      discoveryUnavailable: true,
    }),
  }));
  await page.route(`**/api/v1/servers/${RUNNING_ID}`, (route) => route.fulfill({
    json: serverResponseSchema.parse({
      server: { ...app.servers[0], state: "unknown", status: "Live status unavailable",
        image: "", fileRoots: [], gameConsole: null, ports: [] },
      stats: null, discoveryUnavailable: true,
    }),
  }));
  await mockFailedOperation(page);
  await page.route(`**/api/v1/servers/${RUNNING_ID}/availability`, (route) => route.fulfill({
    json: availabilityResponseSchema.parse({
      policy: { enabled: true, maintenance: false, graceSeconds: 120 },
      state: { outageStartedAt: OUTAGE_START, notified: true, suppressedUntil: 0,
        intentionallyStopped: false, lastState: "docker_unavailable" },
    }),
  }));
  await app.open();
  const attention = page.getByRole("region", { name: "Needs attention" });
  await expect(attention).toContainText("These items use saved state");
  await attention.getByRole("link", { name: /^Review activity:/ }).click();
  await expect(page.getByRole("region", { name: "Operation details" })).toContainText("Backup destination is unavailable.");
  await expect(page.getByText(/Live server status could not be verified/)).toBeVisible();
  await expect(page.getByRole("button", { name: /^(Start|Stop|Restart)$/ })).toHaveCount(0);

  await app.open();
  await attention.getByRole("link", { name: /^Check availability:/ }).click();
  const availability = page.getByRole("region", { name: "Availability monitoring" });
  await expect(availability).toContainText("Outage since");
  await expect(availability).toContainText("Ludock cannot reach Docker to verify this server.");
  await expect(page.getByText(/Live server status could not be verified/)).toBeVisible();
  await expect(page.getByRole("button", { name: /^(Start|Stop|Restart)$/ })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Save monitoring" })).toBeDisabled();
  expect(app.requests.filter((request) => request.method !== "GET")).toEqual([]);
});

test("a viewer can investigate an outage with monitoring settings kept read-only", async ({ app, page }) => {
  app.user = { ...ADMIN, role: "viewer" };
  app.servers = [{ ...app.servers[0], permissions: ["server.view"] }];
  await mockAttention(page, items.filter((item) => item.kind === "availability"));
  await mockAvailability(page);
  await app.open();
  await page.getByRole("link", { name: /^Check availability:/ }).click();
  const availability = page.getByRole("region", { name: "Availability monitoring" });
  await expect(availability).toContainText("Availability problem detected.");
  await expect(availability).toContainText("Ask an administrator to investigate");
  await expect(page.getByRole("button", { name: "Save monitoring" })).toHaveCount(0);
  await expect(page.getByRole("checkbox", { name: "Monitor this server" })).toHaveCount(0);
  expect(app.requests.filter((request) => request.method !== "GET")).toEqual([]);
});

for (const width of [320, 390]) {
  test(`attention actions remain readable and keyboard accessible at ${width}px`, { tag: "@mobile" }, async ({ app, page }) => {
    await mockAttention(page, items.map((item, index) => index === 0 ? {
      ...item, serverName: "Friends’ survival world — multiplayer-survival-with-a-long-server-name",
    } : item));
    await page.setViewportSize({ width, height: 844 });
    await app.open();
    const attention = page.getByRole("region", { name: "Needs attention" });
    await expect(attention.getByRole("listitem")).toHaveCount(4);
    const links = attention.getByRole("link");
    for (const link of await links.all()) {
      await link.focus();
      await expect(link).toBeFocused();
      await expect(link).toBeInViewport();
      const bounds = await link.boundingBox();
      expect(bounds?.height).toBeGreaterThanOrEqual(44);
    }
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  });
}
