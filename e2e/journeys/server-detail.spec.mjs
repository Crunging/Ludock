import { test, expect, asAdmin, openServer, expectOperationSucceeded } from "./fixtures.mjs";

asAdmin();

test("reviewing a server, setting up monitoring, and following a restart", async ({ page }) => {
  await openServer(page, "Survival");
  const facts = page.locator(".overview-facts");
  await expect(facts).toContainText("itzg/minecraft-server:java21");
  await expect(facts).toContainText("25565/tcp");
  await expect(page.locator("#availability-title + p")).toHaveText("Monitoring disabled.");

  // Monitoring settings save as they change.
  await page.getByLabel("Monitor this server").check();
  await expect(page.locator("#availability-result")).toHaveText("Monitoring settings saved.");
  await expect(page.locator("#availability-title + p")).not.toHaveText("Monitoring disabled.");
  await page.locator("#availability-result").evaluate((result) => result.replaceChildren());
  await page.getByLabel("Failure grace period (seconds)").fill("180");
  await page.getByLabel("Failure grace period (seconds)").press("Tab");
  await expect(page.locator("#availability-result")).toHaveText("Monitoring settings saved.");
  await page.reload();
  await expect(page.getByLabel("Failure grace period (seconds)")).toHaveValue("180");

  await page.locator(".detail-actions").getByRole("button", { name: "Restart" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Restart server" }).click();
  await expectOperationSucceeded(page);
  await expect(page.locator("#activity-list")).toContainText("Save command sent before stopping.");

  await page.getByLabel("Status").selectOption("failed");
  await expect(page.locator(".activity-filter-count")).toHaveText(/Showing 0 of \d+ recent operations\./);
  await expect(page).toHaveURL(/status=failed/);
});

test("switching a server's tabs keeps the page loaded", async ({ page }) => {
  await openServer(page, "Survival");
  // A full page load would drop this marker.
  await page.evaluate(() => { window.stayedOnPage = true; });
  const activity = page.getByRole("tab", { name: "Activity" });
  await activity.click();
  await expect(page.getByRole("heading", { name: "Recent operations" })).toBeVisible();
  await expect(page).toHaveURL(/\?tab=activity$/);
  await expect(activity).toHaveAttribute("aria-selected", "true");
  await expect(activity).toBeFocused();

  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page.locator("#backup-readiness")).toContainText("Ready to back up.");
  expect(await page.evaluate(() => window.stayedOnPage)).toBe(true);

  // A modified click opens the tab in a new browser tab and leaves this one.
  const [opened] = await Promise.all([page.context().waitForEvent("page"),
    page.getByRole("tab", { name: "Schedules" }).click({ modifiers: ["ControlOrMeta"] })]);
  await expect(opened).toHaveURL(/\?tab=schedules$/);
  await opened.close();
  await expect(page).toHaveURL(/\?tab=backups$/);

  // Back and forward still land on the right tab.
  await page.goBack();
  await expect(page).toHaveURL(/\?tab=activity$/);
  await expect(page.getByRole("heading", { name: "Recent operations" })).toBeVisible();
  await page.goForward();
  await expect(page.locator("#backup-readiness")).toContainText("Ready to back up.");

  // Quick clicks: the last tab wins even when the one before it answers
  // first. Both requests are held until both are sent.
  const holds = {};
  const sent = (name) => new Promise((resolve) => { holds[name] = { resolve }; });
  const sentBoth = Promise.all([sent("schedules"), sent("activity")]);
  const gates = {};
  await page.route(/\?tab=(schedules|activity)$/, async (route) => {
    const name = new URL(route.request().url()).searchParams.get("tab");
    const gate = new Promise((resolve) => { gates[name] = resolve; });
    holds[name].resolve();
    await gate;
    await route.continue();
  });
  await page.getByRole("tab", { name: "Schedules" }).click();
  await page.getByRole("tab", { name: "Activity" }).click();
  await sentBoth;
  const schedules = page.waitForResponse(/\?tab=schedules$/);
  gates.schedules();
  await schedules;
  const activityAnswer = page.waitForResponse(/\?tab=activity$/);
  gates.activity();
  await activityAnswer;
  await expect(page.getByRole("heading", { name: "Recent operations" })).toBeVisible();
  await expect(page.getByRole("tab", { name: "Activity" })).toHaveAttribute("aria-selected", "true");
  await expect(page).toHaveURL(/\?tab=activity$/);
  await page.unroute(/\?tab=(schedules|activity)$/);
});

test("a later tab wins over following earlier work", async ({ page }) => {
  await openServer(page, "Survival");
  // Hold the page that would follow the restart until Backups has opened.
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  await page.route(/\?tab=activity&operation=/, async (route) => {
    await held;
    await route.continue();
  });
  const follow = page.waitForRequest(/\?tab=activity&operation=/);
  await page.locator(".detail-actions").getByRole("button", { name: "Restart" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Restart server" }).click();
  await follow;
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page).toHaveURL(/\?tab=backups$/);
  await expect(page.getByRole("tab", { name: "Backups" })).toHaveAttribute("aria-selected", "true");
  const followed = page.waitForResponse(/\?tab=activity&operation=/);
  release();
  await followed;
  await page.waitForTimeout(300);
  await expect(page).toHaveURL(/\?tab=backups$/);
  await expect(page.getByRole("tab", { name: "Backups" })).toHaveAttribute("aria-selected", "true");
  await page.unroute(/\?tab=activity&operation=/);
  // Let the restart finish before the next journey.
  await expect(page.locator(".detail-actions").getByRole("button", { name: "Restart" })).toBeEnabled({ timeout: 45_000 });
});

test("a tab opened while an action is sending stays open", async ({ page }) => {
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page.locator("#backup-readiness")).toContainText("Ready to back up.");
  // Hold the backup request until the user has moved to Activity.
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  await page.route(/\/backups$/, async (route) => {
    if (route.request().method() !== "POST") return route.continue();
    await held;
    await route.continue();
  });
  const sent = page.waitForRequest((request) => request.method() === "POST" && /\/backups$/.test(request.url()));
  await page.getByRole("button", { name: "Create backup" }).click();
  await page.getByRole("dialog", { name: "Back up Survival?" }).getByRole("button", { name: "Create backup" }).click();
  await sent;
  await page.getByRole("tab", { name: "Activity" }).click();
  await expect(page.getByRole("heading", { name: "Recent operations" })).toBeVisible();
  const answered = page.waitForResponse((response) => response.request().method() === "POST" && /\/backups$/.test(response.url()));
  release();
  await answered;
  await page.waitForTimeout(500);
  await expect(page).toHaveURL(/\?tab=activity$/);
  await expect(page.getByRole("tab", { name: "Activity" })).toHaveAttribute("aria-selected", "true");
  await page.unroute(/\/backups$/);
  // The backup still runs; let it finish before the next journey.
  await expect(page.locator(".detail-actions").getByRole("button", { name: "Restart" })).toBeEnabled({ timeout: 45_000 });
});
