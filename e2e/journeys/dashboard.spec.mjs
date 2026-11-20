import { test, expect, asAdmin, dockerAction, openServer, serverRow } from "./fixtures.mjs";

asAdmin();

/** Reloads the list as a live update does; the page has no Refresh button. */
async function refreshList(page) {
  await expect(page.getByRole("button", { name: "Refresh" })).toHaveCount(0);
  await page.locator("#server-filters").evaluate((form) => form.requestSubmit());
}

test("finding servers and starting and stopping one from the list", async ({ page }) => {
  // Pages are compressed; the server list repeats its markup for each server.
  const response = await page.goto("/");
  expect(response.headers()["content-encoding"]).toBe("gzip");
  await expect(page.locator(".server-row")).toHaveCount(4);
  await expect(serverRow(page, "Survival")).toContainText("Running");
  await expect(serverRow(page, "Survival")).toContainText("25565");
  // The strip under the title counts every server, whatever the filters.
  const fleet = page.locator("#fleet");
  await expect(fleet).toHaveText("1 running, 3 stopped");

  // Typing keeps going while results arrive: the keyboard types into
  // whatever has focus, so a replaced search box would lose the "e".
  const search = page.getByPlaceholder("Find a server…");
  await search.click();
  await page.keyboard.type("valh");
  await expect(page.locator(".server-row")).toHaveCount(1);
  await expect(page.locator("#server-count")).toHaveText("1 of 4 servers");
  await expect(fleet).toHaveText("1 running, 3 stopped");
  await page.keyboard.type("e");
  await expect(search).toHaveValue("valhe");
  await expect(search).toBeFocused();
  await expect(page).toHaveURL(/\/\?search=valhe$/);

  // Live refreshes after an action keep the filter.
  const valheim = serverRow(page, "valheim");
  await valheim.getByRole("button", { name: "Start" }).click();
  await expect(valheim).toContainText("Running");
  await expect(fleet).toHaveText("2 running, 2 stopped");
  await expect(page.locator(".server-row")).toHaveCount(1);
  await expect(search).toHaveValue("valhe");

  // The filter survives a reload, and clearing it shows everything again.
  await page.reload();
  await expect(page.locator(".server-row")).toHaveCount(1);
  // Refreshes replace the state choices; choosing one afterwards still filters.
  await search.fill("");
  await expect(page.locator(".server-row")).toHaveCount(4);
  await page.getByLabel("State").selectOption("running");
  await expect(page).toHaveURL(/\/\?state=running$/);
  await expect(page.locator(".server-row")).toHaveCount(2);
  await expect(page.getByLabel("State")).toHaveValue("running");
  await search.fill("no such server");
  await expect(page.locator(".server-list__empty")).toContainText("No servers match your filters.");
  await page.getByRole("button", { name: "Clear filters" }).click();
  await expect(page.locator(".server-row")).toHaveCount(4);
  await expect(search).toHaveValue("");
  await expect(search).toBeFocused();
  await expect(page).toHaveURL(/\/$/);

  await valheim.getByRole("button", { name: "Stop" }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("Stop valheim?");
  await dialog.getByRole("button", { name: "Stop server" }).click();
  await expect(valheim).toContainText("Stopped", { timeout: 30_000 });
});

test("a stop answered after a refresh still reaches the page", async ({ page }) => {
  await page.goto("/");
  // The stop is refused as unauthenticated after the list has refreshed. The
  // page's own handler sends a bare 401 to sign-in, which needs the button
  // that sent the stop to still be in the page.
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  await page.route(/\/servers\/[^/]+\/stop$/, async (route) => {
    await held;
    await route.fulfill({ status: 401, body: "" });
  });
  await serverRow(page, "Survival").getByRole("button", { name: "Stop" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Stop server" }).click();
  const refreshed = page.waitForResponse(/\/fragments\/servers/);
  await refreshList(page);
  await refreshed;
  const signIn = page.waitForRequest(/\/login$/);
  release();
  await signIn;
});

test("cancelling a restart from the More menu keeps the keyboard in place", async ({ page }) => {
  await page.goto("/");
  const survival = serverRow(page, "Survival");
  const more = survival.getByRole("button", { name: /More actions/ });
  await more.click();
  await survival.getByRole("button", { name: "Restart…" }).click();
  await expect(page.getByRole("dialog", { name: "Restart Survival?" })).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog")).toBeHidden();
  await expect(more).toBeFocused();
});

test("the More menu on the last server opens past the end of the list", async ({ page }) => {
  await page.goto("/");
  await page.getByPlaceholder("Find a server…").fill("valh");
  await expect(page.locator(".server-row")).toHaveCount(1);

  await serverRow(page, "valheim").getByRole("button", { name: /More actions/ }).click();
  const files = serverRow(page, "valheim").getByRole("link", { name: "Files" });
  // The list must not clip the menu: the browser hits the item near its bottom edge.
  await files.scrollIntoViewIfNeeded();
  await expect.poll(() => files.evaluate((link) => {
    const box = link.getBoundingClientRect();
    return link.contains(document.elementFromPoint(box.x + box.width / 2, box.bottom - 4));
  })).toBe(true);
  await files.click();
  await expect(page).toHaveURL(/\/files\//);
});

test("list refreshes show local times from the first frame", async ({ page }) => {
  await page.goto("/?search=valh");
  await expect(page.locator(".server-row")).toHaveCount(1);
  // Records every time label the list shows while a refresh adds a server:
  // a refresh morphs the list, so rows already there don't change.
  const shown = await page.evaluate(() => new Promise((resolve) => {
    const servers = document.getElementById("servers");
    const texts = [];
    const record = () => servers.querySelectorAll("time").forEach((time) => texts.push(time.textContent));
    const observer = new MutationObserver(record);
    observer.observe(servers, { childList: true, subtree: true, characterData: true });
    document.body.addEventListener("htmx:after:settle", function settled(event) {
      if (event.target !== servers) return;
      document.body.removeEventListener("htmx:after:settle", settled);
      // The swap settles in the task that changed the list, before the
      // observer is told.
      if (observer.takeRecords().length) record();
      observer.disconnect();
      resolve(texts);
    });
    const search = document.querySelector("#server-filters input[type=search]");
    search.value = "";
    search.form.requestSubmit();
  }));
  expect(shown.length).toBeGreaterThan(0);
  expect(shown.filter((text) => text.includes("UTC"))).toEqual([]);
});

test("on a phone, the menu holds the navigation and servers stay compact", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/");
  const menu = page.getByRole("button", { name: "Menu" });
  const settings = page.getByRole("link", { name: "Settings" });
  await expect(settings).toBeHidden();
  await menu.click();
  await expect(menu).toHaveAttribute("aria-expanded", "true");
  await expect(settings).toBeInViewport();
  await page.keyboard.press("Escape");
  await expect(settings).toBeHidden();
  await expect(menu).toBeFocused();

  // A server's name, state, address, last backup, and actions fit in a
  // short card, so several fit on one screen.
  for (const row of await page.locator(".server-row").all()) {
    expect((await row.boundingBox()).height).toBeLessThan(220);
  }

  await menu.click();
  await settings.click();
  await expect(page).toHaveURL(/\/settings$/);
});

test("copying a server's address", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.goto("/");
  const copy = serverRow(page, "Survival").getByRole("button", { name: "Copy address for Survival" });
  await copy.click();
  await expect(copy).toHaveText("✓");
  expect(await page.evaluate(() => navigator.clipboard.readText())).toMatch(/:25565$/);
  // The icon comes back, ready for another copy.
  await expect(copy.locator("svg")).toBeVisible();
});

test("a state chosen while the list is loading still applies", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator(".server-row").first()).toBeVisible();
  // Holds the next list response, so the choice lands while it loads.
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  let waiting = false;
  await page.route("**/fragments/servers?*", async (route) => {
    if (!waiting) {
      waiting = true;
      await held;
    }
    await route.continue();
  });
  await refreshList(page);
  await expect.poll(() => waiting).toBe(true);
  await page.getByLabel("State").selectOption("running");
  release();
  await expect(page).toHaveURL(/\/\?state=running$/);
  await expect(page.getByLabel("State")).toHaveValue("running");
});

