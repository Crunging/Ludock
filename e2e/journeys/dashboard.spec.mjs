import { test, expect, asAdmin, serverRow } from "./fixtures.mjs";

asAdmin();

test("finding servers and starting and stopping one from the list", async ({ page }) => {
  await page.goto("/");
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
  await search.fill("");
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
