import { test, expect, asAdmin, dockerAction, gameCommand, openServer } from "../fixtures.mjs";

asAdmin();
// Each journey waits for the scheduler, so they run beside each other.
test.describe.configure({ mode: "parallel" });

/** Opens the schedule form's More options once the page has filled in the
 * browser's time zone; typing earlier would add to it. */
async function moreOptions(form) {
  await expect(form.locator("input[name=timezone]")).not.toHaveValue("");
  const more = form.locator("#schedule-more");
  if (!(await more.evaluate((details) => details.open))) await more.locator("summary").click();
}

// Fills in a one-time run at the current UTC minute, which the scheduler's
// next check, at most 15 seconds away, runs. A minute about to end is waited
// out so the run can't fall into the past while the form is filled.
async function dueNow(form) {
  await moreOptions(form);
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
async function expectResult(page, row, text, timeout = 45_000) {
  await expect(async () => {
    await page.reload();
    await expect(row.locator(".schedule-result")).toContainText(text, { timeout: 1000 });
  }).toPass({ timeout, intervals: [2000] });
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

// Adds a restart due now that waits for an empty server. A one-minute wait
// ends at the end of the scheduled minute, so it lasts at most a minute.
async function addWaitingRestart(page, minutes, then) {
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Schedules" }).click();
  const form = page.locator("#schedule-form");
  await form.getByLabel("Back up first").uncheck();
  await dueNow(form);
  await form.getByLabel("Wait until no one is online").check();
  await form.getByLabel("Wait up to (minutes)").fill(String(minutes));
  await form.getByLabel(then).check();
  // Warnings announce a run that goes ahead, so skipping turns them off.
  if (then === "Skip this run") await form.getByLabel("Warn players in the game").uncheck();
  await form.getByRole("button", { name: "Add schedule" }).click();
  await expect(page).toHaveURL(/saved=created/);
  const summary = `Waits up to ${minutes} min for an empty server, then ${then === "Skip this run" ? "skips" : "goes ahead"}`;
  return page.locator("#schedule-list tbody tr", { hasText: summary }).last();
}

async function expectWaiting(page, row) {
  await expect(async () => {
    await page.reload();
    await expect(row.locator(".schedule-next-run")).toContainText("When the server is empty", { timeout: 1000 });
  }).toPass({ timeout: 45_000, intervals: [2000] });
}

test("a scheduled restart waits for players to leave", async ({ page }) => {
  test.setTimeout(150_000);
  const console = await page.context().newPage();
  await gameCommand(console, "fake-join Mara");
  try {
    const row = await addWaitingRestart(page, 30, "Skip this run");
    await expectWaiting(page, row);
    await expect(row.locator(".schedule-next-run")).toContainText("then skipping");
    await expect(row.locator(".schedule-result")).toContainText("Waiting for players to leave");
    await page.getByRole("tab", { name: "Activity" }).click();
    await expect(page.locator(".activity-waiting")).toContainText("A scheduled restart is waiting for players to leave, until");
    await page.getByRole("tab", { name: "Overview" }).click();
    await expect(page.locator(".overview-facts")).toContainText("Restart waiting for players to leave, until");
  } finally {
    await gameCommand(console, "fake-leave Mara");
  }
  // The restart goes ahead once the server is empty.
  await page.getByRole("tab", { name: "Schedules" }).click();
  await expect(page).toHaveURL(/tab=schedules/);
  const row = page.locator("#schedule-list tbody tr", { hasText: "Waits up to 30 min" }).last();
  await expectResult(page, row, "Succeeded", 75_000);
  await expect(row.locator("td").nth(2)).toHaveText("Completed");
});

test("a waiting restart goes ahead anyway at the end of its wait", async ({ page }) => {
  test.setTimeout(150_000);
  const console = await page.context().newPage();
  await gameCommand(console, "fake-join Ada");
  try {
    const row = await addWaitingRestart(page, 1, "Go ahead anyway");
    await expectWaiting(page, row);
    await expect(row.locator(".schedule-next-run")).toContainText("then going ahead anyway");
    await expectResult(page, row, "Succeeded", 100_000);
  } finally {
    await gameCommand(console, "fake-leave Ada");
  }
});

test("a waiting restart is skipped when players stay online", async ({ page }) => {
  test.setTimeout(150_000);
  const console = await page.context().newPage();
  await gameCommand(console, "fake-join Theo");
  try {
    const row = await addWaitingRestart(page, 1, "Skip this run");
    await expectWaiting(page, row);
    await expectResult(page, row, "Skipped: players were still online when the wait ended", 100_000);
    await expect(row.getByRole("link", { name: "View activity" })).toHaveCount(0);
  } finally {
    await gameCommand(console, "fake-leave Theo");
  }
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
