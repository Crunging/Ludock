import { test, expect, asAdmin, dockerAction, gameCommand as command, serverRow, openServer, createUser, signIn, shareServer } from "./fixtures.mjs";

asAdmin();

// onlineNames are the names in a server's list of who is online.
function onlineNames(page) {
  return page.locator(".overview-players").getByRole("list", { name: "Online players" }).locator(".player-name");
}

test("kicking a player and seeing them leave", async ({ page }) => {
  const console = await page.context().newPage();
  await command(console, "fake-join Mara");
  await openServer(page, "Survival");
  await expect(onlineNames(page)).toHaveText(["Mara"], { timeout: 30_000 });

  // The name opens what can be done to the player.
  await page.getByRole("button", { name: "Mara: actions" }).click();
  await page.getByRole("button", { name: "Kick", exact: true }).click();
  await expect(page.locator("#detail-status")).toHaveText("Mara was kicked.");
  await expect(onlineNames(page)).toHaveCount(0);
  await expect(page.locator(".overview-players")).toContainText(/0\s*of 10 online/);

  // Activity says what was done, once.
  await page.goto("/activity?show=everything&action=kicked");
  await expect(page.locator(".audit-entry").filter({ hasText: "Kicked Mara" })).toHaveCount(1);
  await expect(page.locator(".audit-entry").filter({ hasText: "Kicked Mara" })).toContainText("Succeeded");
});

test("banning a player, then lifting the ban", async ({ page }) => {
  const console = await page.context().newPage();
  await command(console, "fake-join Theo");
  await openServer(page, "Survival");
  await expect(onlineNames(page)).toHaveText(["Theo"], { timeout: 30_000 });

  // A ban lasts, so it asks first; cancelling sends nothing.
  await page.getByRole("button", { name: "Theo: actions" }).click();
  await page.getByRole("button", { name: "Ban…" }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("Ban Theo?");
  await expect(dialog).toContainText("You can lift it under Allowlist and bans.");
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(onlineNames(page)).toHaveText(["Theo"]);
  await page.getByRole("button", { name: "Theo: actions" }).click();
  await page.getByRole("button", { name: "Ban…" }).click();
  await dialog.getByRole("button", { name: "Ban player" }).click();
  await expect(page.locator("#detail-status")).toHaveText("Theo is banned.");
  await expect(onlineNames(page)).toHaveCount(0);

  // The ban list shows them until the ban is lifted.
  await page.getByText("Allowlist and bans", { exact: true }).click();
  const banned = page.locator(".player-list", { has: page.getByRole("heading", { name: "Banned" }) });
  await expect(banned.locator(".player-name")).toHaveText(["Theo"]);
  await banned.getByRole("button", { name: "Lift ban: Theo" }).click();
  await expect(page.locator(".player-lists")).toContainText("Theo’s ban is lifted.");
  await expect(banned).toContainText("Nobody is banned.");
  await command(console, "fake-join Theo");
  await openServer(page, "Survival");
  await expect(onlineNames(page)).toHaveText(["Theo"], { timeout: 15_000 });
  await command(console, "fake-leave Theo");

  // Searching Activity for one player action finds only that action.
  await page.goto("/activity?show=everything&action=banned");
  await expect(page.locator(".audit-entry strong").getByText("Banned Theo", { exact: true })).toHaveCount(1);
  await page.goto("/activity?show=everything&action=kicked");
  await expect(page.locator(".audit-entry").filter({ hasText: "Theo" })).toHaveCount(0);
});

test("adding a player to the allowlist, then removing them", async ({ page }) => {
  await openServer(page, "Survival");
  await page.getByText("Allowlist and bans", { exact: true }).click();
  const lists = page.locator(".player-lists");
  const allowed = lists.locator(".player-list", { has: page.getByRole("heading", { name: "Allowlist" }) });
  // Whether the allowlist is on is the server's own setting, shown as it is.
  await expect(allowed).toContainText("Off in the server’s settings, so anyone can join.");
  await expect(allowed).toContainText("Nobody is on the allowlist.");

  await allowed.getByLabel("Player name").fill("Juno_2");
  await allowed.getByRole("button", { name: "Add to allowlist" }).click();
  await expect(lists).toContainText("Juno_2 is on the allowlist.");
  await expect(allowed.locator(".player-name")).toHaveText(["Juno_2"]);
  await allowed.getByLabel("Player name").fill("Juno_2");
  await allowed.getByRole("button", { name: "Add to allowlist" }).click();
  await expect(lists).toContainText("Juno_2 was already on the allowlist.");

  // The browser keeps a name the game can't take from being sent.
  await allowed.getByLabel("Player name").fill("@a");
  await allowed.getByRole("button", { name: "Add to allowlist" }).click();
  expect(await allowed.getByLabel("Player name").evaluate((input) => input.validity.patternMismatch)).toBe(true);

  await allowed.getByRole("button", { name: "Remove: Juno_2" }).click();
  await expect(lists).toContainText("Juno_2 is off the allowlist.");
  await expect(allowed).toContainText("Nobody is on the allowlist.");
});

test("someone without console access isn't offered player actions", async ({ page, session }) => {
  await createUser(page, "watcher1", "watcher password one");
  // Starting and stopping, and reading logs, without the console.
  await shareServer(page, "Survival", "watcher1", ["See the server", "Start", "Stop", "Read logs"]);
  const console = await page.context().newPage();
  await command(console, "fake-join Mara");

  // A fresh profile, not the administrator this file runs as.
  const watcher = await session({ storageState: { cookies: [], origins: [] } });
  await signIn(watcher, "watcher1", "watcher password one");
  await openServer(watcher, "Survival");
  await expect(onlineNames(watcher)).toHaveText(["Mara"], { timeout: 30_000 });
  await expect(watcher.getByRole("button", { name: "Mara: actions" })).toHaveCount(0);
  await expect(watcher.getByText("Allowlist and bans", { exact: true })).toHaveCount(0);
  const csrf = await watcher.locator('meta[name="csrf-token"]').getAttribute("content");
  const kick = await watcher.request.post(watcher.url().split("?")[0] + "/players/kick",
    { form: { _csrf: csrf, player: "Mara" }, headers: { "HX-Request": "true" } });
  expect(kick.status()).toBe(403);
  await expect(onlineNames(watcher)).toHaveText(["Mara"]);
  await command(console, "fake-leave Mara");
});

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
  await expect(onlineNames(page)).toHaveText(["Mara", "Theo"]);
  await expect(players.locator(".player-slot--taken")).toHaveCount(2);

  // The list shows the count and names; stopped servers show nothing to
  // read.
  await page.goto("/");
  const survival = serverRow(page, "Survival").locator(".server-row__players");
  await expect(survival).toContainText("2/10");
  await expect(survival).toContainText("Mara, Theo");
  await expect(serverRow(page, "valheim").locator(".server-row__players")).toContainText("—Not running");

  // The open list updates as soon as someone leaves.
  await command(console, "fake-leave Theo");
  await expect(survival).toContainText("1/10");
  await expect(survival).not.toContainText("Theo");
  await command(console, "fake-leave Mara");
  await expect(survival).toContainText("0/10");

  // While the console can't list players, the game's status query still
  // counts them.
  await command(console, "fake-join Mara");
  await command(console, "fake-outage 20");
  await openServer(page, "Survival");
  await expect(players).toContainText("Counted by the game’s status query", { timeout: 15_000 });
  await expect(players).toContainText(/1\s*of 10 online/);
  await expect(players.getByRole("list", { name: "Online players" })).toHaveCount(0);
  await command(console, "fake-leave Mara");

  // A game that answers neither is reported, not shown as empty.
  await command(console, "fake-hang 20");
  await openServer(page, "Survival");
  // The page can render just before the failed reading, and its live
  // events start a moment later; its next refresh, within 10 seconds,
  // shows the reading.
  await expect(players).toContainText("Couldn’t read players", { timeout: 15_000 });
  await expect(players).toContainText("Ludock reads them over the game console.");
  await page.goto("/");
  await expect(survival).toContainText("Unknown");
  // The next journey on this server needs a game that answers.
  await command(console, "fake-hang 0");

  await openServer(page, "valheim");
  await expect(page.locator(".overview-players")).toContainText("Shown while the server is running");
});

