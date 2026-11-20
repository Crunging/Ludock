import { test, expect, asAdmin, openServer, expectOperationSucceeded, fake, monitoringSettings } from "./fixtures.mjs";

asAdmin();
// Each journey here passes on its own, so they spread across workers.
test.describe.configure({ mode: "parallel" });

test("the overview leads with what a server is doing and who is on, with Docker's facts folded away", async ({ page }) => {
  await openServer(page, "Survival");
  const facts = page.locator(".overview-facts").first();
  await expect(facts.locator("dt")).toHaveText(["State", "Game", "Connect", "Last backup", "Next scheduled"]);
  // Who is on, and the lists that go with them, have a section of their own.
  const players = page.getByRole("region", { name: "Players" });
  await expect(players.locator(".overview-players")).toContainText(/of 10 online/);
  await expect(players.getByText("Allowlist and bans", { exact: true })).toBeVisible();
  await expect(page.getByRole("region", { name: "Resources" }).locator("dt")).toHaveText(["CPU", "Memory", "Disk"]);

  // Docker's facts wait behind Details, which a refresh leaves open.
  const details = page.locator("#overview-details");
  await expect(details.getByText("itzg/minecraft-server:java21")).toBeHidden();
  await details.getByText("Details", { exact: true }).click();
  await expect(details.getByText("itzg/minecraft-server:java21")).toBeVisible();
  await expect(details).toContainText("25565/tcp");
  await expect(details).toContainText("100% is one core.");
  await expect(details).toContainText("Health check");
  const refreshed = page.waitForResponse(/\/fragments\/servers\/[^/]+\/overview$/);
  await page.evaluate(() => document.body.dispatchEvent(new Event("availability-changed")));
  await refreshed;
  await expect(details.getByText("itzg/minecraft-server:java21")).toBeVisible();
  for (const docker of ["Ports", "Image", "Health check", "one core"]) await expect(facts).not.toContainText(docker);

  // Monitoring says how it is, keeps automatic restart in sight, and folds
  // its settings away while it's on.
  await expect(page.locator("#availability-title + p")).toBeVisible();
  await expect(page.getByLabel("Restart it if the game stops answering")).toBeVisible();
  await expect(page.getByLabel("Monitor this server")).toBeHidden();
});

test("on a phone, a server's players come right after its main facts", async ({ session }) => {
  const page = await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE, viewport: { width: 390, height: 844 } });
  await openServer(page, "Survival");
  const players = page.getByRole("heading", { level: 2, name: "Players" });
  // Five short facts above it, and nothing Docker-facing between.
  const above = await page.locator(".overview-facts").first().evaluate((list) => list.getBoundingClientRect().bottom);
  const heading = await players.evaluate((element) => element.getBoundingClientRect().top);
  expect(heading).toBeGreaterThan(above);
  expect(heading - above).toBeLessThan(80);
  await expect(page.locator("#overview-details")).not.toHaveAttribute("open");
  await expect(page.locator(".overview-players")).toContainText(/of 10 online/);
  // Nothing on the page scrolls sideways.
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);
});

