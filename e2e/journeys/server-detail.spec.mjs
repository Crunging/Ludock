import { test, expect, asAdmin, openServer, expectOperationSucceeded } from "./fixtures.mjs";

asAdmin();

test("reviewing a server, setting up monitoring, and following a restart", async ({ page }) => {
  await openServer(page, "Survival");
  const facts = page.locator(".overview-facts");
  await expect(facts).toContainText("itzg/minecraft-server:java21");
  await expect(facts).toContainText("25565/tcp");
  await expect(page.locator("#availability-title + p")).toHaveText("Monitoring disabled.");

  // Monitoring settings save as they change.
  await page.getByLabel("Monitor this server").check();
  await expect(page.locator("#availability-result")).toHaveText("Monitoring settings saved.");
  await expect(page.locator("#availability-title + p")).not.toHaveText("Monitoring disabled.");
  await page.locator("#availability-result").evaluate((result) => result.replaceChildren());
  await page.getByLabel("Failure grace period (seconds)").fill("180");
  await page.getByLabel("Failure grace period (seconds)").press("Tab");
  await expect(page.locator("#availability-result")).toHaveText("Monitoring settings saved.");
  await page.reload();
  await expect(page.getByLabel("Failure grace period (seconds)")).toHaveValue("180");

  await page.locator(".detail-actions").getByRole("button", { name: "Restart" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Restart server" }).click();
  await expectOperationSucceeded(page);
  await expect(page.locator("#activity-list")).toContainText("Save command sent before stopping.");

  await page.getByLabel("Status").selectOption("failed");
  await expect(page.locator(".activity-filter-count")).toHaveText(/Showing 0 of \d+ recent operations\./);
  await expect(page).toHaveURL(/status=failed/);
});

test("switching a server's tabs keeps the page loaded", async ({ page }) => {
  await openServer(page, "Survival");
  // A full page load would drop this marker.
  await page.evaluate(() => { window.stayedOnPage = true; });
  const activity = page.getByRole("tab", { name: "Activity" });
  await activity.click();
  await expect(page.getByRole("heading", { name: "Recent operations" })).toBeVisible();
  await expect(page).toHaveURL(/\?tab=activity$/);
  await expect(activity).toHaveAttribute("aria-selected", "true");
  await expect(activity).toBeFocused();

  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page.locator("#backup-readiness")).toContainText("Ready to back up.");
  expect(await page.evaluate(() => window.stayedOnPage)).toBe(true);

  // Back and forward still land on the right tab.
  await page.goBack();
  await expect(page).toHaveURL(/\?tab=activity$/);
  await expect(page.getByRole("heading", { name: "Recent operations" })).toBeVisible();
  await page.goForward();
  await expect(page.locator("#backup-readiness")).toContainText("Ready to back up.");
});
