import { backupSettingsResponseSchema, backupSettingsSchema, backupStorageResponseSchema } from "@ludock/shared";
import { test, expect } from "./fixtures";

test("backup storage uses saved limits, refreshes after save and recovers from unreadable disk space", async ({ app, page }) => {
  const gib = 1024 ** 3;
  let settings = { destination: "/backups", retentionCount: 10, maxBytes: 100 * gib, reserveBytes: 5 * gib };
  let unreadable = false;
  await page.route("**/api/v1/settings/deployment", (route) => route.fulfill({ json: {
    backupRoots: ["/backups"], composeRoots: [], composeAvailable: false,
  } }));
  await page.route("**/api/v1/notifications", (route) => route.fulfill({ json: { configured: false, enabled: false } }));
  await page.route("**/api/v1/notifications/deliveries", (route) => route.fulfill({ json: { deliveries: [] } }));
  await page.route("**/api/v1/settings/backups", async (route) => {
    if (route.request().method() === "PUT") settings = backupSettingsSchema.parse(route.request().postDataJSON());
    await route.fulfill({ json: backupSettingsResponseSchema.parse({ settings }) });
  });
  await page.route("**/api/v1/settings/backups/status", async (route) => {
    await route.fulfill({ json: backupStorageResponseSchema.parse({ storage: {
      configured: true,
      archiveBytes: 2.5 * gib,
      maxBytes: settings.maxBytes,
      reserveBytes: settings.reserveBytes,
      availableBytes: unreadable ? null : 24 * gib,
      issues: unreadable ? [{ code: "destination_unavailable", message: "The destination is not accessible. Check its mount and permissions." }] : [],
    } }) });
  });

  await app.open("/settings");
  const storage = page.getByRole("region", { name: "Current storage" });
  await expect(storage.getByText("2.5 GiB", { exact: true })).toBeVisible();
  await expect(storage.getByText("100 GiB", { exact: true })).toBeVisible();
  await expect(storage.getByText("24 GiB", { exact: true })).toBeVisible();
  await expect(storage.getByText("5 GiB", { exact: true })).toBeVisible();

  const limit = page.getByLabel("Total backup limit (GiB)");
  await limit.fill("75");
  await storage.getByRole("button", { name: "Refresh storage" }).click();
  await expect(storage.getByText("100 GiB", { exact: true })).toBeVisible();
  await expect(limit).toHaveValue("75");
  await page.getByRole("button", { name: "Save backup settings" }).click();
  await expect(storage.getByText("75 GiB", { exact: true })).toBeVisible();

  unreadable = true;
  await storage.getByRole("button", { name: "Refresh storage" }).click();
  await expect(storage.getByText("Unavailable", { exact: true })).toBeVisible();
  await expect(storage.getByText("The destination is not accessible. Check its mount and permissions.")).toBeVisible();
  await expect(storage.getByText("24 GiB", { exact: true })).toHaveCount(0);
  unreadable = false;
  await storage.getByRole("button", { name: "Refresh storage" }).click();
  await expect(storage.getByText("24 GiB", { exact: true })).toBeVisible();
  await expect(page.locator("body")).toHaveJSProperty("scrollWidth", await page.locator("body").evaluate((body) => body.clientWidth));
});
