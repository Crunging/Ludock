import type { Page } from "@playwright/test";
import {
  apiErrorSchema,
  savedScheduleSchema,
  scheduleEnabledRequestSchema,
  scheduleResponseSchema,
  schedulesResponseSchema,
  updateScheduleRequestSchema,
} from "@ludock/shared";
import { test, expect, ADMIN, RUNNING_ID } from "./fixtures";

const SCHEDULE_ID = "77777777-7777-4777-8777-777777777777";
const NOW = new Date("2026-09-11T12:00:00Z");
const NEXT_RUN = Date.UTC(2026, 8, 12, 9);
const EDITED_NEXT_RUN = Date.UTC(2026, 8, 11, 14, 45);

async function mockSchedules(page: Page) {
  let schedule = savedScheduleSchema.parse({
    id: SCHEDULE_ID,
    serverId: RUNNING_ID,
    ownerId: ADMIN.id,
    action: "restart",
    enabled: true,
    time: "09:00",
    days: [0, 1, 2, 3, 4, 5, 6],
    timezone: "UTC",
    lastResult: null,
    lastOperation: null,
    lastRunAt: null,
    lastSlot: null,
    revision: 1,
    nextRunAt: NEXT_RUN,
    nextRunUnavailableReason: null,
  });
  const writes: { method: string; body: unknown; expectedRevision?: number }[] = [];
  let rejectNextEdit = false;
  await page.clock.setFixedTime(NOW);
  await page.route(`**/api/v1/servers/${RUNNING_ID}/schedules**`, async (route) => {
    const request = route.request();
    const method = request.method();
    const pathname = new URL(request.url()).pathname;
    if (method === "GET" && pathname.endsWith("/schedules")) {
      await route.fulfill({ json: schedulesResponseSchema.parse({ schedules: [schedule] }) });
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
  return { writes, rejectNextEdit: () => { rejectNextEdit = true; } };
}

test("schedules can be edited, paused, and resumed with the latest saved revision", async ({ app, page }) => {
  const fixture = await mockSchedules(page);
  await app.open(`/servers/${RUNNING_ID}`);
  await page.getByRole("tab", { name: "Schedules", exact: true }).click();
  const table = page.getByRole("table", { name: "Schedules", exact: true });
  const row = table.getByRole("row").filter({ has: page.getByRole("button", { name: "Edit", exact: true }) });
  await expect(row.getByRole("cell", { name: "Enabled", exact: true })).toBeVisible();

  const edit = row.getByRole("button", { name: "Edit", exact: true });
  await edit.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("combobox", { name: "Action", exact: true })).toHaveValue("restart");
  await expect(page.getByLabel("Time", { exact: true })).toHaveValue("09:00");
  await page.getByLabel("Time", { exact: true }).fill("14:45");
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

  await row.getByRole("button", { name: "Pause", exact: true }).click();
  await expect(row.getByRole("cell").nth(2)).toHaveText("Paused");
  expect(fixture.writes[1]).toEqual({ method: "PATCH", body: { enabled: false, revision: 2 }, expectedRevision: 2 });

  const resume = row.getByRole("button", { name: "Resume", exact: true });
  await expect(resume).toBeEnabled();
  await resume.focus();
  await page.keyboard.press("Enter");
  await expect(row.getByRole("cell", { name: "Enabled", exact: true })).toBeVisible();
  expect(fixture.writes[2]).toEqual({ method: "PATCH", body: { enabled: true, revision: 3 }, expectedRevision: 3 });
});

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
