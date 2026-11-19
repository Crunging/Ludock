import { test, expect, admin, asAdmin, signIn, serverRow, openServer, expectOperationSucceeded, createUser } from "./fixtures.mjs";

asAdmin();
// Each journey here passes on its own, so they spread across workers.
test.describe.configure({ mode: "parallel" });

test("searching operation history and the audit log", async ({ page, session }) => {
  await openServer(page, "Survival");
  await page.locator(".detail-actions").getByRole("button", { name: "Restart" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Restart server" }).click();
  await expectOperationSucceeded(page);

  await page.goto("/operations");
  const rows = page.locator(".data-table tbody tr");
  await expect(rows.first()).toContainText("Survival");
  await page.getByLabel("Status").selectOption("succeeded");
  await page.getByRole("button", { name: "Search" }).click();
  await expect(page).toHaveURL(/\/operations\?status=succeeded$/);

  await page.getByLabel("From", { exact: true }).fill("2030-01-01T00:00");
  await page.getByLabel("To", { exact: true }).fill("2020-01-01T00:00");
  await page.getByRole("button", { name: "Search" }).click();
  await expect(page.locator("[data-range-error]")).toHaveText("From must be before or equal to To.");

  await page.goto("/operations");
  await rows.first().getByRole("link").first().click();
  await expect(page.getByRole("heading", { name: "Operation details" })).toBeVisible();
  await expect(page.locator(".operation-detail-summary")).toContainText("Succeeded");
  await page.getByRole("link", { name: "Related audit events" }).click();
  await expect(page.locator(".audit-entry").first()).toContainText(".succeeded");

  // A sign-in on another device, for the audit log to find.
  const other = await session({ storageState: { cookies: [], origins: [] } });
  await signIn(other, admin.username, admin.password);
  await expect(other.getByRole("heading", { name: "Servers", exact: true })).toBeVisible();
  await page.goto("/audit?action=auth.login");
  await expect(page.locator(".audit-entry").first()).toContainText("auth.login");
});

test("reading Ludock's logs and diagnostics", async ({ page }) => {
  await page.goto("/logs");
  await expect(page.locator(".application-log").first()).toBeVisible();
  await page.getByLabel("Search logs").fill("Ludock listening");
  await expect(page.locator(".application-log")).toHaveCount(1);
  await expect(page).toHaveURL(/q=Ludock\+listening/);

  // The log list stops polling while the tab is hidden and resumes once
  // it is shown again.
  let polls = 0;
  page.on("request", (request) => { if (request.url().includes("/fragments/logs")) polls++; });
  const setHidden = (hidden) => page.evaluate((value) => {
    Object.defineProperty(document, "hidden", { configurable: true, get: () => value });
    document.dispatchEvent(new Event("visibilitychange"));
  }, hidden);
  await setHidden(true);
  await page.waitForTimeout(4500);
  expect(polls).toBe(0);
  await setHidden(false);
  await expect.poll(() => polls, { timeout: 5000 }).toBeGreaterThan(0);

  // A poll that answers late, for the filter before it, leaves a newer filter in place.
  await page.getByLabel("Search logs").fill("");
  await expect(page.locator(".application-log").nth(1)).toBeVisible();
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  let holding = false;
  await page.route(/\/fragments\/logs\?.*since=/, async (route) => {
    if (holding) return route.continue();
    holding = true;
    await held;
    // Without the buffer state it names, the poll is answered in full.
    const address = new URL(route.request().url());
    address.searchParams.delete("since");
    await route.continue({ url: address.toString() });
  });
  await expect.poll(() => holding).toBe(true);
  const late = page.waitForResponse((response) => /\/fragments\/logs\?/.test(response.url()) && !response.url().includes("since=") &&
    response.request().headers()["hx-source"] === "div#application-logs");
  await page.getByLabel("Search logs").fill("Ludock listening");
  await page.waitForTimeout(600);
  release();
  await late;
  await expect(page.locator(".application-log")).toHaveCount(1);
  await page.waitForTimeout(300);
  await expect(page.locator(".application-log")).toHaveCount(1);
  await page.unroute(/\/fragments\/logs\?.*since=/);

  await page.goto("/diagnostics");
  await expect(page.locator(".metadata-list")).toContainText("Docker EngineConnected");
  await expect(page.getByRole("cell", { name: "Minecraft", exact: true })).toBeVisible();
});

test("changing the public address and backup limits", async ({ page }) => {
  await page.goto("/settings");
  const connection = page.locator("#connection-form");
  await connection.getByLabel("Public address").fill("not a host!");
  await connection.getByRole("button", { name: "Save address" }).click();
  await expect(connection.locator(".alert--error")).toBeVisible();
  await connection.getByLabel("Public address").fill("Play.Example.com");
  await connection.getByRole("button", { name: "Save address" }).click();
  await expect(connection.locator(".alert--success")).toHaveText("Server address saved.");

  await page.goto("/");
  await expect(serverRow(page, "Survival")).toContainText("play.example.com:25565");

  // Backup storage set itself up: the page says where backups go and what
  // they use, and keeps the folder and limits under Advanced.
  await page.goto("/settings");
  const backups = page.locator("#backup-settings-form");
  const storage = backups.locator("#backup-storage");
  await expect(storage).toContainText("Saved in");
  await expect(storage).toContainText("Backups use");
  await expect(storage).toContainText("Free on disk");
  await expect(storage).not.toContainText("Total limit");
  await expect(backups.getByLabel("Backup folder")).toBeHidden();
  await backups.getByText("Advanced").click();
  await expect(backups.getByLabel("Total limit for all backups (GiB)")).toHaveValue("");
  await backups.getByLabel("Total limit for all backups (GiB)").fill("2.5");
  await backups.getByRole("button", { name: "Save backup settings" }).click();
  await expect(backups.locator(".alert--success")).toContainText("Backup settings saved.");
  await expect(storage).toContainText("Total limit2.5 GiB");
  await backups.getByText("Advanced").click();
  await expect(backups.getByLabel("Total limit for all backups (GiB)")).toHaveValue("2.5");
  await backups.getByLabel("Total limit for all backups (GiB)").fill("");
  await backups.getByRole("button", { name: "Save backup settings" }).click();
  await expect(storage).not.toContainText("Total limit");

  // The long backup folder path wraps instead of widening a phone screen.
  await page.setViewportSize({ width: 375, height: 800 });
  await page.reload();
  await expect(storage).toContainText("Saved in");
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(375);
  await page.setViewportSize({ width: 1280, height: 800 });

  // Restore the default address so other journeys see the page's own host.
  await page.locator("#connection-form").getByLabel("Public address").fill("");
  await page.locator("#connection-form").getByRole("button", { name: "Save address" }).click();
  await expect(page.locator("#connection-form .alert--success")).toHaveText("Server address cleared.");
});

test("a role changed while a deletion is answering stays changed", async ({ page }) => {
  await createUser(page, "alice", "alice-password-123", "viewer");
  await createUser(page, "bob", "bob-password-1234", "viewer");
  // Alice's deletion is done on the server, but its answer arrives last.
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  await page.route(/\/users\/[^/]+\/delete$/, async (route) => {
    const response = await route.fetch();
    await held;
    await route.fulfill({ response });
  });
  const alice = page.locator(".user-row", { hasText: "alice" });
  await alice.getByRole("button", { name: "Delete" }).click();
  await page.getByRole("dialog", { name: "Delete alice?" }).getByRole("button", { name: "Delete user" }).click();
  const promoted = page.waitForResponse(/\/users\/[^/]+\/access$/);
  await page.getByLabel("Role for bob").selectOption("operator");
  await promoted;
  const deleted = page.waitForResponse(/\/users\/[^/]+\/delete$/);
  release();
  await deleted;
  await expect(page.getByLabel("Role for bob")).toHaveValue("operator");
});
