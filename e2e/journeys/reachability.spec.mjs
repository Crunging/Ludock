import { test, expect, asAdmin, dockerAction, serverRow, openServer, restoreServer } from "./fixtures.mjs";

asAdmin();

test("counting players with the game's status query when it has no console", async ({ page }) => {
  await openServer(page, "palworld");
  await expect(page.locator(".overview-game")).toContainText("Not checked");

  await dockerAction("valheim", "start");
  try {
    await openServer(page, "valheim");
    const players = page.locator(".overview-players");
    await expect(players).toContainText(/2\s*of 10 online/, { timeout: 45_000 });
    await expect(players).toContainText("Counted by the game’s status query");
    await expect(players.locator(".player-slot--taken")).toHaveCount(2);
    await expect(page.locator(".overview-game")).toContainText("Answering");

    await page.goto("/");
    await expect(serverRow(page, "valheim").locator(".server-row__players")).toContainText("2/10");
  } finally {
    await restoreServer(page, "valheim");
  }
  await openServer(page, "valheim");
  await expect(page.locator(".overview-game")).toContainText("Checked while the server is running");
});
