import { test, expect, asAdmin, gameCommand, openServer } from "../fixtures.mjs";

asAdmin();

// With someone online, a restart offers to warn them first and is on by
// default; the game hears it, and the restart follows a minute later.
test("restarting with players online warns them first", async ({ page }) => {
  test.setTimeout(150_000);
  const console = await page.context().newPage();
  await gameCommand(console, "fake-join Ada");
  try {
    await openServer(page, "Survival");
    await expect(page.locator(".overview-players").getByRole("list", { name: "Online players" }).locator(".player-name"))
      .toHaveText(["Ada"], { timeout: 30_000 });
    await page.reload();
    await page.locator(".detail-actions").getByRole("button", { name: "Restart" }).click();
    const dialog = page.getByRole("dialog", { name: "Restart Survival?" });
    await expect(dialog.getByLabel("Warn players and wait 1 minute")).toBeChecked();
    await expect(dialog.getByLabel("Take a backup first")).not.toBeChecked();
    await dialog.getByRole("button", { name: "Restart server" }).click();
    await expect(page.locator("#selected-operation")).toContainText("Giving players 1 minute");
    await gameCommand(console, "fake-said");
    await expect(console.locator(".xterm-rows")).toContainText("Server restart in 1 minute.");
    await expect(page.locator("#selected-operation")).toContainText("Succeeded", { timeout: 90_000 });
  } finally {
    await gameCommand(console, "fake-leave Ada");
  }
});
