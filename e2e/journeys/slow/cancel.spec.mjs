import { test, expect, asAdmin, gameCommand, openServer, expectOperationSucceeded } from "../fixtures.mjs";

asAdmin();
// Each journey restarts or backs up servers of its own.
test.describe.configure({ mode: "parallel" });

/** Starts a restart that warns the players online, from the server's page. */
async function warnedRestart(page, server) {
  await openServer(page, server);
  await expect(page.locator(".overview-players").getByRole("list", { name: "Online players" }).locator(".player-name"))
    .toHaveText(["Ada"], { timeout: 30_000 });
  await page.reload();
  await page.locator(".detail-actions").getByRole("button", { name: "Restart" }).click();
  const dialog = page.getByRole("dialog", { name: `Restart ${server}?` });
  await expect(dialog.getByLabel("Warn players and wait 1 minute")).toBeChecked();
  await expect(dialog.getByLabel("Back up first")).not.toBeChecked();
  await dialog.getByRole("button", { name: "Restart server" }).click();
  await expect(page.locator("#selected-operation")).toContainText("Giving players 1 minute");
}

// Two warned restarts count down side by side, both followed live on
// Activity. One is cancelled there: its players hear it's off, the server
// keeps running, and its page, left open, says who cancelled it. The other
// goes ahead once its minute is up: Cancel goes away and the restart
// finishes, as Activity and the details say.
test("a warned restart counts down on Activity, and is cancelled there or goes ahead after its minute", async ({ page, ownServer, otherServer }) => {
  test.setTimeout(150_000);
  const console = await page.context().newPage();
  await gameCommand(console, "fake-join Ada", ownServer.name);
  await gameCommand(console, "fake-join Ada", otherServer.name);
  const other = await page.context().newPage();
  await warnedRestart(page, ownServer.name);
  await warnedRestart(other, otherServer.name);

  const activity = await page.context().newPage();
  await activity.goto("/activity");
  const restart = (server) => activity.locator("#activity-list tbody tr", { hasText: server }).filter({ hasText: "Restart" }).first();
  await expect(restart(ownServer.name)).toContainText("Giving players 1 minute");
  const cancelled = restart(otherServer.name);
  await cancelled.getByRole("button", { name: "Cancel restart" }).click();
  await expect(activity.locator("#activity-status")).toHaveText("Cancelled. Nothing was changed.");
  await expect(cancelled).toContainText("Cancelled by admin before it changed anything.");
  await expect(cancelled.getByRole("button", { name: "Cancel restart" })).toHaveCount(0);
  await expect(other.locator("#selected-operation")).toContainText("Cancelled by admin before it changed anything.");
  await expect(other.locator(".detail-header__identity")).toContainText("Running");
  await gameCommand(console, "fake-said", otherServer.name);
  await expect(console.locator(".xterm-rows")).toContainText("Said: Server restart in 1 minute. | Server restart cancelled.");

  const cancel = page.locator("#selected-operation").getByRole("button", { name: "Cancel restart" });
  await expect(cancel).toBeVisible();
  await expect(cancel).toHaveCount(0, { timeout: 90_000 });
  const details = page.locator("#selected-operation");
  await expect(details).toContainText("Succeeded", { timeout: 45_000 });
  await expect(details).not.toContainText("Cancelled");
  await expect(details).not.toContainText("Progress");
  const ahead = restart(ownServer.name);
  await expect(ahead).toContainText("Succeeded", { timeout: 10_000 });
  await expect(ahead.getByRole("button", { name: "Cancel restart" })).toHaveCount(0);
  await activity.close();
  await other.close();
});

// A backup or restore stops the server like a restart, so with someone
// online it offers the same warning, on by default, and can be cancelled
// during the minute like one.
test("a backup and a restore warn the players online first", async ({ page, ownServer }) => {
  test.setTimeout(120_000);
  const server = ownServer.name;
  const console = await page.context().newPage();
  await gameCommand(console, "fake-join Ada", server);
  await openServer(page, server);
  await expect(page.locator(".overview-players").getByRole("list", { name: "Online players" }).locator(".player-name"))
    .toHaveText(["Ada"], { timeout: 30_000 });
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page.locator("#create-backup")).toBeEnabled();
  await page.getByRole("button", { name: "Create backup" }).click();
  const backup = page.getByRole("dialog", { name: `Back up ${server}?` });
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
  await openServer(page, server);
  await expect(page.locator(".overview-players").getByRole("list", { name: "Online players" }).locator(".player-name"))
    .toHaveText(["Ada"], { timeout: 30_000 });
  await page.getByRole("tab", { name: "Backups" }).click();
  await page.locator("#backup-list tbody tr").first().getByRole("link", { name: "Restore…" }).click();
  const restore = page.locator("form.danger-panel");
  await expect(restore.getByLabel("Warn players and wait 1 minute")).toBeChecked();
  await restore.getByLabel(`Type ${server} to confirm`).fill(server);
  await restore.getByRole("button", { name: "Restore game data" }).click();
  await expect(details).toContainText("Giving players 1 minute");
  await details.getByRole("button", { name: "Cancel restore" }).click();
  await expect(page.locator("#detail-status")).toHaveText("Cancelled. Nothing was changed.");

  await gameCommand(console, "fake-said", server);
  await expect(console.locator(".xterm-rows")).toContainText("Server restart for a backup in 1 minute. | Server restart for a backup cancelled.");
  await expect(console.locator(".xterm-rows")).toContainText(
    "Server restart to restore a backup in 1 minute. | Server restart to restore a backup cancelled.");
});
