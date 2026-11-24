import { writeFile } from "node:fs/promises";
import { join } from "node:path";
import { test, expect, asAdmin, dockerAction, monitoringSettings, openServer, restoreServer, serverRow } from "../fixtures.mjs";
import { freshStack } from "../../setup.mjs";

asAdmin();
// Each journey waits for monitoring to notice, so they run beside each other.
test.describe.configure({ mode: "parallel" });

// While fake-hang is in the fake Valheim server's data folder, its status
// query goes silent, as a game that hangs while its container keeps running,
// until the container starts again, which a restart cures.
const valheimFile = (name, dir = process.env.LUDOCK_E2E_DIR) => join(dir, "docker", "data", "valheim", name);

/**
 * Turns Valheim's monitoring on with a short wait before alerting, then
 * starts it through Ludock. A stop through Ludock, as an earlier journey's
 * restoreServer, pauses monitoring for the wait it had then; the start
 * replaces that pause with this one.
 */
async function startWatched(page, seconds) {
  await openServer(page, "valheim");
  await monitoringSettings(page);
  await page.getByLabel("Monitor this server").check();
  const wait = page.getByLabel("Wait before alerting (seconds)");
  if (await wait.inputValue() !== String(seconds)) {
    const saved = page.waitForResponse((response) => response.request().method() === "POST" && response.url().endsWith("/availability"));
    await wait.fill(String(seconds));
    await wait.press("Tab");
    expect((await saved).ok()).toBe(true);
  }
  await page.locator(".detail-actions").getByRole("button", { name: "Start" }).click();
  await expect(page.locator(".detail-actions").getByRole("button", { name: "Stop" })).toBeVisible();
}

test.describe("a game that hangs while Ludock restarts", () => {
  // Restarting Ludock would disturb the other journeys, so this one has an
  // install of its own.
  let stack;
  test.beforeAll(async ({}, info) => {
    stack = await freshStack(info.parallelIndex);
  });
  test.afterAll(async () => {
    await stack?.stop();
  });

  test("is still caught: it alerts, and automatic restart restarts it", async ({ session }) => {
    test.setTimeout(360_000);
    const page = await session({ baseURL: stack.env.LUDOCK_E2E_URL, storageState: stack.env.LUDOCK_E2E_ADMIN_STATE });
    // The outage starts at the first monitor tick after a reading finds the
    // game silent, and Ludock restarts within seconds of that reading, so a
    // 20-second wait ends after the restart, which must judge it afresh.
    await startWatched(page, 20);
    const game = page.locator(".overview-game");
    await expect(game).toContainText("Answering", { timeout: 45_000 });
    await writeFile(valheimFile("fake-hang", stack.env.LUDOCK_E2E_DIR), "");
    await expect(game).toContainText("Not answering", { timeout: 45_000 });

    // The new Ludock has never heard the game answer, but knows it did in
    // this run, so it doesn't take it for a game still starting.
    // A page left open would see its live updates drop.
    await page.goto("about:blank");
    await stack.restart();
    await openServer(page, "valheim");
    await expect(page.locator(".overview-game")).toContainText("Not answering", { timeout: 45_000 });
    await expect(page.locator(".overview-game")).toContainText("the game stopped answering its status check");
    // The container still runs; only the game went quiet.
    await expect(page.locator(".overview-facts").first()).toContainText("Running");
    await page.goto("/");
    const attention = page.locator("#attention");
    await expect(attention).toContainText("Game not answering", { timeout: 90_000 });
    await openServer(page, "valheim");
    await expect(page.locator("#availability-title + p")).toHaveText("The server is down.");
    await expect(page.getByRole("region", { name: "Monitoring" }).locator(".metadata-list")).toContainText("Running, but the game isn’t answering");

    const saved = page.waitForResponse((response) => response.url().endsWith("/availability"));
    await page.getByLabel("Restart it if the game stops answering").check();
    expect((await saved).ok()).toBe(true);
    const status = page.locator(".recovery-status");
    await expect(status).toContainText("because the game stopped answering", { timeout: 120_000 });
    await expect(status).toContainText("It answered again.", { timeout: 60_000 });
    await page.locator(".section-tabs").getByRole("link", { name: "Activity" }).click();
    await expect(page.getByText("By Ludock, because the game stopped answering").first()).toBeVisible();
    await page.getByRole("link", { name: "Restart" }).first().click();
    await expect(page.locator(".metadata-list")).toContainText("Ludock, because the game stopped answering");
    // Its answer ends the outage.
    await openServer(page, "valheim");
    await expect(async () => {
      await page.reload();
      await expect(page.locator("#availability-title + p")).toHaveText("No outage detected.", { timeout: 1000 });
    }).toPass({ timeout: 30_000, intervals: [3_000] });
  });
});

test("an outage reaches an open server list when its grace period ends, and clears", async ({ page }) => {
  test.setTimeout(150_000);
  try {
    await startWatched(page, 10);
    const detail = await page.context().newPage();
    await openServer(detail, "valheim");

    await page.goto("/");
    const attention = page.locator("#attention");
    await expect(attention).not.toContainText("Server down");
    await dockerAction("valheim", "stop");
    await expect(serverRow(page, "valheim")).toContainText("Stopped");
    await expect(attention).toContainText("Server down", { timeout: 60_000 });
    // The list and its summary call it down, as the alert does, and so
    // does the server's open page.
    await expect(serverRow(page, "valheim").locator(".server-row__state")).toContainText("Down");
    await expect(page.locator("#fleet")).toContainText("1 down");
    await expect(detail.locator(".detail-header__identity")).toContainText("Down", { timeout: 30_000 });

    // Starting it is what the owner came for: Start leads on the row, the
    // page, and the attention item, which starts it.
    const row = serverRow(page, "valheim");
    await expect(row.getByRole("button", { name: "Start" })).toHaveClass(/secondary-btn--accent/);
    await expect(row.getByRole("link", { name: "Logs" })).not.toHaveClass(/secondary-btn--accent/);
    await detail.reload();
    await expect(detail.locator(".detail-actions").getByRole("button", { name: "Start" })).toHaveClass(/primary-btn/);
    await expect(detail.locator(".detail-actions").getByRole("link", { name: "Logs" })).not.toHaveClass(/primary-btn/);
    await attention.getByRole("button", { name: "Start valheim" }).click();
    await expect(attention).not.toContainText("Server down", { timeout: 30_000 });
    await expect(row.getByRole("button", { name: "Stop" })).toBeVisible();

    await monitoringSettings(detail);
    await detail.getByLabel("Monitor this server").uncheck();
    await expect(detail.locator("#availability-title + p")).toHaveText("Monitoring is off.");
  } finally {
    await restoreServer(page, "valheim");
  }
});
