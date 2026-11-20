import { test, expect, asAdmin, serverRow, openServer, gameCommand, expectOperationSucceeded, fake } from "./fixtures.mjs";

asAdmin();

test("a server on a supported game image appears by itself, with its game and address", async ({ page }) => {
  await page.goto("/");
  // The fake daemon's Enshrouded container has no Ludock labels, and also
  // publishes the older game port 15636, which a first-port guess would pick.
  const row = serverRow(page, "enshrouded");
  await expect(row.locator(".server-row__game")).toHaveText("Enshrouded");
  await expect(row.locator(".server-row__address")).toContainText(":15637");
});

test("Valheim's files and backups leave out the server its image downloads", async ({ page }) => {
  // lloesche/valheim-server's README also mounts the 4 GB server download at
  // /opt/valheim, which every backup copied.
  await openServer(page, "valheim");
  const disks = page.locator(".overview-disks > li");
  await expect(disks).toHaveCount(1);
  await expect(disks).toContainText("/data/valheim");
});

/** Adds one of the fake daemon's real-image games for a journey, and takes it away after. */
async function withGame(page, game, run) {
  await fake("/fake/game", { game, name: game });
  try {
    // Ludock finds it from Docker's events, shortly after.
    await expect(async () => {
      await page.goto("/");
      await expect(serverRow(page, game)).toBeVisible({ timeout: 1000 });
    }).toPass();
    await run();
  } finally {
    await openServer(page, game);
    await page.getByRole("button", { name: "Remove from Ludock" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Remove server" }).click();
    await expect(page).toHaveURL(/\/$/);
    await fake("/fake/remove", { container: game });
  }
}

test("Terraria quits with its own exit command, so it stops saved and a backup succeeds", async ({ page }) => {
  // Every Terraria image runs the game where Docker's stop signal never
  // reaches it, so a stop alone waited two minutes, killed it, and failed
  // the backup.
  await withGame(page, "terraria", async () => {
    await page.goto("/");
    await expect(serverRow(page, "terraria").locator(".server-row__game")).toHaveText("Terraria");
    await expect(serverRow(page, "terraria").locator(".server-row__address")).toContainText(":7777");
    await openServer(page, "terraria");
    await page.getByRole("tab", { name: "Backups" }).click();
    await page.locator("#create-backup:not([disabled])").click();
    await page.getByRole("dialog").getByRole("button", { name: "Create backup" }).click();
    await expectOperationSucceeded(page);
    const operation = page.locator("#selected-operation");
    await expect(operation).toContainText("Save command sent before stopping.");
    await expect(operation).not.toContainText("Force-stopped");
    await page.goto("/");
    await serverRow(page, "terraria").getByRole("link", { name: "Console" }).click();
    await page.getByRole("navigation", { name: "Console mode" }).getByRole("link", { name: "Logs", exact: true }).click();
    await expect(page.locator("#console-output")).toContainText("Saving before exit...");
  });
});

test("Factorio's console reads the password its image generates, lists who is online, and warns players in chat", async ({ page }) => {
  // The image passes its generated RCON password on the command line and
  // keeps it only in config/rconpw.
  await withGame(page, "factorio", async () => {
    await page.goto("/");
    await expect(serverRow(page, "factorio").locator(".server-row__address")).toContainText(":34197");
    await openServer(page, "factorio");
    await expect(page.locator(".overview-players")).toContainText(/0\s*online/, { timeout: 45_000 });
    const console = await page.context().newPage();
    await gameCommand(console, "fake-join Ada", "factorio");
    await openServer(page, "factorio");
    await expect(page.locator(".overview-players .player-name")).toHaveText(["Ada"], { timeout: 30_000 });

    await page.locator(".detail-actions").getByRole("button", { name: "Stop", exact: true }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByLabel("Warn players and wait 1 minute")).toBeChecked();
    await dialog.getByRole("button", { name: "Stop server" }).click();
    await expect(page.locator("#selected-operation")).toContainText("Giving players 1 minute");
    await page.getByRole("button", { name: "Cancel stop" }).first().click();
    await expect(page.locator("#selected-operation")).toContainText("Cancelled");
    await gameCommand(console, "fake-said", "factorio");
    await expect(console.locator(".xterm-rows")).toContainText("Said: Server shutting down in 1 minute. | Server shutdown cancelled.");
  });
});
