import { createServer } from "node:http";
import { test, expect, createUser, signIn } from "./fixtures.mjs";
import { freshStack } from "../setup.mjs";

// Other files leave their worker's install set up, so these journeys start
// an install of their own, just past creating the first administrator.
let stack;
test.beforeAll(async ({}, info) => {
  stack = await freshStack(info.parallelIndex);
});
test.afterAll(async () => {
  await stack?.stop();
});

async function adminPage(session) {
  return session({ baseURL: stack.env.LUDOCK_E2E_URL, storageState: stack.env.LUDOCK_E2E_ADMIN_STATE, timezoneId: "Europe/Berlin" });
}

function item(page, title) {
  return page.locator("#checklist li", { has: page.getByText(title, { exact: true }) });
}

test("members never see the setup list", async ({ session }) => {
  const admin = await adminPage(session);
  await createUser(admin, "friend", "friend password one");
  await admin.goto("/");
  await expect(admin.getByRole("region", { name: "Finish setting up" })).toBeVisible();
  const member = await session({ baseURL: stack.env.LUDOCK_E2E_URL });
  await signIn(member, "friend", "friend password one");
  await expect(member.getByRole("heading", { level: 1, name: "Servers" })).toBeVisible();
  await expect(member.getByRole("region", { name: "Needs attention" })).toBeVisible();
  await expect(member.locator("#checklist")).toHaveCount(0);
  await expect(member.getByText("Finish setting up")).toHaveCount(0);
});

