import { request } from "node:http";
import { join } from "node:path";
import { test, expect, asAdmin, dockerAction, openServer } from "./fixtures.mjs";

asAdmin();

const GiB = 1024 ** 3;
const valheimData = () => join(process.env.LUDOCK_E2E_DIR, "docker", "data", "valheim");

/** Calls one of the fake daemon's journey controls, such as /fake/disk. */
function fake(path, query) {
  return new Promise((resolve, reject) => {
    const call = request({ socketPath: join(process.env.LUDOCK_E2E_DIR, "docker", "docker.sock"), method: "POST",
      path: path + "?" + new URLSearchParams(query) }, (response) => {
      response.resume();
      response.on("end", resolve);
    });
    call.on("error", reject);
    call.end();
  });
}

/** Makes measuring Valheim's data folder report a disk this full, or fail. */
const disk = (percent) => fake("/fake/disk", percent === "fail" ? { path: valheimData(), fail: "1" }
  : percent === undefined ? { path: valheimData() } : { path: valheimData(), used: percent * GiB, available: (100 - percent) * GiB });

/** Changes a monitoring number and waits for the save. */
async function setNumber(page, label, value) {
  const saved = page.waitForResponse((response) => response.request().method() === "POST" && response.url().endsWith("/availability"));
  await page.getByLabel(label).fill(String(value));
  await page.getByLabel(label).press("Tab");
  expect((await saved).ok()).toBe(true);
}

async function toggle(page, label, on) {
  const saved = page.waitForResponse((response) => response.request().method() === "POST" && response.url().endsWith("/availability"));
  await page.getByLabel(label).setChecked(on);
  expect((await saved).ok()).toBe(true);
}

/** Reloads the dashboard until Needs attention does or doesn't show text. */
async function expectAttention(page, text, shown, timeout = 100_000) {
  const attention = expect.poll(async () => {
    await page.goto("/");
    await expect(page.locator("#attention")).not.toBeEmpty();
    return page.locator("#attention").textContent();
  }, { timeout, intervals: [5_000] });
  await (shown ? attention.toContain(text) : attention.not.toContain(text));
}

test("a stopped server's disk space, a nearly full disk, an unknown measurement, and a changed threshold", async ({ page }) => {
  test.setTimeout(480_000);
  try {
    await openServer(page, "valheim");
    const figure = page.locator(".overview-disk");
    await expect(figure).toContainText("Measured while monitoring is on");
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
    await expect(page.locator(".metadata-list")).toContainText("Disk nearly full");

    // A measurement that fails says so, keeps the figures, and keeps the alert.
    await disk("fail");
    await expect(figure).toContainText("Couldn’t be measured just now", { timeout: 100_000 });
    await expect(figure).toContainText("95% full");
    await expectAttention(page, "Disk nearly full", true, 5_000);

    // Alerting disks are measured every minute, so freed space clears soon.
    await disk(50);
    await expectAttention(page, "Disk nearly full", false);
    await openServer(page, "valheim");
    await expect(figure).toContainText("50% full");
    await expect(figure).not.toContainText("Couldn’t be measured");

    await disk(95);
    await setNumber(page, "Disk alert level (% full)", 92);
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
    await expectAttention(page, "Disk nearly full", false, 5_000);

    await openServer(page, "valheim");
    await setNumber(page, "Disk alert level (% full)", 93);
    await expectAttention(page, "Disk nearly full", true, 60_000);
    await openServer(page, "valheim");
    await toggle(page, "Alert when a data folder’s disk is nearly full", false);
    await expect(page.getByLabel("Disk alert level (% full)")).toBeHidden();
    await expectAttention(page, "Disk nearly full", false, 10_000);
  } finally {
    await disk(undefined);
    await openServer(page, "valheim");
    if (await page.getByLabel("Monitor this server").isChecked()) await toggle(page, "Monitor this server", false);
  }
});

test("memory that stays near its container's limit alerts, until it falls or the alert is turned off", async ({ page }) => {
  test.setTimeout(480_000);
  await dockerAction("valheim", "start");
  try {
    await openServer(page, "valheim");
    // The overview refreshes every 10 seconds; a start from outside Ludock
    // can take a refresh or two to show.
    await expect(page.locator(".overview-facts")).toContainText("of 2 GiB", { timeout: 30_000 });
    await toggle(page, "Monitor this server", true);
    await setNumber(page, "For at least (minutes)", 1);

    await fake("/fake/stats", { container: "valheim", memory: 1.95 * GiB });
    // Samples are taken once a minute.
    await expectAttention(page, "Memory near its limit", true);
    await expect(page.locator("#attention")).toContainText("Over 90% of its memory limit since");
    await openServer(page, "valheim");
    await expect(page.locator(".metadata-list")).toContainText("Memory near its limit");

    await fake("/fake/stats", { container: "valheim", memory: GiB });
    await expectAttention(page, "Memory near its limit", false);

    await fake("/fake/stats", { container: "valheim", memory: 1.95 * GiB });
    await expectAttention(page, "Memory near its limit", true);
    await openServer(page, "valheim");
    await toggle(page, "Alert when memory stays near the container’s limit", false);
    await expect(page.getByLabel("Memory alert level (% of limit)")).toBeHidden();
    await expectAttention(page, "Memory near its limit", false, 10_000);
  } finally {
    await fake("/fake/stats", { container: "valheim", memory: GiB });
    await openServer(page, "valheim");
    if (await page.getByLabel("Monitor this server").isChecked()) await toggle(page, "Monitor this server", false);
    await dockerAction("valheim", "stop");
  }
});
