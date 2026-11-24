import { spawn } from "node:child_process";
import { once } from "node:events";
import { createServer } from "node:http";
import { existsSync, mkdtempSync, readdirSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { test, expect, createUser, signIn, shareServer } from "./fixtures.mjs";
import { freshStack } from "../setup.mjs";

// Other files leave their worker's install set up, so these journeys start
// an install of their own, just past creating the first administrator, with
// a folder mounted for second copies of backups.
let stack;
test.beforeAll(async ({}, info) => {
  stack = await freshStack(info.parallelIndex, { found: true });
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

test("members never see the setup list, and a member with one server sees no filters", async ({ session }) => {
  const admin = await adminPage(session);
  await createUser(admin, "friend", "friend password one");
  await admin.goto("/");
  await expect(admin.getByRole("region", { name: "Finish setting up" })).toBeVisible();
  const member = await session({ baseURL: stack.env.LUDOCK_E2E_URL });
  await signIn(member, "friend", "friend password one");
  await expect(member.getByRole("heading", { level: 1, name: "Servers" })).toBeVisible();
  await expect(member.locator("#checklist")).toHaveCount(0);
  await expect(member.getByText("Finish setting up")).toHaveCount(0);

  // One shared server is the whole list: nothing to search or filter, and
  // nothing needs attention, so neither shows.
  await shareServer(admin, "Survival", "friend", "Can run it");
  await member.reload();
  await expect(member.getByRole("link", { name: "Survival", exact: true })).toBeVisible();
  await expect(member.getByPlaceholder("Find a server…")).toBeHidden();
  await expect(member.getByLabel("State")).toBeHidden();
  await expect(member.getByRole("region", { name: "Needs attention" })).toHaveCount(0);
  await expect(member.getByRole("button", { name: "Refresh" })).toHaveCount(0);
  // An administrator with more servers can filter them.
  await admin.goto("/");
  await expect(admin.getByPlaceholder("Find a server…")).toBeVisible();
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
    const backups = item(page, "Back up your servers");
    await expect(backups).toContainText("Survival and valheim have no scheduled backups. Ludock can’t back up 2 other servers (why).");
    await expect(item(page, "Check the address players use")).toContainText("Players are shown 127.0.0.1, which only works on this computer.");
    await expect(item(page, "Add the servers Ludock didn’t find")).toContainText("satisfactory looks like a game server");
    // Needs attention shows only when something is wrong.
    await expect(page.getByRole("region", { name: "Needs attention" })).toHaveCount(0);

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
    await expect(dialog).toContainText("Each running server goes offline for a few minutes during its backup, then goes back to how it was.");
    await dialog.getByRole("button", { name: "Back up every night" }).click();
    await expect(checklist.locator(".alert--success")).toHaveText(
      "Nightly backups scheduled: Survival at 04:00, valheim at 04:15 (Europe/Berlin).");
    await expect(item(page, "Back up your servers")).toHaveCount(0);
    await page.getByRole("link", { name: "Survival", exact: true }).click();
    await page.locator(".section-tabs").getByRole("link", { name: "Schedules" }).click();
    const schedule = page.locator("#schedule-list tbody tr");
    await expect(schedule).toHaveCount(1);
    await expect(schedule).toContainText("Backup");
    await expect(schedule).toContainText("04:00, every day");
    await expect(schedule).toContainText("Europe/Berlin");
    // As considerate as a schedule made by hand.
    await expect(schedule).toContainText("Waits up to 30 minutes for everyone to leave, then goes ahead");
    await expect(schedule).toContainText("Warns players 5 and 1 minutes before");

    // Automatic restart: one click, after saying what it does, turns it on
    // for each monitored server whose game can answer.
    await page.goto("/");
    const restarts = item(page, "Restart servers that stop answering");
    await expect(restarts).toContainText("enshrouded, Survival, and valheim wait for someone to restart them if their game stops answering while they run");
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
    await expect(page.getByRole("heading", { level: 1, name: "Servers" })).toBeVisible();
    await expect(page.locator("#checklist")).toBeEmpty();
  } finally {
    hooks.close();
  }
});

// savesIn lists the saved copies of Ludock's own data in folder.
const savesIn = (folder) => existsSync(folder) ? readdirSync(folder).filter((name) => name.startsWith("ludock-data-")) : [];

test("a folder mounted for second copies is used without asking, until an administrator turns it off", async ({ session }) => {
  const page = await adminPage(session);
  const found = join(stack.env.LUDOCK_E2E_DIR, "found");
  await page.goto("/settings");
  const storage = page.locator("#backup-storage");
  await expect(async () => {
    await page.reload();
    await expect(storage).toContainText(`Second copy in${found}`, { timeout: 1_000 });
  }).toPass({ timeout: 30_000 });
  await expect(storage).toContainText("Ludock found this folder mounted, so it copies every backup there.");
  // Ludock's own data is copied there too.
  await expect.poll(() => savesIn(found).length, { timeout: 45_000 }).toBeGreaterThan(0);

  // Turned off under Advanced, it stays off.
  await page.getByRole("link", { name: "Advanced" }).first().click();
  const form = page.locator("#backup-settings-form");
  await expect(form.getByLabel("Also copy backups to")).toHaveValue(found);
  await form.getByLabel("Also copy backups to").selectOption({ label: "No second copy" });
  await form.getByRole("button", { name: "Save backup settings" }).click();
  await expect(form.locator(".alert--success")).toHaveText("Backup settings saved.");
  await expect(storage).not.toContainText("Second copy in");
  // The choice is saved; the Go tests cover it holding as Ludock looks again.
  await page.reload();
  await expect(storage.locator("#ludock-data-save")).toBeVisible();
  await expect(storage).not.toContainText("Second copy in");
  await expect(page.locator("#backup-settings-form").getByLabel("Also copy backups to")).toHaveValue("");
});

// A server whose data no backup can reach says so in plain words and
// offers no backup anywhere: not on its Backups tab, in the Restart dialog,
// before an update, or as a schedule. The server list doesn't warn about it.
test("a server with no data folders says why it can't be backed up", async ({ session }) => {
  const page = await adminPage(session);
  await page.goto("/");
  const row = page.locator(".server-row", { has: page.getByRole("link", { name: "enshrouded", exact: true }) });
  await expect(row.locator(".server-row__backup")).not.toContainText("Never");
  await expect(row.locator(".server-row__never")).toHaveCount(0);
  await row.getByRole("link", { name: "enshrouded", exact: true }).click();
  await page.locator(".section-tabs").getByRole("link", { name: "Backups" }).click();
  const why = page.locator("#backup-no-data");
  await expect(why).toContainText("Ludock can’t back up this server. None of its data is in a folder Ludock can reach.");
  await expect(why).toContainText("give its game data a writable volume or bind mount");
  await expect(page.getByRole("button", { name: "Create backup" })).toHaveCount(0);
  await expect(page.locator("main")).not.toContainText("Ludock found no data folder it can back up");

  await page.locator(".section-tabs").getByRole("link", { name: "Overview" }).click();
  await expect(page.locator(".overview-facts").first()).not.toContainText("Last backup");
  await page.locator(".section-tabs").getByRole("link", { name: "Schedules" }).click();
  await expect(page.locator("select[name=action] option[value=backup]")).toHaveCount(0);
  await expect(page.getByLabel("Back up first")).toHaveCount(0);
  await page.locator(".section-tabs").getByRole("link", { name: "Update" }).click();
  const first = page.locator("#update-backup");
  await expect(first).toBeDisabled();
  await expect(first).not.toBeChecked();
  await expect(page.locator("#update-panel")).toContainText("This server has no data folders Ludock can reach, so there is nothing to back up first.");
  await expect(page.locator("#update-panel")).toContainText("to update without a backup");
});

test("starting on a beta's data says to start fresh, and leaves the data as it was", async ({ page }) => {
  const dir = mkdtempSync(join(tmpdir(), "ludock-e2e-beta-"));
  const database = join(dir, "ludock.db");
  // A database a 0.6 beta wrote: its marker, and a version 1.0 also has.
  const beta = new DatabaseSync(database);
  beta.exec("CREATE TABLE users (id TEXT PRIMARY KEY); PRAGMA application_id = 1279542091; PRAGMA user_version = 1;");
  beta.close();
  const before = readFileSync(database);
  const probe = createServer().listen(0, "127.0.0.1");
  await once(probe, "listening");
  const port = probe.address().port;
  probe.close();
  const ludock = spawn(join(process.env.LUDOCK_E2E_BIN, "ludock"), ["serve"], {
    env: { ...process.env, LUDOCK_LISTEN: `127.0.0.1:${port}`, LUDOCK_DATA_DIR: dir,
      LUDOCK_DOCKER_SOCKET: join(dir, "no-docker.sock") }, stdio: ["ignore", "pipe", "pipe"] });
  let log = "";
  ludock.stdout.on("data", (chunk) => (log += chunk));
  ludock.stderr.on("data", (chunk) => (log += chunk));
  try {
    await expect(async () => {
      expect((await page.goto(`http://127.0.0.1:${port}/`)).status()).toBe(503);
    }).toPass({ timeout: 15_000 });
    await expect(page.locator("body")).toContainText(
      "This data is from a Ludock beta, and Ludock 1.0 starts fresh, so it can’t use it. Start Ludock with a new, empty data volume");
    await expect(page.locator("body")).toContainText("your game servers and your backup archives are not affected");
    // It waits with the message instead of exiting, which would have its
    // container restarted over and over, and says why in its log once.
    await page.waitForTimeout(1_000);
    expect(ludock.exitCode).toBeNull();
    expect(log.split("This data is from a Ludock beta").length).toBe(2);
    expect(readFileSync(database).equals(before)).toBe(true);
    expect(readdirSync(dir)).toEqual(["ludock.db"]);
  } finally {
    ludock.kill("SIGTERM");
    if (ludock.exitCode === null) await once(ludock, "exit");
    rmSync(dir, { recursive: true, force: true });
  }
});
