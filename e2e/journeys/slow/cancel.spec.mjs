import { test, expect, asAdmin, gameCommand, openServer, createUser, shareServer, signIn, expectOperationSucceeded } from "../fixtures.mjs";

asAdmin();
test.describe.configure({ mode: "parallel" });

/** Starts a restart that warns the players online, from the server's page. */
async function warnedRestart(page) {
  await openServer(page, "Survival");
  await expect(page.locator(".overview-players").getByRole("list", { name: "Online players" }).locator(".player-name"))
    .toHaveText(["Ada"], { timeout: 30_000 });
  await page.reload();
  await page.locator(".detail-actions").getByRole("button", { name: "Restart" }).click();
  const dialog = page.getByRole("dialog", { name: "Restart Survival?" });
  await expect(dialog.getByLabel("Warn players and wait 1 minute")).toBeChecked();
  await dialog.getByRole("button", { name: "Restart server" }).click();
  await expect(page.locator("#selected-operation")).toContainText("Giving players 1 minute");
}

test.describe("cancelling a warned restart", () => {
  let console;
  test.beforeEach(async ({ page }) => {
    console = await page.context().newPage();
    await gameCommand(console, "fake-join Ada");
  });
  test.afterEach(async () => {
    await gameCommand(console, "fake-leave Ada");
  });

  // Once the minute is up and the restart begins, Cancel goes away: the
  // restart finishes.
  test("goes away once the restart begins", async ({ page }) => {
    test.setTimeout(150_000);
    await warnedRestart(page);
    const cancel = page.locator("#selected-operation").getByRole("button", { name: "Cancel restart" });
    await expect(cancel).toBeVisible();
    await expect(cancel).toHaveCount(0, { timeout: 90_000 });
    await expect(page.locator("#selected-operation")).toContainText("Succeeded", { timeout: 45_000 });
    await expect(page.locator("#selected-operation")).not.toContainText("Cancelled");
  });

  // Someone who sees the warning go out on the wrong server cancels it: the
  // players hear it's off, the server keeps running, and Activity says who
  // cancelled it.
  test("tells the players and leaves the server running", async ({ page }) => {
    await warnedRestart(page);
    await page.locator("#selected-operation").getByRole("button", { name: "Cancel restart" }).click();
    await expect(page.locator("#detail-status")).toHaveText("Cancelled. Nothing was changed.");
    const details = page.locator("#selected-operation");
    await expect(details).toContainText("Cancelled by admin before it changed anything.");
    await expect(details.getByRole("button", { name: "Cancel restart" })).toHaveCount(0);
    await expect(page.locator(".detail-header__identity")).toContainText("Running");

    await page.getByRole("tab", { name: "Activity" }).click();
    const row = page.locator("#activity-list tbody tr").first();
    await expect(row).toContainText("Restart");
    await expect(row).toContainText("Cancelled by admin before it changed anything.");

    await gameCommand(console, "fake-said");
    await expect(console.locator(".xterm-rows")).toContainText("Server restart in 1 minute. | Server restart cancelled.");
  });

  // Only someone who may restart the server sees Cancel; a member who can
  // only see it doesn't, and one who can run it cancels it.
  test("is offered to whoever may restart the server", async ({ page, session }) => {
    test.setTimeout(150_000);
    await createUser(page, "cancel-viewer", "viewer password 12345");
    await createUser(page, "cancel-runner", "runner password 12345");
    await shareServer(page, "Survival", "cancel-viewer", "Can see it");
    await shareServer(page, "Survival", "cancel-runner", "Can run it");
    await warnedRestart(page);

    const viewer = await session({ storageState: { cookies: [], origins: [] } });
    await signIn(viewer, "cancel-viewer", "viewer password 12345");
    await openServer(viewer, "Survival");
    await expect(viewer.locator(".detail-operation-notice")).toContainText("Giving players 1 minute");
    await expect(viewer.getByRole("button", { name: "Cancel restart" })).toHaveCount(0);

    const runner = await session({ storageState: { cookies: [], origins: [] } });
    await signIn(runner, "cancel-runner", "runner password 12345");
    await openServer(runner, "Survival");
    await runner.locator(".detail-operation-notice").getByRole("button", { name: "Cancel restart" }).click();
    await expect(runner.locator("#detail-status")).toHaveText("Cancelled. Nothing was changed.");
    await expect(runner.locator(".detail-operation-notice")).toHaveCount(0);
    await expect(page.locator("#selected-operation")).toContainText("Cancelled by cancel-runner before it changed anything.");
  });
});

// A backup or restore stops the server like a restart, so with someone
// online it offers the same warning, on by default, and can be cancelled
// during the minute like one.
test("a backup and a restore warn the players online first", async ({ page }) => {
  test.setTimeout(120_000);
  const console = await page.context().newPage();
  await gameCommand(console, "fake-join Ada");
  try {
    await openServer(page, "Survival");
    await expect(page.locator(".overview-players").getByRole("list", { name: "Online players" }).locator(".player-name"))
      .toHaveText(["Ada"], { timeout: 30_000 });
    await page.getByRole("tab", { name: "Backups" }).click();
    await expect(page.locator("#create-backup")).toBeEnabled();
    await page.getByRole("button", { name: "Create backup" }).click();
    const backup = page.getByRole("dialog", { name: "Back up Survival?" });
    await expect(backup).toContainText("Connected players will be disconnected.");
    await expect(backup.getByLabel("Warn players and wait 1 minute")).toBeChecked();
    await backup.getByRole("button", { name: "Create backup" }).click();
    const details = page.locator("#selected-operation");
    await expect(details).toContainText("Giving players 1 minute");
    await details.getByRole("button", { name: "Cancel backup" }).click();
    await expect(page.locator("#detail-status")).toHaveText("Cancelled. Nothing was changed.");
    await expect(page.locator(".detail-header__identity")).toContainText("Running");

    // Without the warning, the backup goes ahead at once.
    await page.getByRole("link", { name: "Dismiss" }).click();
    await expect(page.locator("#create-backup")).toBeEnabled();
    await page.getByRole("button", { name: "Create backup" }).click();
    await backup.getByLabel("Warn players and wait 1 minute").uncheck();
    await backup.getByRole("button", { name: "Create backup" }).click();
    await expectOperationSucceeded(page);

    // Once the server is back up, it reads who is online again.
    await openServer(page, "Survival");
    await expect(page.locator(".overview-players").getByRole("list", { name: "Online players" }).locator(".player-name"))
      .toHaveText(["Ada"], { timeout: 30_000 });
    await page.getByRole("tab", { name: "Backups" }).click();
    await page.locator("#backup-list tbody tr").first().getByRole("link", { name: "Restore…" }).click();
    const restore = page.locator("form.danger-panel");
    await expect(restore.getByLabel("Warn players and wait 1 minute")).toBeChecked();
    await restore.getByLabel(/Type Survival to confirm/).fill("Survival");
    await restore.getByRole("button", { name: "Restore game data" }).click();
    await expect(details).toContainText("Giving players 1 minute");
    await details.getByRole("button", { name: "Cancel restore" }).click();
    await expect(page.locator("#detail-status")).toHaveText("Cancelled. Nothing was changed.");

    await gameCommand(console, "fake-said");
    await expect(console.locator(".xterm-rows")).toContainText("Server restart for a backup in 1 minute. | Server restart for a backup cancelled.");
    await expect(console.locator(".xterm-rows")).toContainText(
      "Server restart to restore a backup in 1 minute. | Server restart to restore a backup cancelled.");
  } finally {
    await gameCommand(console, "fake-leave Ada");
  }
});