test("a new install lists what's left to set up, and does it in a click", async ({ session }) => {
  const hooks = createServer((request, response) => response.end("{}"));
  await new Promise((done) => hooks.listen(0, "127.0.0.1", done));
  try {
    const page = await adminPage(session);
    await page.goto("/");
    const checklist = page.getByRole("region", { name: "Finish setting up" });
    await expect(checklist.locator("li strong")).toHaveText(["Get alerts", "Back up your servers", "Restart servers that stop answering",
      "Check the address players use", "Add the servers Ludock didn’t find", "Protect your sign-in"]);
    await expect(item(page, "Back up your servers")).toContainText("Survival and valheim have no scheduled backups.");
    await expect(item(page, "Check the address players use")).toContainText("Players are shown 127.0.0.1, which only works on this computer.");
    await expect(item(page, "Add the servers Ludock didn’t find")).toContainText("satisfactory looks like a game server");
    // Needs attention keeps meaning what is wrong now.
    await expect(page.getByRole("region", { name: "Needs attention" })).toContainText("Nothing right now.");

    // Alerts: the link goes to the notification settings; a destination
    // that is on clears the item.
    await item(page, "Get alerts").getByRole("link", { name: "Set up notifications" }).click();
    await expect(page).toHaveURL(/\/settings#notification-settings-title$/);
    const webhook = page.locator("#notification-webhook-card");
    await webhook.locator(":scope > summary").click();
    await webhook.getByLabel("Webhook URL").fill(`http://127.0.0.1:${hooks.address().port}/hook`);
    await webhook.getByLabel("Send alerts to the webhook").check();
    await webhook.getByRole("button", { name: "Save Webhook" }).click();
    await expect(webhook.locator("form .alert--success")).toHaveText("Webhook settings saved.");
    await page.goto("/");
    await expect(checklist.locator("li strong")).toHaveText(["Back up your servers", "Restart servers that stop answering",
      "Check the address players use", "Add the servers Ludock didn’t find", "Protect your sign-in"]);

    // Backups: one click, after saying what it does, schedules a staggered
    // nightly backup for each server that can be backed up.
    await item(page, "Back up your servers").getByRole("button", { name: "Back up every night" }).click();
    const dialog = page.getByRole("dialog", { name: "Back up 2 servers every night?" });
    await expect(dialog).toContainText("for everyone to leave");
    await expect(dialog).toContainText("Each server goes offline for a few minutes during its backup, then starts again.");
    await dialog.getByRole("button", { name: "Back up every night" }).click();
    await expect(checklist.locator(".alert--success")).toHaveText(
      "Nightly backups scheduled: Survival at 04:00, valheim at 04:15 (Europe/Berlin).");
    await expect(item(page, "Back up your servers")).toHaveCount(0);
    await page.getByRole("link", { name: "Survival", exact: true }).click();
    await page.getByRole("tab", { name: "Schedules" }).click();
    const schedule = page.locator("#schedule-list tbody tr");
    await expect(schedule).toHaveCount(1);
    await expect(schedule).toContainText("Backup");
    await expect(schedule).toContainText("04:00, every day");
    await expect(schedule).toContainText("Europe/Berlin");
    // As considerate as a schedule made by hand.
    await expect(schedule).toContainText("Waits up to 30 min for an empty server, then goes ahead");
    await expect(schedule).toContainText("Warnings: 5, 1 min before");

    // Automatic restart: one click, after saying what it does, turns it on
    // for each monitored server whose game can answer.
    await page.goto("/");
    const restarts = item(page, "Restart servers that stop answering");
    await expect(restarts).toContainText("enshrouded, Survival, and valheim stay down if their game stops answering");
    await restarts.getByRole("button", { name: "Restart automatically" }).click();
    const confirm = page.getByRole("dialog", { name: /^Restart these \d servers if their game stops answering\?$/ });
    await expect(confirm).toContainText("Ludock restarts it the way the Restart button does, at most 3 times in 6 hours.");
    await confirm.getByRole("button", { name: "Turn on automatic restart" }).click();
    await expect(checklist.locator(".alert--success")).toHaveText("Automatic restart is on for enshrouded, Survival, and valheim.");
    await expect(restarts).toHaveCount(0);
    await page.getByRole("link", { name: "Survival", exact: true }).click();
    await expect(page.getByLabel("Restart it if the game stops answering")).toBeChecked();
    await page.goto("/activity?show=everything&action=recovery");
    await expect(page.locator(".audit-entry").first()).toContainText("Turned on automatic restart");

    // The address: dismissed once, it stays dismissed.
    await page.goto("/");
    await item(page, "Check the address players use").getByRole("button", { name: "Dismiss" }).click();
    await expect(item(page, "Check the address players use")).toHaveCount(0);
    await page.reload();
    await expect(checklist.locator("li strong")).toHaveText(["Add the servers Ludock didn’t find", "Protect your sign-in"]);

    // Adding the missing server clears its item. On a phone the list of
    // containers sits below the fold, and the regions above it load after the
    // page; the link still brings it into view. Scroll anchoring is off, as
    // in Safari, so Chromium can't hide the problem.
    await page.setViewportSize({ width: 390, height: 700 });
    await page.addInitScript(() => document.addEventListener("DOMContentLoaded", () => {
      const style = document.createElement("style");
      style.textContent = "* { overflow-anchor: none !important; }";
      document.head.append(style);
    }));
    await item(page, "Add the servers Ludock didn’t find").getByRole("link", { name: "Review containers" }).click();
    const add = page.locator("#add-servers");
    await expect(add.getByRole("button", { name: "Add satisfactory" })).toBeVisible();
    await expect(page.locator("#missing-server > summary")).toBeInViewport();
    await add.getByRole("button", { name: "Add satisfactory" }).click();
    await expect(add.locator(".alert--success")).toContainText("Added satisfactory.");
    await expect(checklist.locator("li strong")).toHaveText(["Protect your sign-in"]);
    await item(page, "Protect your sign-in").getByRole("button", { name: "Dismiss" }).click();
    await expect(page.locator("#checklist")).toBeEmpty();
    await page.reload();
    await expect(page.getByRole("region", { name: "Needs attention" })).toBeVisible();
    await expect(page.locator("#checklist")).toBeEmpty();
  } finally {
    hooks.close();
  }
});