test("seeing who is online on a Palworld server", async ({ page }) => {
  await dockerAction("palworld", "start");
  try {
    // Palworld says who is online but not how many it takes.
    await openServer(page, "palworld");
    const players = page.locator(".overview-players");
    await expect(players).toContainText(/0\s*online/, { timeout: 30_000 });
    await expect(players.locator(".player-slot")).toHaveCount(0);

    const console = await page.context().newPage();
    await command(console, "fake-join Ada <b>", "palworld");
    await expect(onlineNames(page)).toHaveText(["Ada <b>"]);
    await expect(players).toContainText(/1\s*online/);

    await page.goto("/");
    const row = serverRow(page, "palworld").locator(".server-row__players");
    await expect(row).toContainText("1");
    await expect(row).toContainText("Ada <b>");

    // Palworld kicks by the Steam ID its list gives, never the name.
    await command(console, "fake-join Zed", "palworld");
    await openServer(page, "palworld");
    await expect(onlineNames(page)).toHaveText(["Ada <b>", "Zed"], { timeout: 15_000 });
    await page.getByRole("button", { name: "Ada <b>: actions" }).click();
    await page.getByRole("button", { name: "Kick", exact: true }).click();
    await expect(page.locator("#detail-status")).toContainText("Asked the game to kick Ada <b>.");
    await expect(page.locator("#detail-status")).toContainText("The game replied: Kicked: Ada <b>");
    await expect(onlineNames(page)).toHaveText(["Zed"]);
    await command(console, "fake-leave Zed", "palworld");

    await command(console, "fake-outage 20", "palworld");
    await openServer(page, "palworld");
    await expect(players).toContainText("Couldn’t read players", { timeout: 15_000 });
    await expect(players).toContainText("Ludock reads them over the game console.");
    await command(console, "fake-outage 0", "palworld");
  } finally {
    await dockerAction("palworld", "stop");
  }
});

