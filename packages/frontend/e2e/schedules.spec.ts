import type { Page } from "@playwright/test";
import {
  apiErrorSchema,
  operationResponseSchema,
  operationSchema,
  savedScheduleSchema,
  scheduleEnabledRequestSchema,
  scheduleResponseSchema,
  scheduleSchema,
  schedulesResponseSchema,
  updateScheduleRequestSchema,
  type NextRunUnavailableReason,
} from "@ludock/shared";
import { test, expect, ADMIN, RUNNING_ID } from "./fixtures";

const SCHEDULE_ID = "77777777-7777-4777-8777-777777777777";
const NOW = new Date("2026-09-11T12:00:00Z");
const NEXT_RUN = Date.UTC(2026, 8, 12, 9);
const EDITED_NEXT_RUN = Date.UTC(2026, 8, 11, 14, 45);
const OPERATION_ID = "88888888-8888-4888-8888-888888888888";
const LAST_RUN = Date.UTC(2026, 8, 10, 9);
const LAST_OPERATION = operationSchema.parse({
  id: OPERATION_ID,
  serverId: RUNNING_ID,
  kind: "restart",
  status: "failed",
  phase: "Restarting container",
  createdAt: LAST_RUN,
  updatedAt: LAST_RUN + 60_000,
  error: "Container stopped unexpectedly during restart.",
  result: null,
});

async function mockSchedules(page: Page, enabled = true, options: {
  empty?: boolean;
  nextRunUnavailableReason?: NextRunUnavailableReason;
} = {}) {
  let hasSchedule = !options.empty;
  let schedule = savedScheduleSchema.parse({
    id: SCHEDULE_ID,
    serverId: RUNNING_ID,
    ownerId: ADMIN.id,
    action: "restart",
    enabled,
    time: "09:00",
    days: [0, 1, 2, 3, 4, 5, 6],
    timezone: "UTC",
    lastResult: "Queued operation",
    lastOperation: LAST_OPERATION,
    lastRunAt: LAST_RUN,
    lastSlot: null,
    revision: 1,
    nextRunAt: enabled && !options.nextRunUnavailableReason ? NEXT_RUN : null,
    nextRunUnavailableReason: options.nextRunUnavailableReason ?? null,
  });
  const writes: { method: string; body: unknown; expectedRevision?: number }[] = [];
  const operationReads: string[] = [];
  let rejectNextEdit = false;
  await page.clock.setFixedTime(NOW);
  await page.route(`**/api/v1/operations/${OPERATION_ID}`, async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    operationReads.push(OPERATION_ID);
    await route.fulfill({ json: operationResponseSchema.parse({ operation: LAST_OPERATION }) });
  });
  await page.route(`**/api/v1/servers/${RUNNING_ID}/schedules**`, async (route) => {
    const request = route.request();
    const method = request.method();
    const pathname = new URL(request.url()).pathname;
    if (method === "GET" && pathname.endsWith("/schedules")) {
      await route.fulfill({ json: schedulesResponseSchema.parse({ schedules: hasSchedule ? [schedule] : [] }) });
      return;
    }
    if (method === "POST" && pathname.endsWith("/schedules")) {
      const body: unknown = request.postDataJSON();
      const input = scheduleSchema.parse(body);
      writes.push({ method, body });
      schedule = savedScheduleSchema.parse({
        ...schedule,
        ...input,
        lastResult: null,
        lastOperation: null,
        lastRunAt: null,
        revision: 1,
        nextRunAt: input.enabled ? EDITED_NEXT_RUN : null,
        nextRunUnavailableReason: null,
      });
      hasSchedule = true;
      await route.fulfill({ status: 201, json: scheduleResponseSchema.parse({ schedule }) });
      return;
    }
    if (pathname.endsWith(`/schedules/${SCHEDULE_ID}`) && ["PUT", "PATCH"].includes(method)) {
      const body = request.postDataJSON() as Record<string, unknown>;
      writes.push({ method, body, expectedRevision: schedule.revision });
      if (rejectNextEdit && method === "PUT") {
        rejectNextEdit = false;
        await route.fulfill({
          status: 409,
          json: apiErrorSchema.parse({ error: "This schedule changed. Reload before saving again." }),
        });
        return;
      }
      if (method === "PUT") {
        const input = updateScheduleRequestSchema.parse(body);
        schedule = savedScheduleSchema.parse({
          ...schedule,
          ...input,
          revision: schedule.revision + 1,
          nextRunAt: input.enabled ? EDITED_NEXT_RUN : null,
        });
      } else {
        const input = scheduleEnabledRequestSchema.parse(body);
        schedule = savedScheduleSchema.parse({
          ...schedule,
          enabled: input.enabled,
          revision: schedule.revision + 1,
          nextRunAt: input.enabled ? EDITED_NEXT_RUN : null,
        });
      }
      await route.fulfill({ json: scheduleResponseSchema.parse({ schedule }) });
      return;
    }
    await route.fallback();
  });
  return { writes, operationReads, rejectNextEdit: () => { rejectNextEdit = true; } };
}

