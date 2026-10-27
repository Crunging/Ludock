import { test, expect, admin, asAdmin, signIn, serverRow, openServer, expectOperationSucceeded, createUser } from "./fixtures.mjs";

asAdmin();

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

  await page.goto("/settings");
  const backups = page.locator("#backup-settings-form");
  await expect(backups.locator("#backup-storage")).toContainText("Archive usage");
  await backups.getByLabel("Backups per server").fill("3");
  await backups.getByLabel("Total backup limit (GiB)").fill("2.5");
  await backups.getByRole("button", { name: "Save backup settings" }).click();
  await expect(backups.locator(".alert--success")).toContainText("Backup settings saved.");
  await expect(backups.getByLabel("Total backup limit (GiB)")).toHaveValue("2.5");

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
