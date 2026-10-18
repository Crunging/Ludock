import type { Page } from "@playwright/test";
import {
  apiErrorSchema,
  savedScheduleSchema,
  scheduleEnabledRequestSchema,
  scheduleResponseSchema,
  scheduleSchema,
  schedulesResponseSchema,
  updateScheduleRequestSchema,
  type Schedule,
} from "@ludock/shared";
import { test, expect, ADMIN, RUNNING_ID } from "./fixtures";

const SCHEDULE_ID = "77777777-7777-4777-8777-777777777777";
const FINISHED_ONCE_ID = "77777777-7777-4777-8777-777777777778";
const QUEUED_ONCE_ID = "77777777-7777-4777-8777-777777777779";
const NOW = new Date("2026-09-11T12:00:00Z");
const NEXT_RUN = Date.UTC(2026, 8, 12, 9);
const EDITED_NEXT_RUN = Date.UTC(2026, 8, 11, 14, 45);

function savedSchedule(overrides: Record<string, unknown> = {}): Schedule {
  return savedScheduleSchema.parse({
    id: SCHEDULE_ID,
    serverId: RUNNING_ID,
    ownerId: ADMIN.id,
    action: "restart",
    createBackup: false,
    enabled: true,
    time: "09:00",
    timezone: "UTC",
    lastResult: null,
    lastOperation: null,
    lastRunAt: null,
    lastSlot: null,
    revision: 1,
    nextRunAt: NEXT_RUN,
    nextRunUnavailableReason: null,
    completedAt: null,
    ...(overrides.kind === "once" ? {} : { kind: "recurring", days: [0, 1, 2, 3, 4, 5, 6] }),
    ...overrides,
  });
}

/** Keeps a saved schedule's history while replacing its settings with saved input. */
function withSettings(current: Schedule, settings: Record<string, unknown>, revision: number) {
  const { id, serverId, ownerId, lastResult, lastOperation, lastRunAt, lastSlot, nextRunUnavailableReason, completedAt } = current;
  return savedSchedule({
    id, serverId, ownerId, lastResult, lastOperation, lastRunAt, lastSlot, nextRunUnavailableReason, completedAt,
    ...settings,
    revision,
    nextRunAt: settings.enabled ? EDITED_NEXT_RUN : null,
  });
}

async function mockSchedules(page: Page, initial: Schedule[] = [savedSchedule()]) {
  const schedules = [...initial];
  const writes: { method: string; body: unknown; expectedRevision?: number }[] = [];
  let concurrentChange: Record<string, unknown> | null = null;
  await page.clock.setFixedTime(NOW);
  await page.route(`**/api/v1/servers/${RUNNING_ID}/schedules**`, async (route) => {
    const request = route.request();
    const method = request.method();
    const id = new URL(request.url()).pathname.match(/\/schedules\/([^/]+)$/)?.[1];
    if (!id && method === "GET") {
      await route.fulfill({ json: schedulesResponseSchema.parse({ schedules }) });
      return;
    }
    const body = request.postDataJSON() as Record<string, unknown>;
    if (!id && method === "POST") {
      writes.push({ method, body });
      const schedule = withSettings(savedSchedule({ id: crypto.randomUUID() }), scheduleSchema.parse(body), 1);
      schedules.push(schedule);
      await route.fulfill({ status: 201, json: scheduleResponseSchema.parse({ schedule }) });
      return;
    }
    const index = schedules.findIndex((item) => item.id === id);
    if (index < 0 || !["PUT", "PATCH"].includes(method)) {
      await route.fallback();
      return;
    }
    const current = schedules[index];
    writes.push({ method, body, expectedRevision: current.revision });
    if (concurrentChange && method === "PUT") {
      // Another session saved first, so this edit was based on an old revision.
      schedules[index] = savedSchedule({ ...current, ...concurrentChange, revision: current.revision + 1 });
      concurrentChange = null;
      await route.fulfill({
        status: 409,
        json: apiErrorSchema.parse({ error: "This schedule changed. Reload before saving again." }),
      });
      return;
    }
    if (method === "PUT") {
      schedules[index] = withSettings(current, updateScheduleRequestSchema.parse(body), current.revision + 1);
    } else {
      const input = scheduleEnabledRequestSchema.parse(body);
      schedules[index] = savedSchedule({
        ...current,
        enabled: input.enabled,
        revision: current.revision + 1,
        nextRunAt: input.enabled && current.completedAt === null ? EDITED_NEXT_RUN : null,
      });
    }
    await route.fulfill({ json: scheduleResponseSchema.parse({ schedule: schedules[index] }) });
  });
  return {
    schedules,
    writes,
    changeBeforeNextEdit: (change: Record<string, unknown>) => { concurrentChange = change; },
  };
}

