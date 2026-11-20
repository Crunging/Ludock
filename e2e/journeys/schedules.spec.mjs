import { test, expect, asAdmin, createUser, dockerAction, openServer, serverRow, signIn, shareServer } from "./fixtures.mjs";

asAdmin();

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

test("scheduling, editing, pausing, and deleting a restart", async ({ page, session }) => {
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Schedules" }).click();
  const form = page.locator("#schedule-form");
  await moreOptions(form);
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
  await expect(form.getByLabel("Warn players in the game")).toBeChecked();
  await expect(page.locator("#schedule-preview")).toContainText("05:30 (Europe/Berlin)");
  await form.getByRole("button", { name: "Add schedule" }).click();

  await expect(page).toHaveURL(/saved=created/);
  const row = page.locator("#schedule-list tbody tr").first();
  await expect(row).toContainText("05:30, weekdays");
  await expect(row).toContainText("Warnings: 5, 1 min before");

  // Editing keeps its warnings, said in one line.
  await row.getByRole("link", { name: "Edit" }).click();
  await expect(page.locator("#schedule-preview")).toContainText("Anyone still on is warned in the game 5 and 1 minutes before it goes ahead.");
  await expect(page.locator("#schedule-form").getByLabel("Warn players in the game")).toBeChecked();
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

// The common case needs no choices: a daily restart at 08:00 in the
// browser's time zone that backs up first, waits for players to leave, and
// warns anyone still on. The rest waits under More options.
test("a new schedule opens on a daily restart that backs up first", async ({ page }) => {
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Schedules" }).click();
  const form = page.locator("#schedule-form");
  await expect(form.getByLabel("Action")).toHaveValue("restart");
  await expect(form.getByLabel("Back up first")).toBeChecked();
  await expect(form.getByRole("button", { name: "Every day" })).toHaveAttribute("aria-pressed", "true");
  await expect(form.getByLabel("Time", { exact: true })).toHaveValue("08:00");
  const zone = await page.evaluate(() => Intl.DateTimeFormat().resolvedOptions().timeZone);
  await expect(page.locator("#schedule-preview")).toContainText(`08:00 (${zone})`);
  await expect(page.locator("#schedule-preview")).toContainText(
    "Players: Waits up to 30 minutes for everyone to leave. Anyone still on is warned in the game 5 and 1 minutes before it goes ahead.");
  await expect(form.locator("input[name=timezone]")).toBeHidden();
  await expect(form.getByLabel("Wait until no one is online")).toBeHidden();
  await expect(form.getByLabel("Create paused")).toHaveCount(0);
  await expect(form.getByRole("button", { name: /Use my time zone/ })).toHaveCount(0);
  await moreOptions(form);
  await expect(form.locator("input[name=timezone]")).toHaveValue(zone);
  await expect(form.getByLabel("Wait until no one is online")).toBeChecked();
  // A backup offers the same, and the line says what turning one off means.
  await form.getByLabel("Action").selectOption("backup");
  await form.getByLabel("Warn players in the game").uncheck();
  await expect(page.locator("#schedule-preview")).toContainText("then goes ahead without warning anyone still on.");
  await form.getByLabel("Warn players in the game").check();
  await form.getByLabel("Action").selectOption("restart");

  await form.getByRole("button", { name: "Add schedule" }).click();
  await expect(page).toHaveURL(/saved=created/);
  const row = page.locator("#schedule-list tbody tr", { hasText: "08:00, every day" });
  await expect(row).toContainText("Backup before restart");
  await expect(row).toContainText("Waits up to 30 min for an empty server, then goes ahead");
  await expect(row).toContainText("Warnings: 5, 1 min before");
  await expect(row.locator("td").nth(2)).toHaveText("Enabled");
  await row.getByRole("button", { name: "Delete" }).click();
  await page.getByRole("dialog", { name: "Delete this schedule?" }).getByRole("button", { name: "Delete schedule" }).click();
  await expect(row).toHaveCount(0);
});

test("a new schedule starts on an action its owner can use", async ({ page, session }) => {
  await createUser(page, "scheduler", "scheduler password one");
  await shareServer(page, "Survival", "scheduler", ["Restart", "Set up their own schedules"]);

  const operator = await session({ storageState: { cookies: [], origins: [] } });
  await signIn(operator, "scheduler", "scheduler password one");
  await operator.getByRole("link", { name: "Survival", exact: true }).click();
  await operator.getByRole("tab", { name: "Schedules" }).click();
  const form = operator.locator("#schedule-form");
  await expect(form.getByLabel("Action")).toHaveValue("restart");
  await expect(form.locator(".schedule-form-note").getByText("Connected players are disconnected.").filter({ visible: true })).toHaveCount(1);
  // Backing up first needs backup access.
  await expect(form.getByLabel("Back up first")).toHaveCount(0);
  await expect(form.getByText("Starts the server if it is stopped.")).toBeHidden();
  await expect(operator.locator("#schedule-preview")).toContainText("08:00");
  // Sending commands needs console access too.
  await expect(form.getByLabel("Action").locator("option", { hasText: "Send command" })).toHaveCount(0);

  await shareServer(page, "Survival", "scheduler", ["Restart", "Set up their own schedules", "Send console commands"]);
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

// The fake game consoles take fake-join and fake-leave, which change who is
// online; a command sent from the console page also starts a new reading.
async function consoleCommand(page, text) {
  await page.goto("/");
  await serverRow(page, "Survival").getByRole("link", { name: "Console" }).click();
  await expect(page.locator("[data-console-status]")).toHaveText("Connected");
  await page.getByLabel("Game command").fill(text);
  await page.getByRole("button", { name: "Send" }).click();
  await expect(page.locator("[data-console-feedback]")).toHaveText("Command sent. Check the output for its result.");
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
  await consoleCommand(console, "fake-join Mara");
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
    await consoleCommand(console, "fake-leave Mara");
  }
  // The restart goes ahead once the server is empty.
  await page.getByRole("tab", { name: "Schedules" }).click();
  await expect(page).toHaveURL(/tab=schedules/);
  const row = page.locator("#schedule-list tbody tr", { hasText: "Waits up to 30 min" }).last();
  await expectResult(page, row, "Succeeded", 75_000);
  await expect(row.locator("td").nth(2)).toHaveText("Completed");
});

test("a waiting restart is skipped when players stay online", async ({ page }) => {
  test.setTimeout(150_000);
  const console = await page.context().newPage();
  await consoleCommand(console, "fake-join Theo");
  try {
    const row = await addWaitingRestart(page, 1, "Skip this run");
    await expectWaiting(page, row);
    await expectResult(page, row, "Skipped: players were still online when the wait ended", 100_000);
    await expect(row.getByRole("link", { name: "View activity" })).toHaveCount(0);
  } finally {
    await consoleCommand(console, "fake-leave Theo");
  }
});

test("a waiting restart goes ahead anyway at the end of its wait", async ({ page }) => {
  test.setTimeout(150_000);
  const console = await page.context().newPage();
  await consoleCommand(console, "fake-join Ada");
  try {
    const row = await addWaitingRestart(page, 1, "Go ahead anyway");
    await expectWaiting(page, row);
    await expect(row.locator(".schedule-next-run")).toContainText("then going ahead anyway");
    await expectResult(page, row, "Succeeded", 100_000);
  } finally {
    await consoleCommand(console, "fake-leave Ada");
  }
});

// A member who manages schedules sees the administrator's schedule, and who
// set it up, but can only read it.
test("a member sees the owner's schedules read-only", async ({ page, session }) => {
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Schedules" }).click();
  const form = page.locator("#schedule-form");
  await form.getByLabel("Time", { exact: true }).fill("03:10");
  await form.getByRole("button", { name: "Add schedule" }).click();
  await expect(page).toHaveURL(/saved=created/);
  try {
    await createUser(page, "planner", "planner password one");
    await shareServer(page, "Survival", "planner", ["Restart", "Set up their own schedules"]);
    const member = await session({ storageState: { cookies: [], origins: [] } });
    await signIn(member, "planner", "planner password one");
    await member.getByRole("link", { name: "Survival", exact: true }).click();
    await expect(member.locator(".overview-facts")).not.toContainText("Nothing scheduled");
    await member.getByRole("tab", { name: "Schedules" }).click();
    const row = member.locator("#schedule-list tbody tr", { hasText: "03:10, every day" });
    await expect(row).toContainText("Set up by admin");
    await expect(row.getByRole("button", { name: "Edit" })).toBeDisabled();
    await expect(row.getByRole("button", { name: "Pause" })).toBeDisabled();
    await expect(row.getByRole("button", { name: "Delete" })).toBeDisabled();
  } finally {
    await page.goto("/servers");
    await openServer(page, "Survival");
    await page.getByRole("tab", { name: "Schedules" }).click();
    const row = page.locator("#schedule-list tbody tr", { hasText: "03:10, every day" });
    await row.getByRole("button", { name: "Delete" }).click();
    await page.getByRole("dialog", { name: "Delete this schedule?" }).getByRole("button", { name: "Delete schedule" }).click();
    await expect(row).toHaveCount(0);
  }
});
