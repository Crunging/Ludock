import { test, expect, asAdmin, createUser, openServer, signIn, shareServer } from "./fixtures.mjs";

asAdmin();
// Each journey schedules on a server of its own.
test.describe.configure({ mode: "parallel" });

/** Opens the schedule form's More options once the page has filled in the
 * browser's time zone; typing earlier would add to it. */
async function moreOptions(form) {
  await expect(form.locator("input[name=timezone]")).not.toHaveValue("");
  const more = form.locator("#schedule-more");
  if (!(await more.evaluate((details) => details.open))) await more.locator("summary").click();
}

test("scheduling, editing, pausing, and deleting a restart", async ({ page, session, ownServer }) => {
  await openServer(page, ownServer.name);
  await page.locator(".section-tabs").getByRole("link", { name: "Schedules" }).click();
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
  // Other journeys leave their own schedules on this server.
  const row = page.locator("#schedule-list tbody tr", { hasText: "weekdays" });
  await expect(row).toContainText("05:30, weekdays");
  await expect(row).toContainText("Warns players 5 and 1 minutes before");

  // Editing keeps its warnings, said in one line.
  await row.getByRole("link", { name: "Edit" }).click();
  await expect(page.locator("#schedule-preview")).toContainText("Anyone still on is warned in the game 5 and 1 minutes before it goes ahead.");
  await expect(page.locator("#schedule-form").getByLabel("Warn players in the game")).toBeChecked();
  await page.locator("#schedule-form").getByLabel("Time", { exact: true }).fill("06:45");
  await page.locator("#schedule-form").getByRole("button", { name: "Save changes" }).click();
  await expect(page).toHaveURL(/saved=saved/);
  await expect(row).toContainText("06:45, weekdays");

  // The notice comes with the refreshed list, not ahead of a slower read of it.
  await page.route(/\/fragments\/servers\/[^/]+\/schedules(\?|$)/, async (route) => {
    await new Promise((resolve) => setTimeout(resolve, 1500));
    await route.continue();
  });
  await row.getByRole("button", { name: "Pause" }).click();
  await expect(page.locator("#detail-status")).toHaveText("Schedule paused.");
  expect(await row.locator("td").nth(2).textContent()).toBe("Paused");
  await page.unrouteAll();
  await row.getByRole("button", { name: "Resume" }).click();
  await expect(row.locator("td").nth(2)).toHaveText("Enabled");

  await page.locator(".section-tabs").getByRole("link", { name: "Overview" }).click();
  await expect(page.locator(".overview-facts").first()).toContainText("Restart");

  // Someone else pauses the schedule while it is being edited: saving shows
  // the conflict and keeps the draft, which then saves without resuming it.
  await page.locator(".section-tabs").getByRole("link", { name: "Schedules" }).click();
  await row.getByRole("link", { name: "Edit" }).click();
  const other = await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
  await openServer(other, ownServer.name);
  await other.locator(".section-tabs").getByRole("link", { name: "Schedules" }).click();
  await other.locator("#schedule-list tbody tr", { hasText: "weekdays" }).getByRole("button", { name: "Pause" }).click();
  await expect(other.locator("#detail-status")).toHaveText("Schedule paused.");

  const editor = page.locator("#schedule-form");
  await editor.getByLabel("Time", { exact: true }).fill("07:15");
  await editor.getByRole("button", { name: "Save changes" }).click();
  await expect(editor.locator(".schedule-conflict")).toContainText("This schedule changed elsewhere. Your draft is still here");
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
  await expect(row).toHaveCount(0);
});

// The common case needs no choices: a daily restart at 08:00 in the
// browser's time zone that backs up first, waits for players to leave, and
// warns anyone still on. The rest waits under More options.
test("a new schedule opens on a daily restart that backs up first", async ({ page, ownServer }) => {
  await openServer(page, ownServer.name);
  await page.locator(".section-tabs").getByRole("link", { name: "Schedules" }).click();
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
  // The summary follows the wait being turned off and on.
  await form.getByLabel("Wait until no one is online").uncheck();
  await expect(page.locator("#schedule-preview")).toContainText("Players: Players are warned in the game 5 and 1 minutes before.");
  await form.getByLabel("Wait until no one is online").check();
  await expect(page.locator("#schedule-preview")).toContainText("Players: Waits up to 30 minutes for everyone to leave.");
  // A backup offers the same, and the line says what turning one off means.
  await form.getByLabel("Action").selectOption("backup");
  await form.getByLabel("Warn players in the game").uncheck();
  await expect(page.locator("#schedule-preview")).toContainText("then goes ahead without warning anyone still on.");
  await form.getByLabel("Warn players in the game").check();
  await form.getByRole("button", { name: "Add schedule" }).click();
  await expect(page).toHaveURL(/saved=created/);

  // A restart at the same time as that backup is pointed at Back up first,
  // and can still be saved beside it.
  const preview = page.locator("#schedule-preview");
  await form.getByRole("button", { name: "Weekends" }).click();
  await expect(preview).toContainText("Same time: Sometimes runs at the same time as “Backup, 08:00, every day”. One waits for the other to finish");
  await form.getByRole("button", { name: "Every day" }).click();
  await expect(preview).toContainText(`Same time: Runs at the same time as “Backup, 08:00, every day”. The restart already backs up first`);
  await form.getByLabel("Back up first").uncheck();
  await expect(preview).toContainText("To back up and restart with one stop, use Back up first on the restart instead.");
  await form.getByLabel("Back up first").check();
  await form.getByRole("button", { name: "Add schedule" }).click();
  await expect(page).toHaveURL(/saved=created/);
  const row = page.locator("#schedule-list tbody tr", { hasText: "Backup before restart" });
  await expect(row).toContainText("08:00, every day");
  await expect(row).toContainText("Waits up to 30 minutes for everyone to leave, then goes ahead");
  await expect(row).toContainText("Warns players 5 and 1 minutes before");
  await expect(row).toContainText("Runs at the same time as “Backup, 08:00, every day”. The restart already backs up first");
  await expect(row.locator("td").nth(2)).toHaveText("Enabled");
  const backup = page.locator("#schedule-list tbody tr", { hasText: "08:00, every day" }).filter({ hasNotText: "Backup before restart" });
  await expect(backup).toContainText("Runs at the same time as “Restart, 08:00, every day”.");
  for (const each of [row, backup]) {
    await each.getByRole("button", { name: "Delete" }).click();
    await page.getByRole("dialog", { name: "Delete this schedule?" }).getByRole("button", { name: "Delete schedule" }).click();
    await expect(each).toHaveCount(0);
  }
});

