import { test, expect, asAdmin, serverRow, openServer } from "./fixtures.mjs";

asAdmin();

// The fake Minecraft console takes fake-join, fake-leave, and fake-outage,
// which change what its list command answers.
async function command(page, text) {
  await page.goto("/");
  await serverRow(page, "Survival").getByRole("link", { name: "Console" }).click();
  await expect(page.locator("[data-console-status]")).toHaveText("Connected");
  await page.getByLabel("Game command").fill(text);
  await page.getByRole("button", { name: "Send" }).click();
  await expect(page.locator("[data-console-feedback]")).toHaveText("Command sent. Check the output for its result.");
}

test("seeing who is online, as players join and leave", async ({ page }) => {
  // Readings run in the background every 30 seconds, and right after a
  // console command.
  await openServer(page, "Survival");
  const players = page.locator(".overview-players");
  await expect(players).toContainText(/0\s*of 10 online/, { timeout: 30_000 });
  await expect(players.locator(".player-slot")).toHaveCount(10);
  await expect(players.locator(".player-slot--taken")).toHaveCount(0);

  const console = await page.context().newPage();
  await command(console, "fake-join Mara");
  await command(console, "fake-join Theo");
  await expect(players).toContainText(/2\s*of 10 online/);
  await expect(players.getByRole("list", { name: "Online players" }).getByRole("listitem")).toHaveText(["Mara", "Theo"]);
  await expect(players.locator(".player-slot--taken")).toHaveCount(2);

  // The list shows the count and names; servers that can't list players
  // show nothing to read.
  await page.goto("/");
  const survival = serverRow(page, "Survival").locator(".server-row__players");
  await expect(survival).toContainText("2/10");
  await expect(survival).toContainText("Mara, Theo");
  await expect(serverRow(page, "valheim").locator(".server-row__players")).toContainText("—Not available");

  // The open list updates as soon as someone leaves.
  await command(console, "fake-leave Theo");
  await expect(survival).toContainText("1/10");
  await expect(survival).not.toContainText("Theo");
  await command(console, "fake-leave Mara");
  await expect(survival).toContainText("0/10");

  // A console that stops answering is reported, not shown as empty.
  await command(console, "fake-outage 20");
  await openServer(page, "Survival");
  // The page can render just before the failed reading, and its live
  // events start a moment later; its next refresh, within 10 seconds,
  // shows the reading.
  await expect(players).toContainText("Couldn’t read players", { timeout: 15_000 });
  await expect(players).toContainText("Check that RCON is on");
  await page.goto("/");
  await expect(survival).toContainText("Unavailable");

  await openServer(page, "valheim");
  await expect(page.locator(".overview-players")).toContainText("Not available. Ludock lists players for Minecraft servers with RCON.");
});
