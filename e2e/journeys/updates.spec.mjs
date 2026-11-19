import { createServer } from "node:http";
import { request } from "node:http";
import { join } from "node:path";
import { test, expect, asAdmin, openServer, serverRow, expectOperationSucceeded } from "./fixtures.mjs";

asAdmin();

const image = "itzg/minecraft-server:java21";

/** Points a tag in the fake daemon's registry at a new image ID, or makes its lookups fail with status. */
function registry(query) {
  return new Promise((resolve, reject) => {
    const call = request({ socketPath: join(process.env.LUDOCK_E2E_DIR, "docker", "docker.sock"), method: "POST",
      path: "/fake/registry?" + new URLSearchParams({ image, ...query }) }, (response) => {
      response.resume();
      response.on("end", resolve);
    });
    call.on("error", reject);
    call.end();
  });
}

/** Presses Check now until the result says text; Ludock asks the registry at most every 10 seconds. */
async function checkNow(page, text) {
  await page.getByRole("tab", { name: "Update" }).click();
  const check = page.locator("#image-check");
  await expect(async () => {
    await check.getByRole("button", { name: "Check now" }).click();
    await expect(check).toContainText(text, { timeout: 2_000 });
  }).toPass({ timeout: 30_000, intervals: [1_000, 3_000] });
}

async function updateSurvival(page) {
  const form = page.locator("#update-panel form");
  await form.getByLabel("Create a stopped-server backup before recreation").uncheck();
  await form.getByLabel(/Type Survival to skip the backup/).fill("Survival");
  await form.getByLabel(/I understand/).check();
  await form.getByRole("button", { name: "Update server" }).click();
  await expectOperationSucceeded(page);
}

// The fake daemon offers a newer image on the first pull. Updating needs no
// setup: Ludock recreates the container through Docker alone.
test("updating a server's image, then finding it current", async ({ page }) => {
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Update" }).click();
  const form = page.locator("#update-panel form");
  await expect(form.locator(".metadata-list")).toContainText("itzg/minecraft-server:java21");
  await form.getByText("What gets replaced").click();
  await expect(form.locator(".disclosure")).toContainText("including unnamed ones");
  await form.getByLabel("Create a stopped-server backup before recreation").uncheck();
  await form.getByLabel(/Type Survival to skip the backup/).fill("Survival");
  await form.getByLabel(/I understand/).check();
  await form.getByRole("button", { name: "Update server" }).click();
  await expectOperationSucceeded(page);
  await expect(page.locator(".detail-actions").getByRole("button", { name: "Stop" })).toBeEnabled();

  // The replacement is the same server, now running the pulled image.
  await page.getByRole("tab", { name: "Update" }).click();
  await form.getByLabel("Create a stopped-server backup before recreation").uncheck();
  await form.getByLabel(/Type Survival to skip the backup/).fill("Survival");
  await form.getByLabel(/I understand/).check();
  await form.getByRole("button", { name: "Update server" }).click();
  await expect(page.locator("#selected-operation")).toContainText("Already current", { timeout: 45_000 });
  await expect(page.getByRole("link", { name: "Recreate anyway" })).toBeVisible();
});

// A webhook records the alerts Ludock sends.
async function webhook(page) {
  const bodies = [];
  const server = createServer((incoming, response) => {
    let body = "";
    incoming.on("data", (chunk) => { body += chunk; });
    incoming.on("end", () => {
      bodies.push(JSON.parse(body));
      response.end("{}");
    });
  });
  await new Promise((done) => server.listen(0, "127.0.0.1", done));
  await page.goto("/settings");
  const card = page.locator("#notification-webhook-card");
  if (!(await card.evaluate((details) => details.open))) await card.locator("summary").click();
  await card.locator("input[name=url]").fill(`http://127.0.0.1:${server.address().port}/hook`);
  await card.getByLabel("Send alerts to the webhook").check();
  await card.getByRole("button", { name: "Save Webhook" }).click();
  await expect(card.locator("form .alert--success")).toHaveText("Webhook settings saved.");
  return {
    alerts: () => bodies.filter((body) => body.message.includes("newer version of its container image")),
    close: async () => {
      await page.goto("/settings");
      if (!(await card.evaluate((details) => details.open))) await card.locator("summary").click();
      await card.getByLabel("Send alerts to the webhook").uncheck();
      await card.getByRole("button", { name: "Save Webhook" }).click();
      await expect(card.locator("form .alert--success")).toHaveText("Webhook settings saved.");
      server.close();
    },
  };
}

