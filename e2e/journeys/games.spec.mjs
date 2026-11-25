import { test, expect, asAdmin, serverRow, openServer, gameCommand, expectOperationSucceeded, restoreServer, withGame } from "./fixtures.mjs";

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

test("taking Valheim down says who is on, though the game can't be warned", async ({ page }) => {
  // Valheim's status check counts two players; it has no console to message them.
  await openServer(page, "valheim");
  try {
    await page.locator(".detail-actions").getByRole("button", { name: "Start" }).click();
    await expect(page.locator("#selected-operation")).toContainText("Succeeded");
    await page.locator(".section-tabs").getByRole("link", { name: "Overview" }).click();
    await expect(page.locator(".overview-players")).toContainText(/2\s*of 10 online/, { timeout: 45_000 });
    // The header's dialog text follows its own refresh.
    const online = "2 players are online and will be disconnected.";
    await expect(async () => {
      await page.locator(".detail-actions").getByRole("button", { name: "Stop" }).click();
      const dialog = page.getByRole("dialog", { name: "Stop valheim?" });
      try {
        await expect(dialog).toContainText(online, { timeout: 1000 });
      } finally {
        await dialog.getByRole("button", { name: "Cancel" }).click();
      }
    }).toPass({ timeout: 30_000 });
    await page.locator(".detail-actions").getByRole("button", { name: "Restart" }).click();
    await expect(page.getByRole("dialog", { name: "Restart valheim?" })).toContainText(online);
    await page.getByRole("dialog").getByRole("button", { name: "Cancel" }).click();

    await page.locator(".section-tabs").getByRole("link", { name: "Backups" }).click();
    await page.locator("#create-backup:not([disabled])").click();
    const backup = page.getByRole("dialog", { name: "Back up valheim?" });
    // The Backups tab doesn't follow the player count, so it gives no number.
    await expect(backup).toContainText("The server stops for the whole copy, then returns to how it was. Connected players will be disconnected.");
    await expect(backup.getByRole("checkbox")).toHaveCount(0);
    await backup.getByRole("button", { name: "Cancel" }).click();
  } finally {
    await restoreServer(page, "valheim");
  }
});

test("Terraria quits with its own exit command, so it stops saved and a backup succeeds", async ({ page }) => {
  // Every Terraria image runs the game where Docker's stop signal never
  // reaches it, so a stop alone waited two minutes, killed it, and failed
  // the backup.
  await withGame(page, "terraria", async (terraria) => {
    await page.goto("/");
    await expect(serverRow(page, terraria).locator(".server-row__game")).toHaveText("Terraria");
    await expect(serverRow(page, terraria).locator(".server-row__address")).toContainText(":7777");
    await openServer(page, terraria);
    await page.locator(".section-tabs").getByRole("link", { name: "Backups" }).click();
    await page.locator("#create-backup:not([disabled])").click();
    await page.getByRole("dialog").getByRole("button", { name: "Create backup" }).click();
    await expectOperationSucceeded(page);
    const operation = page.locator("#selected-operation");
    await expect(operation).toContainText("Save command sent before stopping.");
    await expect(operation).not.toContainText("Force-stopped");
    await page.goto("/");
    await serverRow(page, terraria).getByRole("link", { name: "Console" }).click();
    await page.getByRole("navigation", { name: "Console mode" }).getByRole("link", { name: "Logs", exact: true }).click();
    await expect(page.locator("#console-output")).toContainText("Saving before exit...");
  });
});

test("Terraria lists who is online from the replies in its log, and kicks and bans them", async ({ page }) => {
  // Terraria answers playing in its own output, not to the command.
  await withGame(page, "terraria", async (terraria) => {
    await openServer(page, terraria);
    const players = page.locator(".overview-players");
    await expect(players).toContainText(/0\s*online/, { timeout: 45_000 });
    const console = await page.context().newPage();
    await gameCommand(console, "fake-join Ada", terraria);
    await gameCommand(console, "fake-join Zed", terraria);
    await openServer(page, terraria);
    const names = page.locator(".overview-players .player-name");
    await expect(names).toHaveText(["Ada", "Zed"], { timeout: 30_000 });
    // Rows give each player's address, which never shows.
    await expect(players).not.toContainText("172.18.0.1");

    await page.getByRole("button", { name: "Zed: actions" }).click();
    await page.getByRole("button", { name: "Kick", exact: true }).click();
    await expect(page.locator("#detail-status")).toContainText("Asked the game to kick Zed.");
    await expect(names).toHaveText(["Ada"], { timeout: 15_000 });
    await page.getByRole("button", { name: "Ada: actions" }).click();
    await page.getByRole("button", { name: "Ban…" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Ban player" }).click();
    await expect(page.locator("#detail-status")).toContainText("Asked the game to ban Ada.");
    await expect(players).toContainText(/0\s*online/, { timeout: 15_000 });
  });
});

test("Factorio's console reads the password its image generates, lists who is online, and warns players in chat", async ({ page }) => {
  // The image passes its generated RCON password on the command line and
  // keeps it only in config/rconpw.
  await withGame(page, "factorio", async (factorio) => {
    await page.goto("/");
    await expect(serverRow(page, factorio).locator(".server-row__address")).toContainText(":34197");
    await openServer(page, factorio);
    await expect(page.locator(".overview-players")).toContainText(/0\s*online/, { timeout: 45_000 });
    const console = await page.context().newPage();
    await gameCommand(console, "fake-join Ada", factorio);
    await openServer(page, factorio);
    await expect(page.locator(".overview-players .player-name")).toHaveText(["Ada"], { timeout: 30_000 });

    await page.locator(".detail-actions").getByRole("button", { name: "Stop", exact: true }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByLabel("Warn players and wait 1 minute")).toBeChecked();
    await dialog.getByRole("button", { name: "Stop server" }).click();
    await expect(page.locator("#selected-operation")).toContainText("Giving players 1 minute");
    await page.getByRole("button", { name: "Cancel stop" }).first().click();
    await expect(page.locator("#selected-operation")).toContainText("Cancelled");
    await gameCommand(console, "fake-said", factorio);
    await expect(console.locator(".xterm-rows")).toContainText("Said: Server shutting down in 1 minute. | Server shutdown cancelled.");
  });
});
