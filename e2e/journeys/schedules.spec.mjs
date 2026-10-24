import { test, expect, asAdmin, openServer } from "./fixtures.mjs";

asAdmin();

test("scheduling, editing, pausing, and deleting a restart", async ({ page, session }) => {
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Schedules" }).click();
  const form = page.locator("#schedule-form");
  await form.getByLabel("Time zone").fill("Europe/Berlin");

  // A one-time run in the past is refused and the draft stays.
  await form.getByLabel("Once").check();
  await form.getByLabel("Date").fill("2020-01-01");
  await expect(page.locator("#schedule-preview")).toContainText("Choose a future date");
  await form.getByRole("button", { name: "Add schedule" }).click();
  await expect(page.locator("#schedule-form .alert--error")).toContainText("One-time schedules need a date and time in the future.");

  await form.getByLabel("On chosen days").check();
  await form.getByLabel("Action").selectOption("restart");
  await form.getByRole("button", { name: "Weekdays" }).click();
  await form.getByLabel("Time", { exact: true }).fill("05:30");
  await form.getByLabel("Announce countdown to players").check();
  await expect(page.locator("#schedule-preview")).toContainText("05:30 (Europe/Berlin)");
  await form.getByRole("button", { name: "Add schedule" }).click();

  await expect(page).toHaveURL(/saved=created/);
  const row = page.locator("#schedule-list tbody tr").first();
  await expect(row).toContainText("05:30, weekdays");
  await expect(row).toContainText("Warnings: 10, 5, 1 min before");

  await row.getByRole("link", { name: "Edit" }).click();
  await page.locator("#schedule-form").getByLabel("Time", { exact: true }).fill("06:45");
  await page.locator("#schedule-form").getByRole("button", { name: "Save changes" }).click();
  await expect(page).toHaveURL(/saved=saved/);
  await expect(row).toContainText("06:45, weekdays");

  await row.getByRole("button", { name: "Pause" }).click();
  await expect(page.locator("#detail-status")).toHaveText("Schedule paused.");
  await expect(row.locator("td").nth(2)).toHaveText("Paused");
  await row.getByRole("button", { name: "Resume" }).click();
  await expect(row.locator("td").nth(2)).toHaveText("Enabled");

  await page.getByRole("tab", { name: "Overview" }).click();
  await expect(page.locator(".overview-facts")).toContainText("Restart");

  // Someone else pauses the schedule while it is being edited: saving shows
  // the conflict and keeps the draft, which then saves without resuming it.
  await page.getByRole("tab", { name: "Schedules" }).click();
  await row.getByRole("link", { name: "Edit" }).click();
  const other = await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
  await openServer(other, "Survival");
  await other.getByRole("tab", { name: "Schedules" }).click();
  await other.locator("#schedule-list tbody tr").first().getByRole("button", { name: "Pause" }).click();
  await expect(other.locator("#detail-status")).toHaveText("Schedule paused.");

  const editor = page.locator("#schedule-form");
  await editor.getByLabel("Time", { exact: true }).fill("07:15");
  await editor.getByRole("button", { name: "Save changes" }).click();
  await expect(editor.locator(".schedule-conflict")).toContainText("This schedule changed elsewhere. Your draft is preserved.");
  await expect(editor.getByLabel("Time", { exact: true })).toHaveValue("07:15");
  await editor.getByRole("button", { name: "Keep my draft" }).click();
  await expect(editor.getByText("This schedule stays paused when saved.")).toBeVisible();
  await editor.getByRole("button", { name: "Save changes" }).click();
  await expect(page).toHaveURL(/saved=saved/);
  await expect(row).toContainText("07:15, weekdays");
  await expect(row.locator("td").nth(2)).toHaveText("Paused");

  // Cancelling the confirmation keeps the schedule.
  const confirm = page.getByRole("dialog", { name: "Delete this schedule?" });
  await row.getByRole("button", { name: "Delete" }).click();
  await expect(confirm).toContainText("It won’t run again.");
  await confirm.getByRole("button", { name: "Cancel" }).click();
  await expect(confirm).toBeHidden();
  await expect(row).toBeVisible();
  await row.getByRole("button", { name: "Delete" }).click();
  await confirm.getByRole("button", { name: "Delete schedule" }).click();
  await expect(page.locator("#schedule-list")).toContainText("No schedules.");
});
