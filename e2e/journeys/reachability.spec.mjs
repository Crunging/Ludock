import { rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { test, expect, asAdmin, dockerAction, serverRow, openServer } from "./fixtures.mjs";

asAdmin();

// The fake Valheim server's status query goes silent while this file
// exists, as a game that hangs while its container keeps running.
const silence = () => join(process.env.LUDOCK_E2E_DIR, "docker", "data", "valheim", "fake-silent");

test("a game that stops answering while its container runs is an outage, until it answers again", async ({ page }) => {
  test.setTimeout(360_000);
  await dockerAction("valheim", "start");
  try {
    await openServer(page, "valheim");
    const game = page.locator(".overview-game");
    // Readings run every 30 seconds.
    await expect(game).toContainText("Answering", { timeout: 35_000 });
    await expect(game).toContainText("Over its Steam server query");
    // On by default, unless a journey before this one turned it off.
    await page.getByLabel("Monitor this server").check();
    await page.getByText("Alert levels").click();
    // A wait no other journey sets, so the change always saves.
    await page.getByLabel("Wait before alerting (seconds)").fill("12");
    const saved = page.waitForResponse((response) => response.request().method() === "POST" && response.url().endsWith("/availability"));
    await page.getByLabel("Wait before alerting (seconds)").press("Tab");
    expect((await saved).ok()).toBe(true);
    // An earlier stop through Ludock pauses monitoring for its grace period.
    await expect.poll(async () => {
      await page.reload();
      return page.locator("#availability-title + p").textContent();
    }, { timeout: 130_000, intervals: [5_000] }).toBe("No outage detected.");

    await writeFile(silence(), "");
    // The container still runs; only the game went quiet.
    await expect(game).toContainText("Not answering", { timeout: 35_000 });
    await expect(game).toContainText("the game stopped answering its Steam server query");
    await expect(page.locator(".overview-facts")).toContainText("Running");

    await page.goto("/");
    const attention = page.locator("#attention");
    // The next monitor tick starts the outage, and the one after its grace
    // period reports it.
    await expect(attention).toContainText("Game not answering", { timeout: 90_000 });
    await openServer(page, "valheim");
    await expect(page.locator("#availability-title + p")).toHaveText("Availability problem detected.");
    await expect(page.locator(".metadata-list")).toContainText("Running, but the game isn’t answering");

    await rm(silence());
    await expect(game).toContainText("Answering", { timeout: 35_000 });
    await page.goto("/");
    await expect(attention).not.toContainText("Game not answering", { timeout: 30_000 });

    await openServer(page, "valheim");
    await page.getByLabel("Monitor this server").uncheck();
    await expect(page.locator("#availability-title + p")).toHaveText("Monitoring disabled.");
  } finally {
    await rm(silence(), { force: true });
    await dockerAction("valheim", "stop");
  }
});

test("counting players with the game's status query when it has no console", async ({ page }) => {
  await openServer(page, "palworld");
  await expect(page.locator(".overview-game")).toContainText("Not checked");

  await dockerAction("valheim", "start");
  try {
    await openServer(page, "valheim");
    const players = page.locator(".overview-players");
    await expect(players).toContainText(/2\s*of 10 online/, { timeout: 35_000 });
    await expect(players).toContainText("Counted by the game’s Steam server query");
    await expect(players.locator(".player-slot--taken")).toHaveCount(2);
    await expect(page.locator(".overview-game")).toContainText("Answering");

    await page.goto("/");
    await expect(serverRow(page, "valheim").locator(".server-row__players")).toContainText("2/10");
  } finally {
    await dockerAction("valheim", "stop");
  }
  await openServer(page, "valheim");
  await expect(page.locator(".overview-game")).toContainText("Checked while the server is running");
});
