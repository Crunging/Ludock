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