test("countdown warnings can be configured, saved, and dropped for other actions", async ({ app, page }) => {
  const fixture = await mockSchedules(page);
  await app.open(`/servers/${RUNNING_ID}?tab=schedules`);
  await page.getByRole("button", { name: "Edit", exact: true }).click();
  await page.getByLabel("Announce countdown to players").check();
  const leads = page.getByLabel("Warning lead times (minutes)");
  const save = page.getByRole("button", { name: "Save changes", exact: true });
  await expect(leads).toHaveValue("10, 5, 1");
  await leads.fill("10, 10");
  await expect(save).toBeDisabled();
  await expect(page.getByRole("alert")).toContainText("unique whole numbers");
  await leads.fill("0, 5");
  await expect(save).toBeDisabled();
  await leads.fill("8, 3, 1");
  await save.click();
  await expect(page.getByRole("heading", { name: "Add schedule", exact: true })).toBeVisible();
  expect(fixture.writes[0]?.body).toMatchObject({ announcements: { leadMinutes: [8, 3, 1] } });
  await expect(page.getByText("Warnings: 8, 3, 1 min before")).toBeVisible();

  await page.getByRole("button", { name: "Edit", exact: true }).click();
  await expect(leads).toHaveValue("8, 3, 1");
  await page.getByRole("combobox", { name: "Action", exact: true }).selectOption("start");
  await expect(page.getByLabel("Announce countdown to players")).toHaveCount(0);
  await save.click();
  await expect(page.getByRole("heading", { name: "Add schedule", exact: true })).toBeVisible();
  expect(fixture.writes[1]?.body).toMatchObject({ action: "start", revision: 2 });
  expect(fixture.writes[1]?.body).not.toHaveProperty("announcements");
  await expect(page.getByText(/Warnings: .* min before/)).toHaveCount(0);
});

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
  await expect(page.getByRole("checkbox", { name: "Create a backup before restarting" })).not.toBeChecked();
  await page.getByRole("checkbox", { name: "Create a backup before restarting" }).check();
  await page.getByLabel("Time", { exact: true }).fill("14:45");
  await page.getByRole("button", { name: "Save changes", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Add schedule", exact: true })).toBeVisible();
  expect(fixture.writes).toEqual([{
    method: "PUT",
    expectedRevision: 1,
    body: {
      kind: "recurring", action: "restart", createBackup: true, enabled: true, time: "14:45", days: [0, 1, 2, 3, 4, 5, 6],
      timezone: "UTC", revision: 1,
    },
  }]);
  await expect(row).toContainText("Backup before restart");

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

test("an edit that conflicts with a newer save keeps the draft for review", async ({ app, page }) => {
  const fixture = await mockSchedules(page);
  fixture.changeBeforeNextEdit({ time: "10:30", enabled: false, nextRunAt: null });
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

  await expect(page.getByRole("alert").filter({ hasText: "This schedule changed. Reload before saving again." })).toBeVisible();
  const conflict = page.getByRole("alert").filter({ hasText: "This schedule changed elsewhere" });
  await expect(conflict).toContainText("Current saved settings: restart · 10:30, every day · UTC · Paused");
  await expect(page.getByRole("heading", { name: "Edit schedule", exact: true })).toBeVisible();
  await expect(page.getByLabel("Time", { exact: true })).toHaveValue("14:45");
  await expect(save).toBeDisabled();
  await conflict.getByRole("button", { name: "Keep my draft", exact: true }).click();
  // The other session's pause is kept; only the reviewed fields are replaced.
  await expect(page.getByText("This schedule stays paused when saved.")).toBeVisible();
  await save.click();
  await expect(page.getByRole("heading", { name: "Add schedule", exact: true })).toBeVisible();
  expect(fixture.writes).toHaveLength(2);
  expect(fixture.writes[1]).toMatchObject({
    expectedRevision: 2,
    body: { time: "14:45", enabled: false, revision: 2 },
  });
});

test("one-time schedules run once at a future time and finished runs cannot be resumed", async ({ app, page }) => {
  const completed = { kind: "once", date: "2026-09-11", time: "09:00", lastSlot: "2026-09-11T09:00:UTC", nextRunAt: null, completedAt: Date.UTC(2026, 8, 11, 9) };
  const operation = { serverId: RUNNING_ID, kind: "restart", createdAt: 0, updatedAt: 0, error: null, result: null };
  const fixture = await mockSchedules(page, [
    savedSchedule(),
    savedSchedule({ ...completed, id: FINISHED_ONCE_ID, lastOperation: {
      ...operation, id: "c15cbd1f-dbb6-444d-8b8f-c5d728b94df1", status: "succeeded", phase: "succeeded",
    } }),
    savedSchedule({ ...completed, id: QUEUED_ONCE_ID, time: "11:00", lastOperation: {
      ...operation, id: "c15cbd1f-dbb6-444d-8b8f-c5d728b94df2", status: "queued", phase: "queued",
    } }),
  ]);
  await app.open(`/servers/${RUNNING_ID}?tab=schedules`);
  const table = page.getByRole("table", { name: "Schedules", exact: true });
  const finished = table.getByRole("row").filter({ hasText: "Once, 2026-09-11 at 09:00" });
  await expect(finished.getByRole("cell").nth(2)).toHaveText("Completed");
  await expect(finished.getByRole("cell").nth(3)).toHaveText("None");
  await expect(finished.getByRole("button", { name: "Pause", exact: true })).toBeDisabled();
  // A completed schedule whose run is still queued can be paused to stop that run.
  const queued = table.getByRole("row").filter({ hasText: "Once, 2026-09-11 at 11:00" });
  await queued.getByRole("button", { name: "Pause", exact: true }).click();
  await expect(queued.getByRole("cell").nth(2)).toHaveText("Paused");
  expect(fixture.writes).toEqual([{ method: "PATCH", body: { enabled: false, revision: 1 }, expectedRevision: 1 }]);

  const add = page.getByRole("button", { name: "Add schedule", exact: true });
  const date = page.getByLabel("Date", { exact: true });
  await page.getByRole("button", { name: "Weekends", exact: true }).click();
  await page.getByRole("radio", { name: "Once", exact: true }).check();
  await expect(page.getByRole("group", { name: "Days" })).toHaveCount(0);
  await expect(date).toHaveValue("2026-09-11");
  await date.fill("2026-09-10");
  await expect(add).toBeDisabled();
  await expect(page.getByText("Choose a future date and time that exists in this time zone.")).toBeVisible();
  await date.fill("2026-09-12");
  await expect(add).toBeEnabled();

  // Editing another schedule neither inherits nor discards the new draft's values.
  await table.getByRole("row").filter({ hasText: "09:00, every day" })
    .getByRole("button", { name: "Edit", exact: true }).click();
  await page.getByRole("radio", { name: "Once", exact: true }).check();
  await expect(date).toHaveValue("2026-09-11");
  await page.getByRole("button", { name: "Cancel editing", exact: true }).click();
  await expect(date).toHaveValue("2026-09-12");
  await page.getByRole("radio", { name: "On chosen days", exact: true }).check();
  await expect(page.getByRole("button", { name: "Weekends", exact: true })).toHaveAttribute("aria-pressed", "true");
  await page.getByRole("radio", { name: "Once", exact: true }).check();
  await expect(date).toHaveValue("2026-09-12");

  await add.click();
  await expect(page.getByText("Schedule created.")).toBeVisible();
  expect(fixture.writes[1]).toEqual({ method: "POST", body: {
    kind: "once", action: "start", createBackup: false, enabled: true, date: "2026-09-12", time: "08:00", timezone: "UTC",
  } });
  await expect(table.getByRole("row").filter({ hasText: "Once, 2026-09-12 at 08:00" })).toBeVisible();
});

test("restart backups and countdown warnings follow the access needed to run them", async ({ app, page }) => {
  const fixture = await mockSchedules(page);
  await app.open(`/servers/${RUNNING_ID}?tab=schedules`);
  const action = page.getByRole("combobox", { name: "Action", exact: true });
  const backup = page.getByRole("checkbox", { name: "Create a backup before restarting" });
  await expect(backup).toHaveCount(0);
  await action.selectOption("restart");
  await expect(backup).not.toBeChecked();
  await backup.check();
  await action.selectOption("stop");
  await expect(backup).toHaveCount(0);
  await action.selectOption("restart");
  await expect(backup).not.toBeChecked();
  await backup.check();
  await page.getByRole("button", { name: "Add schedule", exact: true }).click();
  await expect(page.getByText("Schedule created.")).toBeVisible();
  expect(fixture.writes[0]?.body).toMatchObject({ action: "restart", createBackup: true });

  // After losing backup and console access, saved options may be removed but not kept or resumed.
  fixture.schedules.splice(0, fixture.schedules.length, savedSchedule({
    createBackup: true, announcements: { leadMinutes: [5] }, enabled: false, nextRunAt: null,
  }));
  app.servers[0].permissions = app.servers[0].permissions
    .filter((capability) => capability !== "backups.create" && capability !== "console.execute");
  await page.reload();
  await expect(page.getByRole("button", { name: "Resume", exact: true })).toBeDisabled();
  await action.selectOption("restart");
  await expect(backup).toHaveCount(0);
  await page.getByRole("button", { name: "Edit", exact: true }).click();
  const save = page.getByRole("button", { name: "Save changes", exact: true });
  await expect(backup).toBeChecked();
  await expect(save).toBeDisabled();
  await backup.click();
  await expect(backup).toHaveCount(0);
  await expect(page.getByText(/Player warnings require a supported broadcast console and console access/)).toBeVisible();
  await expect(save).toBeDisabled();
  await page.getByRole("button", { name: "Turn off saved warnings", exact: true }).click();
  await save.click();
  await expect(page.getByText("Schedule saved.")).toBeVisible();
  expect(fixture.writes[1]).toMatchObject({ method: "PUT", body: { createBackup: false, enabled: false, revision: 1 } });
  expect(fixture.writes[1]?.body).not.toHaveProperty("announcements");
});
