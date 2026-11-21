import { test, expect, asAdmin, gameCommand, openServer } from "../fixtures.mjs";

asAdmin();
// Each journey waits for the scheduler, so they run beside each other, each
// on a server of its own.
test.describe.configure({ mode: "parallel" });

/** Opens the schedule form's More options once the page has filled in the
 * browser's time zone; typing earlier would add to it. */
async function moreOptions(form) {
  await expect(form.locator("input[name=timezone]")).not.toHaveValue("");
  const more = form.locator("#schedule-more");
  if (!(await more.evaluate((details) => details.open))) await more.locator("summary").click();
}

/** Waits for the next UTC minute unless this one has seconds left. */
async function roomInMinute(seconds) {
  const left = 60 - new Date().getUTCSeconds();
  if (left <= seconds) await new Promise((done) => setTimeout(done, (left + 1) * 1000));
}

// Fills in a one-time run at the current UTC minute, which the scheduler's
// next check, at most 15 seconds away, runs. A minute about to end is waited
// out so the run can't fall into the past while the form is filled.
async function dueNow(form) {
  await moreOptions(form);
  await roomInMinute(15);
  const now = new Date();
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

// Adds a restart due now that waits for an empty server. A one-minute wait
// ends at the end of the scheduled minute, so it lasts at most a minute.
async function addWaitingRestart(page, server, minutes, then) {
  await openServer(page, server);
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
  const summary = `Waits up to ${minutes} ${minutes === 1 ? "minute" : "minutes"} for everyone to leave, then ${then === "Skip this run" ? "skips" : "goes ahead"}`;
  return page.locator("#schedule-list tbody tr", { hasText: summary }).last();
}

async function expectWaiting(page, row) {
  await expect(async () => {
    await page.reload();
    await expect(row.locator(".schedule-next-run")).toContainText("When the server is empty", { timeout: 1000 });
  }).toPass({ timeout: 45_000, intervals: [2000] });
}

// Two servers wait side by side: a waiting run cancelled in place ends
// without acting, and one left waiting goes ahead once its server is empty.
test("a scheduled restart waits for players to leave, and a waiting run can be cancelled", async ({ page, ownServer, otherServer }) => {
  test.setTimeout(150_000);
  const console = await page.context().newPage();
  await gameCommand(console, "fake-join Mara", ownServer.name);
  await gameCommand(console, "fake-join Iris", otherServer.name);
  try {
    const cancelled = await addWaitingRestart(page, otherServer.name, 30, "Skip this run");
    await expectWaiting(page, cancelled);
    await cancelled.getByRole("button", { name: "Cancel this run" }).click();
    await expect(page.locator("#detail-status")).toHaveText("Cancelled. The schedule’s next run is unchanged.");
    await expect(cancelled.getByRole("button", { name: "Cancel this run" })).toHaveCount(0);
    await expect(cancelled.locator(".schedule-result")).toContainText("Cancelled by admin while it waited for players to leave");
    await expect(cancelled.locator(".schedule-next-run")).not.toContainText("When the server is empty");

    const row = await addWaitingRestart(page, ownServer.name, 30, "Skip this run");
    await expectWaiting(page, row);
    await expect(row.locator(".schedule-next-run")).toContainText("then skipping");
    await expect(row.locator(".schedule-result")).toContainText("Waiting for players to leave");
    await page.getByRole("tab", { name: "Activity" }).click();
    await expect(page.locator(".activity-waiting")).toContainText("A scheduled restart is waiting for players to leave, until");
    await page.getByRole("tab", { name: "Overview" }).click();
    await expect(page.locator(".overview-facts").first()).toContainText("Restart waiting for players to leave, until");
  } finally {
    await gameCommand(console, "fake-leave Mara", ownServer.name);
    await gameCommand(console, "fake-leave Iris", otherServer.name);
  }
  // The restart goes ahead once the server is empty.
  await page.getByRole("tab", { name: "Schedules" }).click();
  await expect(page).toHaveURL(/tab=schedules/);
  const row = page.locator("#schedule-list tbody tr", { hasText: "Waits up to 30 min" }).first();
  await expectResult(page, row, "Succeeded", 75_000);
  await expect(row.locator("td").nth(2)).toHaveText("Done");
});

// The one-minute warning goes out between 60 and 30 seconds before the run's
// minute, so a run two minutes ahead is always warned after the form is
// saved, and then runs on its minute.
test("a scheduled restart warns the players a minute ahead, then runs on its minute", async ({ page, ownServer }) => {
  test.setTimeout(200_000);
  await openServer(page, ownServer.name);
  await page.getByRole("tab", { name: "Schedules" }).click();
  const form = page.locator("#schedule-form");
  await form.getByLabel("Back up first").uncheck();
  await moreOptions(form);
  await form.getByLabel("Wait until no one is online").uncheck();
  const runAt = new Date(Date.now() + 2 * 60_000);
  const time = runAt.toISOString().slice(11, 16);
  await form.getByLabel("Time zone").fill("UTC");
  await form.getByLabel("Once").check();
  await form.getByLabel("Date").fill(runAt.toISOString().slice(0, 10));
  await form.getByLabel("Time", { exact: true }).fill(time);
  await form.getByLabel("Warning lead times (minutes)").fill("1");
  await form.getByRole("button", { name: "Add schedule" }).click();
  await expect(page).toHaveURL(/saved=created/);
  const row = page.locator("#schedule-list tbody tr", { hasText: time }).filter({ hasText: "Warns players 1 minute before" }).last();
  const console = await page.context().newPage();
  await expect(async () => {
    await gameCommand(console, "fake-said", ownServer.name);
    await expect(console.locator(".xterm-rows")).toContainText("Server restart in 1 minute.", { timeout: 2000 });
  }).toPass({ timeout: 120_000, intervals: [5000] });
  await expectResult(page, row, "Succeeded", 90_000);
  await expect(row.locator("td").nth(2)).toHaveText("Done");
  await row.getByRole("link", { name: "View activity" }).click();
  await expect(page.locator("#selected-operation")).toContainText("Restart");
  await expect(page.locator("#selected-operation")).toContainText("Succeeded");
});
