import { test, expect, admin, asAdmin, signIn, serverRow, openServer, expectOperationSucceeded, createUser } from "./fixtures.mjs";

asAdmin();
// Each journey here passes on its own, so they spread across workers.
test.describe.configure({ mode: "parallel" });

test("searching Activity, with sign-ins under Everything", async ({ page, session }) => {
  await openServer(page, "Survival");
  await page.locator(".detail-actions").getByRole("button", { name: "Restart" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Restart server" }).click();
  await expectOperationSucceeded(page);

  await page.getByRole("navigation", { name: "Primary navigation" }).getByRole("link", { name: "Activity" }).click();
  await expect(page).toHaveURL(/\/activity$/);
  await expect(page.getByRole("heading", { level: 1, name: "Activity" })).toBeVisible();
  const rows = page.locator(".data-table tbody tr");
  await expect(rows.first()).toContainText("Survival");
  // The filters wait behind Filter until one is in use.
  const filter = page.locator("#history-filter");
  await expect(page.getByLabel("Result")).toBeHidden();
  await expect(filter.locator("summary")).toHaveText("Filter");
  await filter.locator("summary").click();
  await page.getByLabel("Result").selectOption("succeeded");
  await page.getByRole("button", { name: "Search" }).click();
  await expect(page).toHaveURL(/\/activity\?status=succeeded$/);
  // A filtered address opens with them showing and says how many are on.
  await expect(filter).toHaveAttribute("open");
  await expect(filter.locator("summary")).toHaveText(/^Filter\s*1 on$/);
  await expect(page.getByLabel("Result")).toHaveValue("succeeded");
  await page.getByRole("link", { name: "Clear filters" }).click();
  await expect(page).toHaveURL(/\/activity$/);
  await expect(filter).not.toHaveAttribute("open");
  await expect(page.getByRole("button", { name: "Clear filters" })).toHaveCount(0);
  await filter.locator("summary").click();

  await page.getByLabel("From", { exact: true }).fill("2030-01-01T00:00");
  await page.getByLabel("To", { exact: true }).fill("2020-01-01T00:00");
  await page.getByRole("button", { name: "Search" }).click();
  await expect(page.locator("[data-range-error]")).toHaveText("The start date must be before the end date.");

  await page.goto("/activity");
  await rows.first().getByRole("link").first().click();
  await expect(page.getByRole("heading", { name: "Activity details" })).toBeVisible();
  await expect(page.locator(".operation-detail-summary")).toContainText("Succeeded");
  await page.getByRole("link", { name: "Everything recorded about it" }).click();
  await expect(page.getByLabel("Show")).toHaveValue("everything");
  // One entry for the restart, with its outcome.
  await expect(page.locator(".audit-entry")).toHaveCount(1);
  await expect(page.locator(".audit-entry").first()).toContainText("Restarted the server");
  await expect(page.locator(".audit-entry").first()).toContainText("Succeeded");

  // A sign-in on another device shows only under Everything.
  const other = await session({ storageState: { cookies: [], origins: [] } });
  await signIn(other, admin.username, admin.password);
  await expect(other.getByRole("heading", { name: "Servers", exact: true })).toBeVisible();
  await page.goto("/activity");
  await expect(page.getByText("Signed in")).toHaveCount(0);
  await page.locator("#history-filter summary").click();
  await page.getByLabel("Show").selectOption("everything");
  await page.getByLabel("What").fill("login");
  await page.getByRole("button", { name: "Search" }).click();
  await expect(page.locator(".audit-entry").first()).toContainText("Signed in");
  await expect(page.locator(".audit-entry").first()).toContainText(admin.username);

  // The addresses Ludock no longer uses are gone.
  for (const old of ["/operations", "/audit", "/logs", "/diagnostics"]) {
    expect((await page.request.get(old)).status(), old).toBe(404);
  }
});

test("checking the Docker connection under Settings", async ({ page }) => {
  const menu = page.getByRole("navigation", { name: "Primary navigation" });
  await page.goto("/");
  await expect(menu.getByRole("link")).toHaveText(["Servers", "Activity", "Users", "Settings"]);
  await menu.getByRole("link", { name: "Settings" }).click();
  await page.getByRole("navigation", { name: "Settings pages" }).getByRole("link", { name: "System" }).click();
  await expect(page).toHaveURL(/\/settings\/system$/);
  await expect(menu.getByRole("link", { name: "Settings" })).toHaveAttribute("aria-current", "location");
  await expect(page.locator(".metadata-list")).toContainText("DockerConnected");
  // A build without a release version says so.
  await expect(page.locator(".metadata-list")).toContainText("Versiondev");
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

  // Backup storage set itself up: the everyday tab says where backups go
  // and what they use, with the address and alerts; everything else waits
  // under one Advanced area.
  await page.goto("/settings");
  await expect(page.locator(".settings-page h2")).toHaveText(["Server address", "Notifications", "Backups", "Advanced"]);
  await expect(page.locator("details.disclosure:not(.notification-alerts)")).toHaveCount(1);
  const backups = page.locator("#backup-settings-form");
  const storage = page.locator("#backup-storage");
  await expect(storage).toContainText("Saved in");
  await expect(storage).toContainText("Backups use");
  await expect(storage).toContainText("Free on disk");
  await expect(storage).not.toContainText("Total limit");
  await expect(storage.getByRole("button")).toHaveCount(0);
  for (const hidden of [backups.getByLabel("Backup folder"), page.getByLabel("Check for newer server images"),
    page.locator("#upload-limit-form"), page.locator("#notification-deliveries")]) await expect(hidden).toBeHidden();
  await page.locator("#advanced-settings > summary").click();
  const limit = backups.getByLabel("Total limit for all backups (GiB)");
  const save = backups.getByRole("button", { name: "Save backup settings" });
  // Each section's Save waits for a change to it.
  await expect(limit).toHaveValue("");
  await expect(save).toBeDisabled();
  // A half-typed number is refused, never saved: Save offers to save it so
  // the browser can say what's wrong.
  await limit.pressSequentially("-");
  await expect(save).toBeEnabled();
  await save.click();
  expect(await limit.evaluate((field) => field.validationMessage)).not.toBe("");
  await limit.fill("2.5");
  await save.click();
  await expect(backups.locator(".alert--success")).toContainText("Backup settings saved.");
  await expect(storage).toContainText("Total limit2.5 GiB");
  await expect(save).toBeDisabled();
  await page.reload();
  await page.locator("#advanced-settings > summary").click();
  await expect(limit).toHaveValue("2.5");
  // Nor does one clear a saved limit.
  await limit.fill("");
  await limit.pressSequentially("-");
  await save.click();
  expect(await limit.evaluate((field) => field.validity.badInput)).toBe(true);
  await page.reload();
  await page.locator("#advanced-settings > summary").click();
  await expect(limit).toHaveValue("2.5");
  await limit.fill("");
  await save.click();
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

test("someone made an administrator while a deletion is answering stays one", async ({ page }) => {
  await createUser(page, "alice", "alice-password-123");
  await createUser(page, "bob", "bob-password-1234");
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
  const bob = page.locator(".user-row", { hasText: "bob" });
  const promoted = page.waitForResponse(/\/users\/[^/]+\/access$/);
  await bob.getByRole("button", { name: "Make administrator" }).click();
  await page.getByRole("dialog", { name: "Make bob an administrator?" }).getByRole("button", { name: "Make administrator" }).click();
  await promoted;
  const deleted = page.waitForResponse(/\/users\/[^/]+\/delete$/);
  release();
  await deleted;
  await expect(bob.getByRole("button", { name: "Make member" })).toBeVisible();
  await expect(bob).toContainText("Administrator · every server and setting");
});
