import { test, expect, asAdmin, serverRow } from "./fixtures.mjs";

asAdmin();

test("finding servers and starting and stopping one from the list", async ({ page }) => {
  // Pages are compressed; the server list repeats its markup for each server.
  const response = await page.goto("/");
  expect(response.headers()["content-encoding"]).toBe("gzip");
  await expect(page.locator(".server-row")).toHaveCount(2);
  await expect(serverRow(page, "Survival")).toContainText("Running");
  await expect(serverRow(page, "Survival")).toContainText("25565");

  // Typing keeps going while results arrive: the keyboard types into
  // whatever has focus, so a replaced search box would lose the "e".
  const search = page.getByPlaceholder("Find a server…");
  await search.click();
  await page.keyboard.type("valh");
  await expect(page.locator(".server-row")).toHaveCount(1);
  await expect(page.locator("#server-count")).toHaveText("1 of 2 servers");
  await page.keyboard.type("e");
  await expect(search).toHaveValue("valhe");
  await expect(search).toBeFocused();
  await expect(page).toHaveURL(/\/\?search=valhe$/);

  // Live refreshes after an action keep the filter.
  const valheim = serverRow(page, "valheim");
  await valheim.getByRole("button", { name: "Start" }).click();
  await expect(valheim).toContainText("Running");
  await expect(page.locator(".server-row")).toHaveCount(1);
  await expect(search).toHaveValue("valhe");

  // The filter survives a reload, and clearing it shows everything again.
  await page.reload();
  await expect(page.locator(".server-row")).toHaveCount(1);
  // Refreshes replace the state choices; choosing one afterwards still filters.
  await search.fill("");
  await expect(page.locator(".server-row")).toHaveCount(2);
  await page.getByLabel("State").selectOption("running");
  await expect(page).toHaveURL(/\/\?state=running$/);
  await expect(page.locator(".server-row")).toHaveCount(2);
  await expect(page.getByLabel("State")).toHaveValue("running");
  await search.fill("no such server");
  await expect(page.locator(".server-list__empty")).toContainText("No servers match your filters.");
  await page.getByRole("button", { name: "Clear filters" }).click();
  await expect(page.locator(".server-row")).toHaveCount(2);
  await expect(search).toHaveValue("");
  await expect(search).toBeFocused();
  await expect(page).toHaveURL(/\/$/);

  await valheim.getByRole("button", { name: "Stop" }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("Stop valheim?");
  await dialog.getByRole("button", { name: "Stop server" }).click();
  await expect(valheim).toContainText("Stopped", { timeout: 30_000 });
});

test("the More menu on the last server opens past the end of the list", async ({ page }) => {
  await page.goto("/");
  await page.getByPlaceholder("Find a server…").fill("valh");
  await expect(page.locator(".server-row")).toHaveCount(1);

  await serverRow(page, "valheim").getByRole("button", { name: /More actions/ }).click();
  const files = serverRow(page, "valheim").getByRole("link", { name: "Files" });
  // The list must not clip the menu: the browser hits the item near its bottom edge.
  await expect.poll(() => files.evaluate((link) => {
    const box = link.getBoundingClientRect();
    return link.contains(document.elementFromPoint(box.x + box.width / 2, box.bottom - 4));
  })).toBe(true);
  await files.click();
  await expect(page).toHaveURL(/\/files\//);
});

test("list refreshes show local times from the first frame", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator(".server-row time").first()).toBeVisible();
  // Records every time label the list shows while a refresh swaps it in.
  const shown = await page.evaluate(() => new Promise((resolve) => {
    const servers = document.getElementById("servers");
    const texts = [];
    const observer = new MutationObserver(() => servers.querySelectorAll("time").forEach((time) => texts.push(time.textContent)));
    observer.observe(servers, { childList: true, subtree: true, characterData: true });
    document.body.addEventListener("htmx:afterSettle", function settled(event) {
      if (event.detail.target !== servers) return;
      document.body.removeEventListener("htmx:afterSettle", settled);
      observer.disconnect();
      resolve(texts);
    });
    window.htmx.trigger(document.body, "servers-changed");
  }));
  expect(shown.length).toBeGreaterThan(0);
  expect(shown.filter((text) => text.includes("UTC"))).toEqual([]);
});

test("on a phone, the menu holds the navigation and servers stay compact", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/");
  const menu = page.getByRole("button", { name: "Menu" });
  const diagnostics = page.getByRole("link", { name: "Diagnostics" });
  await expect(diagnostics).toBeHidden();
  await menu.click();
  await expect(menu).toHaveAttribute("aria-expanded", "true");
  await expect(diagnostics).toBeInViewport();
  await page.keyboard.press("Escape");
  await expect(diagnostics).toBeHidden();
  await expect(menu).toBeFocused();

  // A server's name, state, address, last backup, and actions fit in a
  // short card, so several fit on one screen.
  for (const row of await page.locator(".server-row").all()) {
    expect((await row.boundingBox()).height).toBeLessThan(220);
  }

  await menu.click();
  await diagnostics.click();
  await expect(page).toHaveURL(/\/diagnostics$/);
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
  await page.getByRole("button", { name: "Refresh" }).click();
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
  await page.getByRole("button", { name: "Refresh" }).click();
  await expect.poll(() => waiting).toBe(true);
  // A search, then a state chosen and set back, while the refresh loads.
  await page.getByPlaceholder("Find a server…").fill("valh");
  await page.waitForTimeout(400);
  await page.getByLabel("State").selectOption("running");
  await page.getByLabel("State").selectOption("all");
  release();
  await expect(page.locator("#server-count")).toHaveText("1 of 2 servers");
  await expect(page).toHaveURL(/\/\?search=valh$/);
});
