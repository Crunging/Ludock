import { rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { test, expect, asAdmin, dockerAction, openServer } from "./fixtures.mjs";

asAdmin();

// The fake Valheim server's status query goes silent while this file is in
// its data folder, until the container starts again: a hang a restart cures.
// When recovery acts, gives up, or stays out of the way is covered by the Go
// tests in internal/monitor and internal/jobs.
const hang = () => join(process.env.LUDOCK_E2E_DIR, "docker", "data", "valheim", "fake-hang");

test("Ludock restarts a game that stopped answering, and says when it answers again", async ({ page }) => {
  test.setTimeout(240_000);
  await dockerAction("valheim", "start");
  try {
    await openServer(page, "valheim");
    const game = page.locator(".overview-game");
    await expect(game).toContainText("Answering", { timeout: 35_000 });
    await page.getByLabel("Monitor this server").check();
    // The shortest wait before alerting, so the restart comes two monitor
    // checks after the game goes silent.
    await page.getByText("Alert levels").click();
    const wait = page.getByLabel("Wait before alerting (seconds)");
    if (await wait.inputValue() !== "10") {
      const saved = page.waitForResponse((response) => response.url().endsWith("/availability"));
      await wait.fill("10");
      await wait.press("Tab");
      expect((await saved).ok()).toBe(true);
    }
    const saved = page.waitForResponse((response) => response.url().endsWith("/availability"));
    await page.getByLabel("Restart it if the game stops answering").check();
    expect((await saved).ok()).toBe(true);
    const status = page.locator(".recovery-status");
    await expect(status).toHaveText("On. Not needed so far.");

    // Monitoring may first wait out the pause after an earlier action.
    await writeFile(hang(), "");
    await expect(status).toContainText("because the game stopped answering", { timeout: 150_000 });
    await expect(status).toContainText("It answered again.", { timeout: 60_000 });
    await expect(game).toContainText("Answering");

    await page.getByRole("tab", { name: "Activity" }).click();
    await expect(page.getByText("By Ludock, because the game stopped answering").first()).toBeVisible();
    await page.getByRole("link", { name: "Restart" }).first().click();
    await expect(page.locator(".metadata-list")).toContainText("Ludock (automatic restart)");
  } finally {
    await rm(hang(), { force: true });
    await openServer(page, "valheim");
    await page.getByLabel("Restart it if the game stops answering").uncheck();
    await dockerAction("valheim", "stop");
  }
});