test("a new schedule starts on an action its owner can use", async ({ page, session, ownServer }) => {
  const scheduler = "scheduler-" + ownServer.container;
  await createUser(page, scheduler, "scheduler password one");
  await shareServer(page, ownServer.name, scheduler, ["Restart", "Set up their own schedules"]);

  const operator = await session({ storageState: { cookies: [], origins: [] } });
  await signIn(operator, scheduler, "scheduler password one");
  await operator.getByRole("link", { name: ownServer.name, exact: true }).click();
  await operator.locator(".section-tabs").getByRole("link", { name: "Schedules" }).click();
  const form = operator.locator("#schedule-form");
  await expect(form.getByLabel("Action")).toHaveValue("restart");
  await expect(form.locator(".schedule-form-note").getByText("Connected players are disconnected.").filter({ visible: true })).toHaveCount(1);
  // Backing up first needs backup access.
  await expect(form.getByLabel("Back up first")).toHaveCount(0);
  await expect(form.getByText("Starts the server if it is stopped.")).toBeHidden();
  await expect(operator.locator("#schedule-preview")).toContainText("08:00");
  // Sending commands needs console access too.
  await expect(form.getByLabel("Action").locator("option", { hasText: "Send command" })).toHaveCount(0);

  await shareServer(page, ownServer.name, scheduler, ["Restart", "Set up their own schedules", "Send console commands"]);
  await operator.reload();
  await operator.locator("#schedule-form").getByLabel("Action").selectOption("command");
  await expect(operator.locator("#schedule-form").getByLabel("Console command")).toBeVisible();
});

// A member who manages schedules sees the administrator's schedule, and who
// set it up, but can only read it.
test("a member sees the owner's schedules read-only", async ({ page, session, ownServer }) => {
  const planner = "planner-" + ownServer.container;
  await openServer(page, ownServer.name);
  await page.locator(".section-tabs").getByRole("link", { name: "Schedules" }).click();
  const form = page.locator("#schedule-form");
  await form.getByLabel("Time", { exact: true }).fill("03:10");
  await form.getByRole("button", { name: "Add schedule" }).click();
  await expect(page).toHaveURL(/saved=created/);
  try {
    await createUser(page, planner, "planner password one");
    await shareServer(page, ownServer.name, planner, ["Restart", "Set up their own schedules"]);
    const member = await session({ storageState: { cookies: [], origins: [] } });
    await signIn(member, planner, "planner password one");
    await member.getByRole("link", { name: ownServer.name, exact: true }).click();
    await expect(member.locator(".overview-facts").first()).not.toContainText("Nothing scheduled");
    await member.locator(".section-tabs").getByRole("link", { name: "Schedules" }).click();
    const row = member.locator("#schedule-list tbody tr", { hasText: "03:10, every day" });
    await expect(row).toContainText("Set up by admin");
    // Usernames show as typed: no style capitalizes them.
    expect(await row.locator(".schedule-owner").innerText()).toBe("Set up by admin");
    // Controls they can't use give way to who can.
    await expect(row).toContainText("Only admin or an administrator can change this.");
    await expect(row.getByRole("button")).toHaveCount(0);
    await expect(row.getByRole("link", { name: "Edit" })).toHaveCount(0);
  } finally {
    await page.goto("/servers");
    await openServer(page, ownServer.name);
    await page.locator(".section-tabs").getByRole("link", { name: "Schedules" }).click();
    const row = page.locator("#schedule-list tbody tr", { hasText: "03:10, every day" });
    await row.getByRole("button", { name: "Delete" }).click();
    await page.getByRole("dialog", { name: "Delete this schedule?" }).getByRole("button", { name: "Delete schedule" }).click();
    await expect(row).toHaveCount(0);
  }
});
