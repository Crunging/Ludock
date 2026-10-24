import { test, expect, asAdmin, openServer, expectOperationSucceeded } from "./fixtures.mjs";

asAdmin();

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