test("tuning a server's monitoring and following a restart", async ({ page, ownServer }) => {
  await openServer(page, ownServer.name);
  // Watched without anyone turning it on; the settings wait behind a disclosure.
  await expect(page.locator("#availability-title + p")).toHaveText(/^(No outage detected|Paused briefly after a restart or stop|Checking it for the first time)\.$/);
  await expect(page.getByLabel("Wait before alerting (seconds)")).toBeHidden();
  const settings = await monitoringSettings(page);
  await expect(page.getByLabel("Monitor this server")).toBeChecked();
  await expect(settings.getByRole("link", { name: "Settings → Notifications" })).toHaveAttribute("href", "/settings#notification-settings-title");

  // Monitoring settings save as they change.
  await page.getByLabel("Wait before alerting (seconds)").fill("180");
  await page.getByLabel("Wait before alerting (seconds)").press("Tab");
  await expect(page.locator("#availability-result")).toHaveText("Monitoring settings saved.");
  await page.reload();
  await monitoringSettings(page);
  await expect(page.getByLabel("Wait before alerting (seconds)")).toHaveValue("180");

  // Turning monitoring off sticks, and leaves its settings open to turn it back on.
  await page.getByLabel("Monitor this server").uncheck();
  await expect(page.locator("#availability-result")).toHaveText("Monitoring settings saved.");
  await page.reload();
  await expect(page.locator("#monitoring-settings")).toHaveAttribute("open");
  await expect(page.getByLabel("Monitor this server")).not.toBeChecked();
  await expect(page.locator("#availability-title + p")).toHaveText("Monitoring is off.");
  await page.getByLabel("Monitor this server").check();
  await expect(page.locator("#availability-result")).toHaveText("Monitoring settings saved.");

  await page.locator(".detail-actions").getByRole("button", { name: "Restart" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Restart server" }).click();
  await expectOperationSucceeded(page);
  await expect(page.locator("#activity-list")).toContainText("Save command sent before stopping.");

  await page.getByLabel("Result").selectOption("succeeded");
  await expect(page.locator(".activity-filter-count")).toHaveText(/^Showing 1 of 1\.$/);
  await expect(page).toHaveURL(/status=succeeded/);
});

test("a restart rides out Docker restarting under it and says it waited", async ({ page }) => {
  await openServer(page, "Survival");
  await page.locator(".detail-actions").getByRole("button", { name: "Restart" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Restart server" }).click();
  const operation = page.locator("#selected-operation");
  await expect(operation).toContainText("Waiting for save");
  await fake("/fake/outage", { seconds: "3" });
  await expectOperationSucceeded(page);
  await expect(operation).toContainText("Waited for Docker to answer again.");
  await expect(page.locator(".detail-actions").getByRole("button", { name: "Stop" })).toBeEnabled();
});

test("reading a server's resource history over an hour and a day", async ({ page }) => {
  await openServer(page, "Survival");
  // Ludock sampled the running server as it started.
  const history = page.locator("#resource-history");
  await expect(history.getByRole("heading", { name: "History" })).toBeVisible();
  const cpu = history.getByRole("img", { name: "CPU over the last hour: average 25.0%, peak 25.0%." });
  const memory = history.getByRole("img", { name: "Memory over the last hour: average 1.5 GiB, peak 1.5 GiB." });
  await expect(cpu).toBeVisible();
  await expect(memory).toBeVisible();
  await expect(history.getByRole("link", { name: "Hour", exact: true })).toHaveAttribute("aria-current", "true");

  // Focusing a chart reads out the latest minute in both charts, and the
  // pointer does the same where it rests.
  await cpu.focus();
  await expect(history.locator(".resource-chart__readout").first()).toContainText("25.0%");
  await expect(history.locator(".resource-chart__readout").last()).toContainText("1.5 GiB");
  await cpu.blur();
  await expect(history.locator(".resource-chart__readout").first()).toBeHidden();
  const box = await memory.boundingBox();
  await page.mouse.move(box.x + box.width - 1, box.y + box.height / 2);
  await expect(history.locator(".resource-chart__readout").first()).toContainText("25.0%");

  // The range changes in place and stays in the address.
  await page.evaluate(() => { window.stayedOnPage = true; });
  await history.getByRole("link", { name: "Day", exact: true }).click();
  await expect(history.getByRole("img", { name: /^CPU over the last day: / })).toBeVisible();
  await expect(history.getByRole("link", { name: "Day", exact: true })).toHaveAttribute("aria-current", "true");
  await expect(page).toHaveURL(/\?range=day$/);
  expect(await page.evaluate(() => window.stayedOnPage)).toBe(true);

  // The overview's own refresh keeps the chosen range.
  const refreshed = page.waitForResponse(/\/fragments\/servers\/[^/]+\/overview$/);
  await page.evaluate(() => document.body.dispatchEvent(new Event("availability-changed")));
  await refreshed;
  await expect(history.getByRole("img", { name: /^CPU over the last day: / })).toBeVisible();

  await page.reload();
  await expect(history.getByRole("link", { name: "Day", exact: true })).toHaveAttribute("aria-current", "true");
});

test("switching a server's tabs keeps the page loaded", async ({ page }) => {
  await openServer(page, "Survival");
  // A full page load would drop this marker.
  await page.evaluate(() => { window.stayedOnPage = true; });
  const activity = page.getByRole("tab", { name: "Activity" });
  await activity.click();
  await expect(page.getByRole("heading", { name: "Recent activity" })).toBeVisible();
  await expect(page).toHaveURL(/\?tab=activity$/);
  await expect(activity).toHaveAttribute("aria-selected", "true");
  await expect(activity).toBeFocused();

  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page.locator("#create-backup")).toBeEnabled();
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
  await expect(page.getByRole("heading", { name: "Recent activity" })).toBeVisible();
  await page.goForward();
  await expect(page.locator("#create-backup")).toBeEnabled();

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
  await expect(page.getByRole("heading", { name: "Recent activity" })).toBeVisible();
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
  await expect(page.locator("#create-backup")).toBeEnabled();
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
  await expect(page.getByRole("heading", { name: "Recent activity" })).toBeVisible();
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

test("a filter answering after another tab opened leaves that tab alone", async ({ page }) => {
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Activity" }).click();
  await expect(page.getByRole("heading", { name: "Recent activity" })).toBeVisible();
  // htmx wires up a new view once it settles, a moment after showing it.
  await expect(page.locator("#detail-body")).not.toHaveClass(/htmx-added/);
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  await page.route(/\/fragments\/servers\/[^/]+\/activity\?/, async (route) => {
    await held;
    await route.continue();
  });
  const filtered = page.waitForRequest(/\/activity\?.*status=failed/);
  await page.getByLabel("Result").selectOption("failed");
  await filtered;
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page.locator("#backups-tab")).toBeVisible();
  const answered = page.waitForResponse(/\/activity\?.*status=failed/);
  release();
  await answered;
  await page.waitForTimeout(300);
  await expect(page).toHaveURL(/\?tab=backups$/);
  await page.unroute(/\/fragments\/servers\/[^/]+\/activity\?/);
});

test("clicking a tab again while its page loads still opens it", async ({ page }) => {
  await openServer(page, "Survival");
  // Hold every tab page until all three clicks are made.
  const gates = [];
  await page.route(/\?tab=(activity|backups)$/, async (route) => {
    const tab = new URL(route.request().url()).searchParams.get("tab");
    await new Promise((resolve) => gates.push({ tab, resolve }));
    await route.continue();
  });
  const activity = page.getByRole("tab", { name: "Activity" });
  await activity.click();
  await expect.poll(() => gates.length).toBe(1);
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect.poll(() => gates.length).toBe(2);
  await activity.click();
  // The third click has a request of its own; Backups answers first.
  const activities = () => gates.filter((gate) => gate.tab === "activity");
  await expect.poll(() => activities().length).toBe(2);
  const backups = page.waitForResponse(/\?tab=backups$/);
  gates.find((gate) => gate.tab === "backups").resolve();
  await backups;
  for (const gate of activities()) gate.resolve();
  await expect(page.getByRole("heading", { name: "Recent activity" })).toBeVisible();
  await expect(page.getByRole("tab", { name: "Activity" })).toHaveAttribute("aria-selected", "true");
  await expect(page).toHaveURL(/\?tab=activity$/);
  await page.unroute(/\?tab=(activity|backups)$/);
});

test("quick repeated clicks end on the last tab clicked", async ({ page }) => {
  await openServer(page, "Survival");
  // Hold every tab page until all four clicks are made.
  const gates = [];
  await page.route(/\?tab=(activity|backups)$/, async (route) => {
    await new Promise((resolve) => gates.push(resolve));
    await route.continue();
  });
  const activity = page.getByRole("tab", { name: "Activity" });
  const backups = page.getByRole("tab", { name: "Backups" });
  for (const [index, tab] of [activity, backups, activity, backups].entries()) {
    await tab.click();
    await expect.poll(() => gates.length).toBe(index + 1);
  }
  // The third answers before the fourth, then the first two.
  const third = page.waitForResponse(/\?tab=activity$/);
  gates[2]();
  await third;
  const fourth = page.waitForResponse(/\?tab=backups$/);
  gates[3]();
  await fourth;
  gates[0]();
  gates[1]();
  await expect(page.locator("#backups-tab")).toBeVisible();
  await expect(backups).toHaveAttribute("aria-selected", "true");
  await expect(page).toHaveURL(/\?tab=backups$/);
  await page.unroute(/\?tab=(activity|backups)$/);
});

test("tabs clicked faster than they open end on the last one", async ({ page }) => {
  await openServer(page, "Survival");
  // Nothing holds these pages back: clicks land while earlier tabs are opening.
  for (const gap of [0, 40]) {
    await page.evaluate(async (gap) => {
      for (const name of ["Activity", "Backups", "Schedules", "Update", "Activity", "Schedules", "Backups"]) {
        [...document.querySelectorAll(".section-tabs a")].find((tab) => tab.textContent.trim() === name).click();
        await new Promise((done) => setTimeout(done, gap));
      }
    }, gap);
    await expect(page.getByRole("tab", { name: "Backups" })).toHaveAttribute("aria-selected", "true");
    await expect(page.locator("#backups-tab")).toBeVisible();
    await expect(page).toHaveURL(/\?tab=backups$/);
    await page.getByRole("tab", { name: "Overview" }).click();
    await expect(page.locator("#overview-live")).toBeVisible();
  }
});

test("a tab clicked the moment the last one appears stays in the page", async ({ page }) => {
  await openServer(page, "Survival");
  await page.evaluate(() => {
    window.samePage = true;
    // The click lands as soon as the Activity tab's content is in the page,
    // before anything else gets a turn.
    let clicked = false;
    document.addEventListener("htmx:before:settle", (event) => {
      if (clicked || event.detail.task?.target?.id !== "detail-body") return;
      clicked = true;
      setTimeout(() => document.getElementById("tab-schedules").click(), 0);
    });
  });
  await page.getByRole("tab", { name: "Activity" }).click();
  await expect(page.getByRole("tab", { name: "Schedules" })).toHaveAttribute("aria-selected", "true");
  await expect(page).toHaveURL(/\?tab=schedules$/);
  expect(await page.evaluate(() => window.samePage)).toBe(true);
});