test("filters changed while the list is loading all apply", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator(".server-row").first()).toBeVisible();
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  let waiting = false;
  await page.route("**/fragments/servers?*", async (route) => {
    if (!waiting) {
      waiting = true;
      await held;
    }
    await route.continue();
  });
  await refreshList(page);
  await expect.poll(() => waiting).toBe(true);
  // A search, then a state chosen and set back, while the refresh loads.
  await page.getByPlaceholder("Find a server…").fill("valh");
  await page.waitForTimeout(400);
  await page.getByLabel("State").selectOption("running");
  await page.getByLabel("State").selectOption("all");
  release();
  await expect(page.locator("#server-count")).toHaveText("1 of 4 servers");
  await expect(page).toHaveURL(/\/\?search=valh$/);
});

test("an outage reaches an open server list when its grace period ends, and clears", async ({ page }) => {
  test.setTimeout(150_000);
  await openServer(page, "valheim");
  // On by default, unless a journey before this one turned it off.
  await page.getByLabel("Monitor this server").check();
  await page.getByText("Alert levels").click();
  await page.getByLabel("Wait before alerting (seconds)").fill("10");
  await page.getByLabel("Wait before alerting (seconds)").press("Tab");
  await expect(page.locator("#availability-result")).toHaveText("Monitoring settings saved.");
  // Starting through Ludock ends any earlier stop's quiet time.
  await page.locator(".detail-actions").getByRole("button", { name: "Start" }).click();
  await expect(page.locator(".detail-actions").getByRole("button", { name: "Stop" })).toBeVisible();

  await page.goto("/");
  const attention = page.locator("#attention");
  await expect(attention).not.toContainText("Server down");
  await dockerAction("valheim", "stop");
  await expect(serverRow(page, "valheim")).toContainText("Stopped");
  await expect(attention).toContainText("Server down", { timeout: 60_000 });
  // The list and its summary call it down, as the alert does.
  await expect(serverRow(page, "valheim").locator(".server-row__state")).toContainText("Down");
  await expect(page.locator("#fleet")).toContainText("1 down");

  await dockerAction("valheim", "start");
  await expect(attention).not.toContainText("Server down", { timeout: 30_000 });

  await dockerAction("valheim", "stop");
  await openServer(page, "valheim");
  await expect(page.locator(".detail-header__identity")).toContainText("Down", { timeout: 60_000 });
  await page.getByLabel("Monitor this server").uncheck();
  await expect(page.locator("#availability-title + p")).toHaveText("Monitoring is off.");
});
