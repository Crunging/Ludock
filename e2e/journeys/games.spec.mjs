import { test, expect, asAdmin, serverRow } from "./fixtures.mjs";

asAdmin();

test("a server on a supported game image appears by itself, with its game and address", async ({ page }) => {
  await page.goto("/");
  // The fake daemon's Enshrouded container has no Ludock labels; of its two
  // published ports, players connect on the game's.
  const row = serverRow(page, "enshrouded");
  await expect(row.locator(".server-row__game")).toHaveText("Enshrouded");
  await expect(row.locator(".server-row__address")).toContainText(":15637");
});
