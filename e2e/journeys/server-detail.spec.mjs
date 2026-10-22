import { test, expect, asAdmin, openServer, expectOperationSucceeded } from "./fixtures.mjs";

asAdmin();

test("reviewing a server, setting up monitoring, and following a restart", async ({ page }) => {
  await openServer(page, "Survival");
  const facts = page.locator(".overview-facts");
  await expect(facts).toContainText("itzg/minecraft-server:java21");
  await expect(facts).toContainText("25565/tcp");
  await expect(page.locator("#availability-title + p")).toHaveText("Monitoring disabled.");

  await page.getByLabel("Monitor this server").check();
  await page.getByLabel("Failure grace period (seconds)").fill("180");
  await page.getByRole("button", { name: "Save monitoring" }).click();
  await expect(page.locator("#availability-result")).toHaveText("Monitoring settings saved.");
  await expect(page.locator("#availability-title + p")).not.toHaveText("Monitoring disabled.");

  await page.locator(".detail-actions").getByRole("button", { name: "Restart" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Restart server" }).click();
  await expectOperationSucceeded(page);
  await expect(page.locator("#activity-list")).toContainText("Save command sent before stopping.");

  await page.getByLabel("Status").selectOption("failed");
  await expect(page.locator(".activity-filter-count")).toHaveText(/Showing 0 of \d+ recent operations\./);
  await expect(page).toHaveURL(/status=failed/);
});
