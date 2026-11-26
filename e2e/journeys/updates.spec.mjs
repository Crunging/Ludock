import { createServer } from "node:http";
import { request } from "node:http";
import { join } from "node:path";
import { test, expect, asAdmin, admin, openServer, serverRow, expectOperationSucceeded, fake } from "./fixtures.mjs";

asAdmin();

/**
 * Points image's tag in the fake daemon's registry at a new image ID, or
 * makes its lookups fail with status. Each journey updates a server of its
 * own, whose tag no other server uses.
 */
function registry(image, query) {
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
  await page.locator(".section-tabs").getByRole("link", { name: "Update" }).click();
  const check = page.locator("#image-check");
  await expect(async () => {
    await check.getByRole("button", { name: "Check now" }).click();
    await expect(check).toContainText(text, { timeout: 2_000 });
  }).toPass({ timeout: 30_000, intervals: [1_000, 3_000] });
}

/** Starts an update without a backup, confirming in the dialog. */
async function startUpdate(page, server) {
  const form = page.locator("#update-panel form");
  await form.getByLabel("Back up first").uncheck();
  await form.getByLabel(`Type ${server} to skip the backup`).fill(server);
  await form.getByRole("button", { name: "Update server" }).click();
  await page.getByRole("dialog", { name: `Update ${server}?` }).getByRole("button", { name: "Update server" }).click();
}

async function update(page, server) {
  await startUpdate(page, server);
  await expectOperationSucceeded(page);
}

