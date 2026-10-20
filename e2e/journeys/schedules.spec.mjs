import { test, expect, asAdmin, openServer } from "./fixtures.mjs";

asAdmin();

test("scheduling, editing, pausing, and deleting a restart", async ({ page }) => {
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

  await page.getByRole("tab", { name: "Schedules" }).click();
  await row.getByRole("button", { name: "Delete" }).click();
  await expect(page.locator("#schedule-list")).toContainText("No schedules.");
});