test("schedules can be edited, paused, and resumed with the latest saved revision", async ({ app, page }, testInfo) => {
  const fixture = await mockSchedules(page);
  await app.open(`/servers/${RUNNING_ID}`);
  await page.getByRole("tab", { name: "Schedules", exact: true }).click();
  const table = page.getByRole("table", { name: "Schedules", exact: true });
  const row = table.getByRole("row").filter({ has: page.getByRole("button", { name: "Edit", exact: true }) });
  await expect(row.getByRole("cell", { name: "Enabled", exact: true })).toBeVisible();
  await expect(row.getByRole("cell").nth(4)).toContainText("Failed");
  await expect(row.getByRole("cell").nth(4)).not.toContainText("Queued operation");
  await expect(row.getByRole("cell").nth(3)).toContainText("UTC");

  const edit = row.getByRole("button", { name: "Edit", exact: true });
  await expect(page.getByRole("checkbox", { name: "Create paused", exact: true })).toBeVisible();
  await edit.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("heading", { name: "Edit schedule", exact: true })).toBeFocused();
  await expect(page.getByRole("checkbox", { name: "Create paused", exact: true })).toHaveCount(0);
  await expect(page.getByRole("combobox", { name: "Action", exact: true })).toHaveValue("restart");
  await expect(page.getByLabel("Time", { exact: true })).toHaveValue("09:00");
  await page.getByLabel("Time", { exact: true }).fill("14:45");
  await expect(page.locator(".schedule-preview")).toContainText(/14:45|2:45/);
  await expect(page.locator(".schedule-preview")).toContainText("UTC");
  const screenshot = testInfo.outputPath("schedule-editor.png");
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.screenshot({ path: screenshot, fullPage: true });
  await testInfo.attach("Schedule editor", { path: screenshot, contentType: "image/png" });
  await page.getByRole("button", { name: "Save changes", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Add schedule", exact: true })).toBeVisible();
  expect(fixture.writes).toEqual([{
    method: "PUT",
    expectedRevision: 1,
    body: {
      action: "restart", enabled: true, time: "14:45", days: [0, 1, 2, 3, 4, 5, 6],
      timezone: "UTC", revision: 1,
    },
  }]);
  await expect(row.getByRole("cell").nth(3)).toContainText(/14:45|2:45/);

  await row.getByRole("button", { name: "Pause", exact: true }).click();
  await expect(row.getByRole("cell").nth(2)).toHaveText("Paused");
  await expect(row.getByRole("cell").nth(4)).toContainText("Failed");
  await expect(row.getByRole("cell").nth(3)).not.toContainText("UTC");
  expect(fixture.writes[1]).toEqual({ method: "PATCH", body: { enabled: false, revision: 2 }, expectedRevision: 2 });

  const resume = row.getByRole("button", { name: "Resume", exact: true });
  await expect(resume).toBeEnabled();
  await resume.focus();
  await page.keyboard.press("Enter");
  await expect(row.getByRole("cell", { name: "Enabled", exact: true })).toBeVisible();
  await expect(row.getByRole("cell").nth(3)).toContainText("UTC");
  expect(fixture.writes[2]).toEqual({ method: "PATCH", body: { enabled: true, revision: 3 }, expectedRevision: 3 });
});

for (const width of [320, 390]) {
  test(`paused schedule drafts preview the selected time zone and remain usable at ${width}px`, async ({ app, page }, testInfo) => {
    const fixture = await mockSchedules(page, false);
    await page.setViewportSize({ width, height: 844 });
    await app.open(`/servers/${RUNNING_ID}`);
    await page.getByRole("tab", { name: "Schedules", exact: true }).click();
    const edit = page.getByRole("button", { name: "Edit", exact: true });
    await edit.click();
    const preview = page.locator(".schedule-preview");
    await expect(preview).toContainText("Next run when resumed:");
    await expect(preview).toContainText("UTC");
    await page.getByLabel("Time zone", { exact: true }).fill("America/Los_Angeles");
    await page.getByLabel("Time", { exact: true }).fill("15:30");
    await expect(preview).toContainText("America/Los_Angeles");
    await expect(preview).toContainText(/15:30|3:30/);
    await expect(page.getByRole("button", { name: "Save changes", exact: true })).toBeEnabled();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    const screenshot = testInfo.outputPath(`schedule-editor-${width}.png`);
    await page.evaluate(() => window.scrollTo(0, 0));
    await page.screenshot({ path: screenshot, fullPage: true });
    await testInfo.attach(`Schedule editor at ${width}px`, { path: screenshot, contentType: "image/png" });

    const cancel = page.getByRole("button", { name: "Cancel editing", exact: true });
    await cancel.focus();
    await page.keyboard.press("Enter");
    await expect(page.getByRole("heading", { name: "Add schedule", exact: true })).toBeVisible();
    await expect(edit).toBeFocused();
    expect(fixture.writes).toEqual([]);
    await edit.click();
    await expect(page.getByLabel("Time", { exact: true })).toHaveValue("09:00");
    await expect(page.getByLabel("Time zone", { exact: true })).toHaveValue("UTC");
  });
}

test("invalid and rejected schedule edits preserve the draft for correction", async ({ app, page }) => {
  const fixture = await mockSchedules(page);
  fixture.rejectNextEdit();
  await app.open(`/servers/${RUNNING_ID}`);
  await page.getByRole("tab", { name: "Schedules", exact: true }).click();
  await page.getByRole("button", { name: "Edit", exact: true }).click();
  await page.getByLabel("Time", { exact: true }).fill("14:45");
  const timezone = page.getByLabel("Time zone", { exact: true });
  const save = page.getByRole("button", { name: "Save changes", exact: true });
  await timezone.fill("Invalid/Timezone");
  await expect(save).toBeDisabled();
  expect(fixture.writes).toEqual([]);
  await timezone.fill("UTC");
  await save.click();
  await expect(page.getByRole("alert")).toContainText("This schedule changed. Reload before saving again.");
  await expect(page.getByRole("heading", { name: "Edit schedule", exact: true })).toBeVisible();
  await expect(page.getByLabel("Time", { exact: true })).toHaveValue("14:45");
  await expect(timezone).toHaveValue("UTC");
  expect(fixture.writes).toHaveLength(1);
  expect(fixture.writes[0]).toMatchObject({ body: { revision: 1 }, expectedRevision: 1 });
});

test("a new schedule can be created paused and only starts running after explicit resume", async ({ app, page }, testInfo) => {
  const fixture = await mockSchedules(page, true, { empty: true });
  await app.open(`/servers/${RUNNING_ID}`);
  await page.getByRole("tab", { name: "Schedules", exact: true }).click();
  await page.getByRole("combobox", { name: "Action", exact: true }).selectOption("restart");
  await page.getByLabel("Time", { exact: true }).fill("14:45");
  await page.getByLabel("Time zone", { exact: true }).fill("UTC");
  await page.getByRole("checkbox", { name: "Create paused", exact: true }).check();
  await expect(page.locator(".schedule-preview")).toContainText("Next run when resumed:");
  await expect(page.locator(".schedule-preview")).toContainText("14:45");
  const screenshot = testInfo.outputPath("schedule-create-paused.png");
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.screenshot({ path: screenshot, fullPage: true });
  await testInfo.attach("Create a paused schedule", { path: screenshot, contentType: "image/png" });
  await page.getByRole("button", { name: "Add schedule", exact: true }).click();

  const table = page.getByRole("table", { name: "Schedules", exact: true });
  const row = table.getByRole("row").filter({ has: page.getByRole("button", { name: "Edit", exact: true }) });
  await expect(row.getByRole("cell").nth(2)).toHaveText("Paused");
  await expect(row.getByRole("cell").nth(3)).toHaveText("Paused");
  await expect(row.getByRole("cell").nth(4)).toContainText("No runs yet");
  expect(fixture.writes).toEqual([{
    method: "POST",
    body: { action: "restart", enabled: false, time: "14:45", days: [0, 1, 2, 3, 4, 5, 6], timezone: "UTC" },
  }]);
  await row.getByRole("button", { name: "Resume", exact: true }).click();
  await expect(row.getByRole("cell").nth(2)).toHaveText("Enabled");
  await expect(row.getByRole("cell").nth(3)).toContainText("14:45");
  expect(fixture.writes[1]).toEqual({ method: "PATCH", body: { enabled: true, revision: 1 }, expectedRevision: 1 });
});

test.describe("schedule controls on touch screens", () => {
  test.use({ hasTouch: true, viewport: { width: 820, height: 1180 } });

  test("row and form actions retain usable touch targets on wider screens", async ({ app, page }) => {
    await mockSchedules(page);
    await app.open(`/servers/${RUNNING_ID}`);
    await page.getByRole("tab", { name: "Schedules", exact: true }).click();
    await expect(page.getByRole("button", { name: "Edit", exact: true })).toBeVisible();
    expect(await page.evaluate(() => matchMedia("(pointer: coarse)").matches)).toBe(true);
    const controls = page.locator(".schedules-table .secondary-btn, .schedule-activity-link, .schedule-form button, .schedule-form .check-label");
    await expect(controls.first()).toBeVisible();
    const heights = await controls.evaluateAll((elements) => elements.map((element) => element.getBoundingClientRect().height));
    expect(heights.length).toBeGreaterThan(0);
    for (const height of heights) expect(height).toBeGreaterThanOrEqual(44);
  });
});

test("a scheduled result opens and focuses its operation even when it is absent from recent activity", async ({ app, page }, testInfo) => {
  const fixture = await mockSchedules(page);
  await app.open(`/servers/${RUNNING_ID}`);
  await page.getByRole("tab", { name: "Schedules", exact: true }).click();
  const table = page.getByRole("table", { name: "Schedules", exact: true });
  const row = table.getByRole("row").filter({ has: page.getByRole("button", { name: "Edit", exact: true }) });
  const result = row.getByRole("cell").nth(4);
  await expect(result).toContainText("Failed");
  await expect(result).toContainText("Sep 10");
  await expect(result).toContainText("09:00");
  await expect(result).toContainText("UTC");
  expect(fixture.operationReads).toEqual([]);
  await result.getByRole("button", { name: "View activity", exact: true }).click();
  await expect(page.getByRole("tab", { name: "Activity", selected: true })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Scheduled operation", exact: true })).toBeFocused();
  const selected = page.getByRole("region", { name: "Scheduled operation", exact: true });
  await expect(selected).toContainText("Failed");
  await expect(selected).toContainText("Container stopped unexpectedly during restart.");
  await expect(page.getByText("No operations yet.", { exact: true })).toBeVisible();
  expect(fixture.operationReads).toContain(OPERATION_ID);
  const screenshot = testInfo.outputPath("scheduled-operation.png");
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.screenshot({ path: screenshot, fullPage: true });
  await testInfo.attach("Scheduled operation details", { path: screenshot, contentType: "image/png" });
});

for (const unavailable of [
  {
    reason: "owner_disabled",
    enabled: true,
    message: "Schedule owner is disabled. Ask an administrator to enable the owner’s account.",
  },
  {
    reason: "binding_changed",
    enabled: true,
    message: "Server identity changed. Ask an administrator to review the binding, then recreate this schedule.",
  },
] as const) {
  test(`schedule next-run guidance explains ${unavailable.reason} while ${unavailable.enabled ? "enabled" : "paused"}`, async ({ app, page }) => {
    await mockSchedules(page, unavailable.enabled, { nextRunUnavailableReason: unavailable.reason });
    await app.open(`/servers/${RUNNING_ID}`);
    await page.getByRole("tab", { name: "Schedules", exact: true }).click();
    const table = page.getByRole("table", { name: "Schedules", exact: true });
    const row = table.getByRole("row").filter({ has: page.getByRole("button", { name: "Edit", exact: true }) });
    await expect(row.getByRole("cell").nth(2)).toHaveText(unavailable.enabled ? "Enabled" : "Paused");
    await expect(row.getByRole("cell").nth(3)).toContainText(unavailable.message);
  });
}
