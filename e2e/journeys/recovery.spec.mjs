import { rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { test, expect, asAdmin, dockerAction, openServer } from "./fixtures.mjs";

asAdmin();

// In the fake Valheim server's data folder, fake-hang silences its status
// query until the container starts again, as a hang a restart cures;
// fake-silent keeps it silent across restarts.
const valheimFile = (name) => join(process.env.LUDOCK_E2E_DIR, "docker", "data", "valheim", name);

/** Starts Valheim with monitoring on, a 10-second wait, and recovery set to recover. */
async function watchValheim(page, recover) {
  await dockerAction("valheim", "start");
  await openServer(page, "valheim");
  await expect(page.locator(".overview-game")).toContainText("Answering", { timeout: 35_000 });
  await page.getByLabel("Monitor this server").check();
  await page.getByText("Alert levels").click();
  const wait = page.getByLabel("Wait before alerting (seconds)");
  if (await wait.inputValue() !== "10") {
    const saved = page.waitForResponse((response) => response.url().endsWith("/availability"));
    await wait.fill("10");
    await wait.press("Tab");
    expect((await saved).ok()).toBe(true);
  }
  const restart = page.getByLabel("Restart it if the game stops answering");
  if (await restart.isChecked() !== recover) {
    const saved = page.waitForResponse((response) => response.url().endsWith("/availability"));
    await restart.setChecked(recover);
    expect((await saved).ok()).toBe(true);
  }
  // Monitoring resumes after an earlier action's pause.
  await expect.poll(async () => {
    await page.reload();
    return page.locator("#availability-title + p").textContent();
  }, { timeout: 60_000, intervals: [5_000] }).toBe("No outage detected.");
}

async function cleanUp(page) {
  await rm(valheimFile("fake-hang"), { force: true });
  await rm(valheimFile("fake-silent"), { force: true });
  await openServer(page, "valheim");
  await page.getByLabel("Restart it if the game stops answering").uncheck();
  await dockerAction("valheim", "stop");
}

const automaticRestarts = (page) => page.getByText("By Ludock, because the game stopped answering");

test("Ludock restarts a game that stopped answering, and says when it answers again", async ({ page }) => {
  test.setTimeout(300_000);
  try {
    await watchValheim(page, true);
    const status = page.locator(".recovery-status");
    await expect(status).toHaveText("On. Not needed so far.");

    await writeFile(valheimFile("fake-hang"), "");
    await expect(page.locator(".overview-game")).toContainText("Not answering", { timeout: 35_000 });
    // The tick after the wait restarts it, and the game answers in its new run.
    await expect(status).toContainText("because the game stopped answering", { timeout: 90_000 });
    await expect(status).toContainText("It answered again.", { timeout: 90_000 });
    await expect(page.locator(".overview-game")).toContainText("Answering");

    await page.getByRole("tab", { name: "Activity" }).click();
    await expect(automaticRestarts(page)).toHaveCount(1);
    await page.getByRole("link", { name: "Restart" }).first().click();
    await expect(page.locator(".metadata-list")).toContainText("Ludock (automatic restart)");
  } finally {
    await cleanUp(page);
  }
});

test("Ludock restarts a hung game once, and leaves one that still doesn't answer to a person", async ({ page }) => {
  test.setTimeout(300_000);
  try {
    await watchValheim(page, true);
    await page.getByRole("tab", { name: "Activity" }).click();
    await expect(page.locator("#recent-operations-title")).toBeVisible();
    const before = await automaticRestarts(page).count();
    await page.getByRole("tab", { name: "Overview" }).click();
    await writeFile(valheimFile("fake-silent"), "");
    const status = page.locator(".recovery-status");
    // Waits five times the 10-second alert wait for the game to answer.
    await expect(status).toContainText("but the game hasn’t answered since", { timeout: 180_000 });
    await expect(status).toContainText("It won’t be restarted again until the game answers.");
    await page.waitForTimeout(20_000);
    await page.getByRole("tab", { name: "Activity" }).click();
    await expect(automaticRestarts(page)).toHaveCount(before + 1);
  } finally {
    await cleanUp(page);
  }
});

test("Ludock doesn't restart a hung game when the switch is off, or once its owner stops it", async ({ page }) => {
  test.setTimeout(300_000);
  try {
    await watchValheim(page, false);
    await page.getByRole("tab", { name: "Activity" }).click();
    await expect(page.locator("#recent-operations-title")).toBeVisible();
    const before = await automaticRestarts(page).count();

    // Off: the hang alerts as an outage, and nothing restarts it.
    await writeFile(valheimFile("fake-hang"), "");
    await page.goto("/");
    await expect(page.locator("#attention")).toContainText("Game not answering", { timeout: 90_000 });
    await openServer(page, "valheim");
    await page.getByRole("tab", { name: "Activity" }).click();
    await expect(automaticRestarts(page)).toHaveCount(before);

    // On, but the owner stops it in another tool before the wait ends: it
    // stays down.
    await rm(valheimFile("fake-hang"));
    await watchValheim(page, true);
    await writeFile(valheimFile("fake-hang"), "");
    await expect(page.locator(".overview-game")).toContainText("Not answering", { timeout: 35_000 });
    await dockerAction("valheim", "stop");
    await page.waitForTimeout(45_000);
    await page.reload();
    await expect(page.locator(".overview-facts")).not.toContainText("Running");
    await page.getByRole("tab", { name: "Activity" }).click();
    await expect(automaticRestarts(page)).toHaveCount(before);
  } finally {
    await cleanUp(page);
  }
});
