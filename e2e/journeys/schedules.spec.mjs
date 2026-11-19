import { test, expect, asAdmin, createUser, dockerAction, openServer, signIn } from "./fixtures.mjs";

asAdmin();

// Fills in a one-time run at the current UTC minute, which the scheduler's
// next check, at most 15 seconds away, runs. A minute about to end is waited
// out so the run can't fall into the past while the form is filled.
async function dueNow(form) {
  await expect(form.getByRole("button", { name: /^Use my time zone \(/ })).toBeVisible();
  let now = new Date();
  if (now.getUTCSeconds() >= 45) {
    await new Promise((done) => setTimeout(done, (61 - now.getUTCSeconds()) * 1000));
    now = new Date();
  }
  await form.getByLabel("Time zone").fill("UTC");
  await form.getByLabel("Once").check();
  await form.getByLabel("Date").fill(now.toISOString().slice(0, 10));
  await form.getByLabel("Time", { exact: true }).fill(now.toISOString().slice(11, 16));
}

// The list refreshes only after changes made on the page, so the run's
// result is read by reloading.
async function expectResult(page, row, text) {
  await expect(async () => {
    await page.reload();
    await expect(row.locator(".schedule-result")).toContainText(text, { timeout: 1000 });
  }).toPass({ timeout: 45_000, intervals: [2000] });
}

async function addCommand(page, command) {
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Schedules" }).click();
  const form = page.locator("#schedule-form");
  await form.getByLabel("Action").selectOption("command");
  await expect(form.getByText("Anyone who can see this schedule can read it, so leave passwords out.")).toBeVisible();
  await dueNow(form);
  // The preview shows the time before a command is typed.
  await expect(page.locator("#schedule-preview")).toContainText("(UTC)");
  await form.getByLabel("Console command").fill(command);
  await form.getByRole("button", { name: "Add schedule" }).click();
  await expect(page).toHaveURL(/saved=created/);
  const row = page.locator("#schedule-list tbody tr", { hasText: command });
  await expect(row.locator("td").first()).toContainText("Send command");
  return row;
}

test("scheduling, editing, pausing, and deleting a restart", async ({ page, session }) => {
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Schedules" }).click();
  const form = page.locator("#schedule-form");
  // The page fills in the browser's time zone once it loads; typing earlier
  // would add to it.
  await expect(form.getByRole("button", { name: /^Use my time zone \(/ })).toBeVisible();
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

test("a new schedule starts on an action its operator can use", async ({ page, session }) => {
  await createUser(page, "scheduler", "scheduler password one", "operator");
  const survival = page.locator("[data-grants] .grants-server", { hasText: "Survival" });
  await survival.getByLabel("Restart").check();
  await survival.getByLabel("Manage own schedules").check();
  await page.getByRole("button", { name: "Save server access" }).click();
  await expect(page.locator("[data-grants] .alert--success")).toBeVisible();

  const operator = await session({ storageState: { cookies: [], origins: [] } });
  await signIn(operator, "scheduler", "scheduler password one");
  await operator.getByRole("link", { name: "Survival", exact: true }).click();
  await operator.getByRole("tab", { name: "Schedules" }).click();
  const form = operator.locator("#schedule-form");
  await expect(form.getByLabel("Action")).toHaveValue("restart");
  await expect(form.getByText("Stops and starts the server at the selected time.")).toBeVisible();
  await expect(form.getByText("Starts the server if it is stopped.")).toBeHidden();
  await expect(operator.locator("#schedule-preview")).toContainText("08:00");
  // Sending commands needs console access too.
  await expect(form.getByLabel("Action").locator("option", { hasText: "Send command" })).toHaveCount(0);

  await survival.getByLabel("Send console commands").check();
  await page.getByRole("button", { name: "Save server access" }).click();
  await expect(page.locator("[data-grants] .alert--success")).toBeVisible();
  await operator.reload();
  await operator.locator("#schedule-form").getByLabel("Action").selectOption("command");
  await expect(operator.locator("#schedule-form").getByLabel("Console command")).toBeVisible();
});

test("a scheduled command reaches the game console", async ({ page }) => {
  test.setTimeout(120_000);
  // The fake game answers "say" with what players see.
  const row = await addCommand(page, "say Saving soon");
  await expectResult(page, row, "Succeeded");
  await expect(row.locator(".schedule-reply")).toHaveText("Reply: [Server] Saving soon");
  await expect(row.locator("td").nth(2)).toHaveText("Completed");
  await row.getByRole("link", { name: "View activity" }).click();
  await expect(page.locator("#selected-operation")).toContainText("Command");
  await expect(page.locator("#selected-operation")).toContainText("Succeeded");
});

test("a scheduled command skips a stopped server", async ({ page }) => {
  test.setTimeout(120_000);
  // Stopped first, so no scheduler check can send the command before.
  await dockerAction("minecraft", "stop");
  try {
    const row = await addCommand(page, "save-all");
    await expectResult(page, row, "Skipped: the server wasn't running");
    await expect(row.getByRole("link", { name: "View activity" })).toHaveCount(0);
  } finally {
    await dockerAction("minecraft", "start");
  }
});
