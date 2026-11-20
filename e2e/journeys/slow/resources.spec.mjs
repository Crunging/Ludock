import { join } from "node:path";
import { test, expect, asAdmin, dockerAction, fake, monitoringSettings, openServer, restoreServer } from "../fixtures.mjs";

asAdmin();
// Each journey waits for monitoring to measure, so they run beside each other.
test.describe.configure({ mode: "parallel" });

const GiB = 1024 ** 3;
const valheimData = () => join(process.env.LUDOCK_E2E_DIR, "docker", "data", "valheim");

/** Makes measuring Valheim's data folder report a disk this full, or fail. */
const disk = (percent) => fake("/fake/disk", percent === "fail" ? { path: valheimData(), fail: "1" }
  : percent === undefined ? { path: valheimData() } : { path: valheimData(), used: percent * GiB, available: (100 - percent) * GiB });

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

/** Reloads the dashboard until Needs attention does or doesn't show text. */
async function expectAttention(page, text, shown, timeout = 100_000) {
  const attention = expect.poll(async () => {
    // It shows nothing until it loads, and nothing when nothing is wrong.
    const loaded = page.waitForResponse((response) => response.url().includes("/fragments/attention"));
    await page.goto("/");
    await loaded;
    await expect(page.locator("#attention .needs-attention__error")).toHaveCount(0);
    return page.locator("#attention").textContent();
  }, { timeout, intervals: [5_000] });
  await (shown ? attention.toContain(text) : attention.not.toContain(text));
}

test("a stopped server's disk space, a nearly full disk, and a measurement that fails", async ({ page }) => {
  test.setTimeout(240_000);
  try {
    await openServer(page, "valheim");
    const figure = page.locator(".overview-disk");
    // On by default, unless a journey before this one turned it off.
    await toggle(page, "Monitor this server", true);
    // Measured on the next tick, though the server is stopped.
    await expect(figure.locator("code")).toHaveText(valheimData(), { timeout: 45_000 });
    await expect(figure).toContainText(/\d+% full/);
    await expect(figure).toContainText(/free of/);

    await disk(95);
    // A changed threshold measures again on the next tick.
    await setNumber(page, "Disk alert level (% full)", 91);
    await expectAttention(page, "Disk nearly full", true, 60_000);
    await expect(page.locator("#attention")).toContainText(`${valheimData()} is 95% full`);
    await openServer(page, "valheim");
    await expect(figure).toContainText("95% full");
    await expect(figure).toContainText("5 GiB free of 100 GiB");
    await expect(page.getByRole("region", { name: "Monitoring" }).locator(".metadata-list")).toContainText("Disk nearly full");

    // A measurement that fails says so, keeps the figures, and keeps the
    // alert. Alerting disks are measured every minute.
    await disk("fail");
    await expect(figure).toContainText("Couldn’t be measured just now", { timeout: 100_000 });
    await expect(figure).toContainText("95% full");
    await expectAttention(page, "Disk nearly full", true, 15_000);
  } finally {
    await disk(undefined);
    await restoreServer(page, "valheim");
  }
});

test("freed space ends a disk alert within a minute", async ({ page }) => {
  test.setTimeout(240_000);
  try {
    await openServer(page, "valheim");
    await toggle(page, "Monitor this server", true);
    await disk(95);
    await setNumber(page, "Disk alert level (% full)", 92);
    await expectAttention(page, "Disk nearly full", true, 60_000);

    // Alerting disks are measured every minute, so freed space clears soon.
    await disk(50);
    await expectAttention(page, "Disk nearly full", false);
    await openServer(page, "valheim");
    const figure = page.locator(".overview-disk");
    await expect(figure).toContainText("50% full");
    await expect(figure).not.toContainText("Couldn’t be measured");
  } finally {
    await disk(undefined);
    await restoreServer(page, "valheim");
  }
});

/**
 * Starts Valheim outside Ludock using nearly all its memory, and waits for
 * the alert judged over one minute.
 */
async function watchMemory(page) {
  // Samples are taken once a minute from the start, and those taken before
  // the alert's settings change still count.
  await fake("/fake/stats", { container: "valheim", memory: 1.95 * GiB });
  await dockerAction("valheim", "start");
  await openServer(page, "valheim");
  // The overview refreshes every 10 seconds; a start from outside Ludock
  // can take a refresh or two to show.
  await expect(page.getByRole("region", { name: "Resources" })).toContainText("of 2 GiB", { timeout: 30_000 });
  await toggle(page, "Monitor this server", true);
  await setNumber(page, "For at least (minutes)", 1);
  await expectAttention(page, "Memory near its limit", true);
}

test("memory that stays near its container's limit alerts, until it falls", async ({ page }) => {
  test.setTimeout(240_000);
  try {
    await watchMemory(page);
    await expect(page.locator("#attention")).toContainText("Over 90% of its memory limit since");
    await openServer(page, "valheim");
    await expect(page.getByRole("region", { name: "Monitoring" }).locator(".metadata-list")).toContainText("Memory near its limit");

    await fake("/fake/stats", { container: "valheim", memory: GiB });
    await expectAttention(page, "Memory near its limit", false);
  } finally {
    await fake("/fake/stats", { container: "valheim", memory: GiB });
    await restoreServer(page, "valheim");
  }
});

test("a disk alert level above the disk's figure, or turned off, ends its alert", async ({ page }) => {
  test.setTimeout(240_000);
  try {
    await openServer(page, "valheim");
    const figure = page.locator(".overview-disk");
    await toggle(page, "Monitor this server", true);
    await disk(95);
    await setNumber(page, "Disk alert level (% full)", 93);
    await expectAttention(page, "Disk nearly full", true, 60_000);

    // Above the disk's figure, the threshold no longer alerts, even once
    // the disk is measured again.
    await openServer(page, "valheim");
    const measured = await figure.locator("time").getAttribute("datetime");
    await setNumber(page, "Disk alert level (% full)", 97);
    await expect.poll(async () => {
      await page.reload();
      return figure.locator("time").getAttribute("datetime");
    }, { timeout: 45_000, intervals: [3_000] }).not.toBe(measured);
    await expectAttention(page, "Disk nearly full", false, 15_000);

    await openServer(page, "valheim");
    await setNumber(page, "Disk alert level (% full)", 94);
    await expectAttention(page, "Disk nearly full", true, 60_000);
    await openServer(page, "valheim");
    await toggle(page, "Alert when a data folder’s disk is nearly full", false);
    await expect(page.getByLabel("Disk alert level (% full)")).toBeHidden();
    await expectAttention(page, "Disk nearly full", false, 15_000);
  } finally {
    await disk(undefined);
    await restoreServer(page, "valheim");
  }
});

test("turning the memory alert off ends it", async ({ page }) => {
  test.setTimeout(240_000);
  try {
    await watchMemory(page);
    await openServer(page, "valheim");
    await toggle(page, "Alert when memory stays near the server’s limit", false);
    await expect(page.getByLabel("Memory alert level (% of limit)")).toBeHidden();
    await expectAttention(page, "Memory near its limit", false, 15_000);
  } finally {
    await fake("/fake/stats", { container: "valheim", memory: GiB });
    await restoreServer(page, "valheim");
  }
});