// A new version of the tag appears in the registry. Administrators see it on
// the list and the Update tab, every destination is told once, and updating
// clears it.
test("finding a newer image, being told once, and updating to it", async ({ page }) => {
  const hook = await webhook(page);
  try {
    await registry({ id: "sha256:" + "3".repeat(64) });
    await openServer(page, "Survival");
    await checkNow(page, "A newer image is available.");
    const check = page.locator("#image-check");
    // Minecraft's image downloads the game on start, so the notice says which
    // update this is.
    await expect(check.locator(".alert")).toContainText("The game already updates on every restart.");
    await expect(check).toContainText("Last checked");

    await page.goto("/");
    await expect(serverRow(page, "Survival").getByRole("link", { name: "Image update available" })).toBeVisible();
    await expect(page.getByRole("link", { name: "Image update available" })).toHaveCount(1);

    await expect.poll(() => hook.alerts().length, { timeout: 30_000 }).toBe(1);
    expect(hook.alerts()[0].message).toMatch(/^Survival: a newer version of its container image is available\./);
    expect(JSON.stringify(hook.alerts()[0])).not.toContain("itzg");
    // Checking again finds the same version, which was already announced.
    await openServer(page, "Survival");
    await page.waitForTimeout(10_000);
    await checkNow(page, "A newer image is available.");
    await page.waitForTimeout(3_000);
    expect(hook.alerts()).toHaveLength(1);

    await updateSurvival(page);
    await page.goto("/");
    await expect(serverRow(page, "Survival")).toBeVisible();
    await expect(page.getByRole("link", { name: "Image update available" })).toHaveCount(0);
    await openServer(page, "Survival");
    await checkNow(page, "This server runs the newest image its tag points to.");
    await expect(page.locator("#image-check .alert")).toHaveCount(0);
  } finally {
    await hook.close();
  }
});

// A registry that refuses is not an alert: the Update tab says so quietly.
test("a registry that refuses the check", async ({ page }) => {
  await registry({ status: "401" });
  try {
    await openServer(page, "Survival");
    await checkNow(page, "The last check couldn’t reach the registry, or the registry refused it.");
    await expect(page.locator("#image-check .alert")).toHaveCount(0);
  } finally {
    await registry({ status: "0" });
  }
});

// Turning the checks off hides what they found until they are on again.
test("turning image update checks off", async ({ page }) => {
  await registry({ id: "sha256:" + "4".repeat(64) });
  await openServer(page, "Survival");
  await checkNow(page, "A newer image is available.");

  const save = async (on) => {
    await page.goto("/settings");
    const form = page.locator("#image-update-settings-form");
    await form.getByLabel("Check for newer server images").setChecked(on);
    await form.getByRole("button", { name: "Save image update settings" }).click();
    await expect(form.locator(".alert--success")).toHaveText(`Image update checks turned ${on ? "on" : "off"}.`);
  };
  await save(false);
  await page.goto("/");
  await expect(serverRow(page, "Survival")).toBeVisible();
  await expect(page.getByRole("link", { name: "Image update available" })).toHaveCount(0);
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Update" }).click();
  await expect(page.locator("#image-check")).toContainText("Checks for newer images are off.");
  await expect(page.locator("#image-check").getByRole("button", { name: "Check now" })).toHaveCount(0);

  await save(true);
  await page.goto("/");
  await expect(serverRow(page, "Survival").getByRole("link", { name: "Image update available" })).toBeVisible();
});
