import { join } from "node:path";
import { test, expect, asAdmin, fake, monitoringSettings, openServer, restoreServer } from "../fixtures.mjs";

asAdmin();

const GiB = 1024 ** 3;
const valheimData = () => join(process.env.LUDOCK_E2E_DIR, "docker", "data", "valheim");

/** Makes measuring Valheim's data folder report a disk this full. */
const disk = (percent) => fake("/fake/disk", percent === undefined ? { path: valheimData() }
  : { path: valheimData(), used: percent * GiB, available: (100 - percent) * GiB });

/** Changes a monitoring number and waits for the save. */
async function setNumber(page, label, value) {
  await monitoringSettings(page);
  const saved = page.waitForResponse((response) => response.request().method() === "POST" && response.url().endsWith("/availability"));
  await page.getByLabel(label).fill(String(value));
  await page.getByLabel(label).press("Tab");
  expect((await saved).ok()).toBe(true);
}

async function toggle(page, label, on) {
  await monitoringSettings(page);
  if ((await page.getByLabel(label).isChecked()) === on) return;
  const saved = page.waitForResponse((response) => response.request().method() === "POST" && response.url().endsWith("/availability"));
  await page.getByLabel(label).setChecked(on);
  expect((await saved).ok()).toBe(true);
}

/** Reloads the dashboard until Needs attention shows text. */
async function expectAttention(page, text, timeout) {
  await expect.poll(async () => {
    const loaded = page.waitForResponse((response) => response.url().includes("/fragments/attention"));
    await page.goto("/");
    await loaded;
    await expect(page.locator("#attention .needs-attention__error")).toHaveCount(0);
    return page.locator("#attention").textContent();
  }, { timeout, intervals: [3_000] }).toContain(text);
}

// Valheim is stopped, and its data folder is measured all the same.
// Alerting disks are measured every minute, so freed space ends the alert
// within one. When a changed policy or a failed measurement ends or keeps
// an alert is covered by the Go tests in internal/monitor.
test("a nearly full disk alerts, until space is freed", async ({ page }) => {
  test.setTimeout(180_000);
  try {
    await disk(95);
    await openServer(page, "valheim");
    const figure = page.locator(".overview-disk");
    // On by default, unless a journey before this one turned it off.
    await toggle(page, "Monitor this server", true);
    // A changed threshold measures on the next tick.
    await setNumber(page, "Disk alert level (% full)", 91);
    await expectAttention(page, `${valheimData()} is 95% full`, 45_000);
    await openServer(page, "valheim");
    await expect(figure.locator("code")).toHaveText(valheimData());
    await expect(figure).toContainText("95% full");
    await expect(figure).toContainText("5 GiB free of 100 GiB");
    await expect(page.getByRole("region", { name: "Monitoring" }).locator(".metadata-list")).toContainText("Disk nearly full");

    // The measurement that finds the space freed ends the alert. The
    // server's page renders both, where an empty attention fragment can't
    // tell an ended alert from one not loaded yet.
    await disk(50);
    await expect(async () => {
      await page.reload();
      await expect(figure).toContainText("50% full", { timeout: 1000 });
    }).toPass({ timeout: 90_000, intervals: [3_000] });
    await expect(page.getByRole("region", { name: "Monitoring" })).not.toContainText("Disk nearly full");
  } finally {
    await disk(undefined);
    await restoreServer(page, "valheim");
  }
});