// The fake daemon offers a newer image on the first pull. Updating needs no
// setup: Ludock recreates the container through Docker alone.
test("updating a server's image, then finding it current", async ({ page, ownServer }) => {
  await openServer(page, ownServer.name);
  await page.locator(".section-tabs").getByRole("link", { name: "Update" }).click();
  const form = page.locator("#update-panel form");
  await expect(form.locator(".metadata-list")).toContainText(ownServer.image);
  await form.getByText("What gets replaced").click();
  await expect(form.locator(".disclosure").first()).toContainText("including unnamed ones");
  await expect(form.getByLabel("Back up first")).toBeChecked();
  await expect(form.getByLabel("Update even if the image is already up to date")).toBeHidden();
  // Cancelling the confirmation starts nothing.
  await form.getByRole("button", { name: "Update server" }).click();
  const confirm = page.getByRole("dialog", { name: `Update ${ownServer.name}?` });
  await expect(confirm).toContainText("Connected players will be disconnected. Ludock replaces the server with the newest image.");
  await confirm.getByRole("button", { name: "Cancel" }).click();
  await expect(page).not.toHaveURL(/[?&]operation=/);
  await update(page, ownServer.name);
  await expect(page.locator(".detail-actions").getByRole("button", { name: "Stop" })).toBeEnabled();

  // The replacement is the same server, now running the pulled image.
  await page.locator(".section-tabs").getByRole("link", { name: "Update" }).click();
  await startUpdate(page, ownServer.name);
  await expect(page.locator("#selected-operation")).toContainText("Already up to date", { timeout: 45_000 });
  await page.getByRole("link", { name: "Update anyway" }).click();
  await expect(form.getByLabel("Update even if the image is already up to date")).toBeChecked();
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
  if (!(await card.evaluate((details) => details.open))) await card.locator(":scope > summary").click();
  await card.locator("input[name=url]").fill(`http://127.0.0.1:${server.address().port}/hook`);
  await card.getByLabel("Send alerts to the webhook").check();
  // Destinations get newer images only once that type is chosen.
  await card.locator(".notification-alerts summary").click();
  await card.getByLabel("Newer images available").check();
  await card.getByRole("button", { name: "Save Webhook" }).click();
  await expect(card.locator("form .alert--success")).toHaveText("Webhook settings saved.");
  return {
    alerts: () => bodies.filter((body) => body.message.includes(": a newer image is available.")),
    all: () => bodies,
    ludockAlerts: () => bodies.filter((body) => body.message.startsWith("Ludock: a newer version is available.")),
    close: async () => {
      await page.goto("/settings");
      if (!(await card.evaluate((details) => details.open))) await card.locator(":scope > summary").click();
      await card.getByLabel("Send alerts to the webhook").uncheck();
      await card.locator(".notification-alerts summary").click();
      await card.getByLabel("Newer images available").uncheck();
      await card.getByRole("button", { name: "Save Webhook" }).click();
      await expect(card.locator("form .alert--success")).toHaveText("Webhook settings saved.");
      server.close();
    },
  };
}

// A new version of the tag appears in the registry. Administrators see it on
// the list and the Update tab, a destination that chose newer images is told
// once, and updating
// clears it.
test("finding a newer image, being told once, and updating to it", async ({ page, ownServer }) => {
  // Manual checks are 10 seconds apart, and this one waits that out twice.
  test.slow();
  const hook = await webhook(page);
  try {
    await registry(ownServer.image, { id: "sha256:" + "3".repeat(64) });
    await openServer(page, ownServer.name);
    await checkNow(page, "A newer image is available.");
    const check = page.locator("#image-check");
    // Minecraft's image downloads the game on start, so the notice says which
    // update this is.
    await expect(check.locator(".alert")).toContainText("The game already updates on every restart.");
    await expect(check).toContainText("Last checked");

    await page.goto("/");
    await expect(serverRow(page, ownServer.name).getByRole("link", { name: "Update available" })).toBeVisible();
    await expect(page.getByRole("link", { name: "Update available" })).toHaveCount(1);

    await expect.poll(() => hook.alerts().length, { timeout: 30_000 }).toBe(1);
    expect(hook.alerts()[0].message).toMatch(new RegExp(`^${ownServer.name}: a newer image is available\\.`));
    expect(JSON.stringify(hook.alerts()[0])).not.toContain("itzg");
    // Checking again finds the same version, which was already announced.
    await openServer(page, ownServer.name);
    await page.waitForTimeout(10_000);
    await checkNow(page, "A newer image is available.");
    await page.waitForTimeout(3_000);
    expect(hook.alerts()).toHaveLength(1);

    await update(page, ownServer.name);
    await page.goto("/");
    await expect(serverRow(page, ownServer.name)).toBeVisible();
    await expect(page.getByRole("link", { name: "Update available" })).toHaveCount(0);
    await openServer(page, ownServer.name);
    await checkNow(page, "This server runs the newest image its tag points to.");
    await expect(page.locator("#image-check .alert")).toHaveCount(0);
    // An update started in the panel shows its result there; only a failure
    // would be pushed.
    expect(hook.all().filter((body) => body.message.includes("update completed"))).toEqual([]);
  } finally {
    await hook.close();
  }
});

// An owner who pulls a newer image with their own tool, such as
// `docker compose pull`, without recreating the server keeps it in Ludock,
// though Docker then lists the container by its image's ID.
test("pulling a newer image outside Ludock keeps the server", async ({ page, ownServer }) => {
  await registry(ownServer.image, { id: "sha256:" + "5".repeat(64) });
  await fake("/images/create", { fromImage: ownServer.image });
  await page.goto("/");
  await expect(serverRow(page, ownServer.name)).toContainText("Running");
  await expect(page.getByText("Server missing in Docker")).toHaveCount(0);
  await openServer(page, ownServer.name);
  await page.locator(".section-tabs").getByRole("link", { name: "Update" }).click();
  await update(page, ownServer.name);
});

// A registry that refuses is not an alert: the Update tab says so quietly.
test("a registry that refuses the check", async ({ page, ownServer }) => {
  await registry(ownServer.image, { status: "401" });
  try {
    await openServer(page, ownServer.name);
    await checkNow(page, "The last check couldn’t reach the registry, or the registry refused it.");
    await expect(page.locator("#image-check .alert")).toHaveCount(0);
  } finally {
    await registry(ownServer.image, { status: "0" });
  }
});

// Turning the checks off hides what they found until they are on again.
test("turning image update checks off", async ({ page, ownServer }) => {
  await registry(ownServer.image, { id: "sha256:" + "4".repeat(64) });
  await openServer(page, ownServer.name);
  await checkNow(page, "A newer image is available.");

  const save = async (on) => {
    await page.goto("/settings");
    await page.locator("#advanced-settings > summary").click();
    const form = page.locator("#image-update-settings-form");
    await form.getByLabel("Check for newer server images").setChecked(on);
    await expect(form.locator(".alert--success")).toHaveText(`Image update checks turned ${on ? "on" : "off"}.`);
  };
  try {
    await save(false);
    await page.goto("/");
    await expect(serverRow(page, ownServer.name)).toBeVisible();
    await expect(page.getByRole("link", { name: "Update available" })).toHaveCount(0);
    await openServer(page, ownServer.name);
    await page.locator(".section-tabs").getByRole("link", { name: "Update" }).click();
    await expect(page.locator("#image-check")).toContainText("Checks for newer images are off.");
    await expect(page.locator("#image-check").getByRole("button", { name: "Check now" })).toHaveCount(0);
  } finally {
    // The setting is the whole install's.
    await save(true);
  }
  await page.goto("/");
  await expect(serverRow(page, ownServer.name).getByRole("link", { name: "Update available" })).toBeVisible();
});

// Ludock checks its own image as it checks a server's. Pinned by digest, as
// the fake daemon starts it, there is nothing to update to, and Settings
// says so. Once it runs from a tag that moves, Settings offers to update it
// and a destination that chose newer images is told once. Updating replaces
// its container through a helper, which the fake daemon runs in place of the
// container, and Activity records it. A version that fails is put back and
// stays in Needs attention until dismissed.
test("finding a newer Ludock and updating it", async ({ page }) => {
  test.setTimeout(240_000);
  const pinned = "ghcr.io/crunging/ludock@sha256:" + "ab".repeat(32);
  const tagged = "ghcr.io/crunging/ludock:latest";
  const check = page.locator("#ludock-check");
  /** Reloads Settings until the Ludock check shows text, pressing Check now when it can. */
  const checkLudock = async (text) => {
    await expect(async () => {
      await page.goto("/settings");
      await page.locator("#advanced-settings > summary").click();
      await check.getByRole("button", { name: "Check now" }).click({ timeout: 2_000 });
      await expect(check).toContainText(text, { timeout: 2_000 });
    }).toPass({ timeout: 45_000, intervals: [1_000, 3_000] });
  };
  const pinnedText = "Ludock’s image is pinned to a digest or image ID, so there is no newer version to update to.";
  await page.goto("/settings");
  await expect(page.locator("#image-update-settings-form")).toBeAttached();
  await expect(check).toContainText(pinnedText);
  const notice = page.locator("#ludock-notice");
  await expect(notice).toBeEmpty();

  const hook = await webhook(page);
  await fake("/fake/recreate", { container: "ludock-dev", image: tagged });
  try {
    await checkLudock("Ludock runs the newest image its tag points to.");
    await expect(check.locator(".alert")).toHaveCount(0);
    await fake("/fake/registry", { image: tagged, id: "sha256:" + "5".repeat(64) });
    await checkLudock("A newer Ludock is available.");
    // The everyday tab leads with the button that updates it.
    await expect(notice.getByRole("button", { name: "Update Ludock" })).toBeVisible();
    await expect(notice.locator("pre")).toHaveCount(0);
    await expect(check).toContainText("Last checked");
    await expect(check.getByRole("button")).toHaveText(["Check now"]);
    await expect.poll(() => hook.ludockAlerts().length, { timeout: 30_000 }).toBe(1);
    expect(JSON.stringify(hook.ludockAlerts()[0])).not.toContain("ghcr.io");

    // The server checks' setting turns this one off and on too.
    const form = page.locator("#image-update-settings-form");
    await form.getByLabel("Check for newer server images").uncheck();
    await expect(form.locator(".alert--success")).toHaveText("Image update checks turned off.");
    await expect(check).toBeEmpty();
    await expect(notice).toBeEmpty();
    await form.getByLabel("Check for newer server images").check();
    await expect(form.locator(".alert--success")).toHaveText("Image update checks turned on.");
    await expect(check).toContainText("A newer Ludock is available.");

    await notice.getByRole("button", { name: "Update Ludock" }).click();
    const confirm = page.getByRole("dialog", { name: "Update Ludock?" });
    await expect(confirm).toContainText("Your game servers keep running, and this page comes back by itself once it has, usually within a few minutes.");
    await confirm.getByRole("button", { name: "Update Ludock" }).click();
    await expect(notice).toContainText("Ludock is updating.");
    await expect(notice.locator("[data-await-restart]")).toBeAttached();
    await expect(notice).toContainText("Ludock was updated to version dev.", { timeout: 30_000 });
    await expect(notice.getByRole("button", { name: "Update Ludock" })).toHaveCount(0);
    await page.goto("/activity?show=everything&action=Updated Ludock");
    const entry = page.locator(".audit-entry").first();
    await expect(entry).toContainText("Updated Ludock from version dev to version dev");
    await expect(entry).toContainText("Succeeded");
    await expect(entry).toContainText(admin.username);
    // The outcome shows until the next check.
    await checkLudock("Ludock runs the newest image its tag points to.");
    await expect(notice).toBeEmpty();

    // A version whose health check fails is put back, said where the update
    // was offered and in Needs attention until dismissed.
    await fake("/fake/registry", { image: tagged, id: "sha256:" + "b".repeat(64) });
    await checkLudock("A newer Ludock is available.");
    await notice.getByRole("button", { name: "Update Ludock" }).click();
    await page.getByRole("dialog", { name: "Update Ludock?" }).getByRole("button", { name: "Update Ludock" }).click();
    const failure = "Ludock’s update didn’t finish: the new version reported itself unhealthy, as one whose image is older than this Ludock’s data does. Ludock put version dev back, with its data as it was.";
    await expect(notice.locator(".alert--error")).toContainText(failure, { timeout: 30_000 });
    await expect(notice.locator("pre")).toHaveText("docker compose pull ludock\ndocker compose up -d ludock");
    await expect.poll(() => hook.all().filter((body) => body.message.startsWith("Ludock couldn’t update itself:")).length, { timeout: 30_000 }).toBe(1);
    await page.goto("/activity?show=everything&action=Updated Ludock");
    await expect(entry).toContainText("Updated Ludock from version dev, put back: the new version reported itself unhealthy");
    await expect(entry).toContainText("Failed");
    await expect(entry).toContainText(admin.username);
    await page.goto("/");
    const item = page.locator(".needs-attention__list li", { hasText: "Ludock’s update didn’t finish" });
    await expect(item).toContainText("The new version reported itself unhealthy, as one whose image is older than this Ludock’s data does, so Ludock is still on version dev.");
    await item.getByRole("link", { name: /^Open Settings/ }).click();
    await notice.getByRole("button", { name: "Dismiss" }).click();
    await expect(notice.locator(".alert--error")).toHaveCount(0);
    await page.goto("/");
    await expect(item).toHaveCount(0);
  } finally {
    await fake("/fake/recreate", { container: "ludock-dev", image: pinned });
    await hook.close();
  }
  await page.goto("/settings");
  await expect(check).toContainText(pinnedText);
});
