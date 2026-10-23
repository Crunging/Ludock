import { test, expect, asAdmin, serverRow } from "./fixtures.mjs";

asAdmin();

test("finding servers and starting and stopping one from the list", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator(".server-row")).toHaveCount(2);
  await expect(serverRow(page, "Survival")).toContainText("Running");
  await expect(serverRow(page, "Survival")).toContainText("25565");

  await page.getByPlaceholder("Find a server…").fill("valh");
  await expect(page.locator(".server-row")).toHaveCount(1);
  await page.getByPlaceholder("Find a server…").fill("");
  await expect(page.locator(".server-row")).toHaveCount(2);

  const valheim = serverRow(page, "valheim");
  await valheim.getByRole("button", { name: "Start" }).click();
  await expect(valheim).toContainText("Running");

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
