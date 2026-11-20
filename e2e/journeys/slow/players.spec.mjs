import { test, expect, asAdmin, gameCommand, openServer } from "../fixtures.mjs";

asAdmin();
// Each journey warns the players on a server of its own.
test.describe.configure({ mode: "parallel" });

// With someone online, a restart offers to warn them first and is on by
// default; the game hears it, and the restart follows a minute later.
test("restarting with players online warns them first", async ({ page, ownServer }) => {
  test.setTimeout(150_000);
  const console = await page.context().newPage();
  await gameCommand(console, "fake-join Ada", ownServer.name);
  try {
    await openServer(page, ownServer.name);
    await expect(page.locator(".overview-players").getByRole("list", { name: "Online players" }).locator(".player-name"))
      .toHaveText(["Ada"], { timeout: 30_000 });
    await page.reload();
    await page.locator(".detail-actions").getByRole("button", { name: "Restart" }).click();
    const dialog = page.getByRole("dialog", { name: `Restart ${ownServer.name}?` });
    await expect(dialog.getByLabel("Warn players and wait 1 minute")).toBeChecked();
    await expect(dialog.getByLabel("Back up first")).not.toBeChecked();
    await dialog.getByRole("button", { name: "Restart server" }).click();
    await expect(page.locator("#selected-operation")).toContainText("Giving players 1 minute");
    await gameCommand(console, "fake-said", ownServer.name);
    await expect(console.locator(".xterm-rows")).toContainText("Server restart in 1 minute.");
    await expect(page.locator("#selected-operation")).toContainText("Succeeded", { timeout: 90_000 });
  } finally {
    await gameCommand(console, "fake-leave Ada", ownServer.name);
  }
});

// A backup started from a page that loaded while nobody was on still warns
// someone who joined since: who is online is read when the backup starts.
test("a backup warns a player who joined after the page loaded", async ({ page, ownServer }) => {
  test.setTimeout(120_000);
  await openServer(page, ownServer.name);
  await expect(page.locator(".overview-players")).toContainText(/0\s*of 10 online/, { timeout: 30_000 });
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page.locator("#create-backup")).toBeEnabled();
  const console = await page.context().newPage();
  await gameCommand(console, "fake-join Ada", ownServer.name);
  try {
    await page.getByRole("button", { name: "Create backup" }).click();
    const backup = page.getByRole("dialog", { name: `Back up ${ownServer.name}?` });
    await expect(backup.getByLabel("Warn players and wait 1 minute")).toBeChecked();
    await expect(backup).toContainText("If nobody is online when it’s time to warn, it goes ahead at once.");
    await backup.getByRole("button", { name: "Create backup" }).click();
    const details = page.locator("#selected-operation");
    await expect(details).toContainText("Giving players 1 minute", { timeout: 40_000 });
    await gameCommand(console, "fake-said", ownServer.name);
    await expect(console.locator(".xterm-rows")).toContainText("Server restart for a backup in 1 minute.");
    await details.getByRole("button", { name: "Cancel backup" }).click();
    await expect(page.locator("#detail-status")).toHaveText("Cancelled. Nothing was changed.");
  } finally {
    await gameCommand(console, "fake-leave Ada", ownServer.name);
  }
});

// A restart from a page that showed players online, asked to warn them,
// doesn't wait once they have all left, and Activity says why.
test("a restart doesn't wait for players who have left", async ({ page, ownServer }) => {
  test.setTimeout(120_000);
  const console = await page.context().newPage();
  await gameCommand(console, "fake-join Ada", ownServer.name);
  await openServer(page, ownServer.name);
  const players = page.locator(".overview-players");
  await expect(players.getByRole("list", { name: "Online players" }).locator(".player-name")).toHaveText(["Ada"], { timeout: 30_000 });
  await gameCommand(console, "fake-leave Ada", ownServer.name);
  await expect(players).toContainText(/0\s*of 10 online/, { timeout: 30_000 });
  await page.locator(".detail-actions").getByRole("button", { name: "Restart" }).click();
  const dialog = page.getByRole("dialog", { name: `Restart ${ownServer.name}?` });
  await expect(dialog.getByLabel("Warn players and wait 1 minute")).toBeChecked();
  await dialog.getByRole("button", { name: "Restart server" }).click();
  const details = page.locator("#selected-operation");
  // Well inside the minute a warning would take.
  await expect(details).toContainText("Succeeded", { timeout: 40_000 });
  await expect(details).toContainText("Nobody was online, so players weren’t warned.");
});
