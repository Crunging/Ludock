import { rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { test, expect, asAdmin, dockerAction, expectOperationSucceeded, fake, openServer, restoreServer, serverRow, withGame } from "../fixtures.mjs";

asAdmin();
// Each journey waits for monitoring to notice, so they run beside each other.
test.describe.configure({ mode: "parallel" });

// While one of these files is in the fake Valheim server's data folder, its
// status query goes silent, as a game that hangs while its container keeps
// running. fake-hang lasts until the container starts again, which a
// restart cures.
const valheimFile = (name) => join(process.env.LUDOCK_E2E_DIR, "docker", "data", "valheim", name);

/**
 * Turns Valheim's monitoring on with a short wait before alerting, then
 * starts it through Ludock. A stop through Ludock, as an earlier journey's
 * restoreServer, pauses monitoring for the wait it had then; the start
 * replaces that pause with this one.
 */
async function startWatched(page, seconds) {
  await openServer(page, "valheim");
  await page.getByLabel("Monitor this server").check();
  await page.getByText("Alert levels").click();
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

test("a game that stops answering while its container runs is an outage, until it answers again", async ({ page }) => {
  test.setTimeout(240_000);
  const silence = valheimFile("fake-silent");
  try {
    // A wait no other journey sets.
    await startWatched(page, 12);
    const game = page.locator(".overview-game");
    // Readings run every 30 seconds, and the overview shows them within 10.
    await expect(game).toContainText("Answering", { timeout: 45_000 });
    await expect(game).toContainText("Over its status check");

    // Monitoring may still be in the start's quiet time; the outage begins
    // once it ends.
    await writeFile(silence, "");
    // The container still runs; only the game went quiet.
    await expect(game).toContainText("Not answering", { timeout: 45_000 });
    await expect(game).toContainText("the game stopped answering its status check");
    await expect(page.locator(".overview-facts")).toContainText("Running");

    await page.goto("/");
    const attention = page.locator("#attention");
    // The next monitor tick starts the outage, and the one after its grace
    // period reports it.
    await expect(attention).toContainText("Game not answering", { timeout: 90_000 });
    await openServer(page, "valheim");
    await expect(page.locator("#availability-title + p")).toHaveText("The server is down.");
    await expect(page.locator(".metadata-list")).toContainText("Running, but the game isn’t answering");

    await rm(silence);
    await expect(page.locator(".overview-game")).toContainText("Answering", { timeout: 45_000 });
    await page.goto("/");
    await expect(attention).not.toContainText("Game not answering", { timeout: 30_000 });

    await openServer(page, "valheim");
    await page.getByLabel("Monitor this server").uncheck();
    await expect(page.locator("#availability-title + p")).toHaveText("Monitoring is off.");
  } finally {
    await rm(silence, { force: true });
    await restoreServer(page, "valheim");
  }
});

// When recovery acts, gives up, or stays out of the way is covered by the Go
// tests in internal/monitor and internal/jobs.
test("Ludock restarts a game that stopped answering, and says when it answers again", async ({ page }) => {
  test.setTimeout(240_000);
  const hang = valheimFile("fake-hang");
  try {
    // The shortest wait before alerting, so the restart comes two monitor
    // checks after the game goes silent.
    await startWatched(page, 10);
    const game = page.locator(".overview-game");
    await expect(game).toContainText("Answering", { timeout: 45_000 });
    const saved = page.waitForResponse((response) => response.url().endsWith("/availability"));
    await page.getByLabel("Restart it if the game stops answering").check();
    expect((await saved).ok()).toBe(true);
    const status = page.locator(".recovery-status");
    await expect(status).toHaveText("On. Not needed so far.");

    await writeFile(hang, "");
    await expect(status).toContainText("because the game stopped answering", { timeout: 120_000 });
    await expect(status).toContainText("It answered again.", { timeout: 60_000 });
    await expect(game).toContainText("Answering");

    await page.getByRole("tab", { name: "Activity" }).click();
    await expect(page.getByText("By Ludock, because the game stopped answering").first()).toBeVisible();
    await page.getByRole("link", { name: "Restart" }).first().click();
    await expect(page.locator(".metadata-list")).toContainText("Ludock, because the game stopped answering");
  } finally {
    await rm(hang, { force: true });
    await openServer(page, "valheim");
    await page.getByLabel("Restart it if the game stops answering").uncheck();
    await restoreServer(page, "valheim");
  }
});

test("a game without a status query answers through its image's health check, which can restart it", async ({ page }) => {
  // Factorio has no status query, but factoriotools/factorio's health check
  // asks the game, so a hung game was only Unhealthy and never restarted.
  test.setTimeout(240_000);
  await withGame(page, "factorio", async (factorio) => {
    await openServer(page, factorio);
    const game = page.locator(".overview-game");
    await expect(game).toContainText("Answering");
    await expect(game).toContainText("Its image’s health check passed");
    await page.getByText("Alert levels").click();
    const wait = page.getByLabel("Wait before alerting (seconds)");
    const saved = page.waitForResponse((response) => response.request().method() === "POST" && response.url().endsWith("/availability"));
    await wait.fill("10");
    await wait.press("Tab");
    expect((await saved).ok()).toBe(true);
    const recovery = page.waitForResponse((response) => response.url().endsWith("/availability"));
    await page.getByLabel("Restart it if the game stops answering").check();
    expect((await recovery).ok()).toBe(true);
    const status = page.locator(".recovery-status");
    await expect(status).toHaveText("On. Not needed so far.");

    await fake("/fake/health", { container: factorio, status: "unhealthy" });
    await expect(game).toContainText("its image’s health check keeps failing", { timeout: 45_000 });
    // The restart starts the game again, and its health check passes.
    await expect(status).toContainText("because the game stopped answering", { timeout: 120_000 });
    await expect(status).toContainText("It answered again.", { timeout: 60_000 });
    await expect(game).toContainText("Its image’s health check passed");
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

    await detail.getByLabel("Monitor this server").uncheck();
    await expect(detail.locator("#availability-title + p")).toHaveText("Monitoring is off.");
  } finally {
    await restoreServer(page, "valheim");
  }
});

// Nobody configured Enshrouded's monitoring. Stopped when Ludock found it, it
// stays quiet; once it runs, a stop outside Ludock is an outage.
test("a server nobody set up is monitored", async ({ page }) => {
  await openServer(page, "enshrouded");
  const status = page.locator("#availability-title + p");
  await expect(page.getByLabel("Monitor this server")).toBeChecked();
  await expect(status).toHaveText("Stopped. Alerts resume once the server is running again.");
  await dockerAction("enshrouded", "start");
  try {
    await expect(status).toHaveText("No outage detected.", { timeout: 30_000 });
    await dockerAction("enshrouded", "stop");
    // Another journey may have shortened its wait before alerting.
    await expect(status).toHaveText(/^The server (looks|is) down\./, { timeout: 30_000 });
    await dockerAction("enshrouded", "start");
    await expect(status).toHaveText("No outage detected.", { timeout: 30_000 });
  } finally {
    // A stop through Ludock stays quiet for the journeys after this one.
    await page.goto(page.url());
    await page.locator(".detail-actions").getByRole("button", { name: "Stop" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Stop server" }).click();
    await expectOperationSucceeded(page);
  }
  await page.getByRole("tab", { name: "Overview" }).click();
  await expect(status).toHaveText("Stopped. Alerts resume once the server is running again.");
});
