import { backupPreflightResponseSchema, operationResponseSchema } from "@ludock/shared";
import { test, expect, RUNNING_ID, RUNNING_NAME } from "./fixtures";

test("backup readiness explains problems before a fresh check and downtime confirmation", { tag: "@responsive" }, async ({ app, page }, testInfo) => {
  app.user = { ...app.user!, role: "operator" };
  app.servers[0].permissions = ["server.view", "backups.create"];
  app.servers[0].latestBackup = { createdAt: Date.UTC(2026, 8, 13, 12), size: 1024 };
  let ready = false;
  let checks = 0;
  let creates = 0;
  await page.route(`**/api/v1/servers/${RUNNING_ID}/backups/preflight`, (route) => {
    checks++;
    return route.fulfill({ json: backupPreflightResponseSchema.parse({
      preflight: { ready, checkedAt: Date.now(), issues: ready ? [] : [
        { code: "BACKUP_DESTINATION", message: "Mount a separate backup destination." },
        { code: "NO_BACKUP_ROOTS", message: "Choose approved mounted data roots for this server." },
      ] },
    }) });
  });
  await page.route(`**/api/v1/servers/${RUNNING_ID}/backups`, (route) => {
    creates++;
    return route.fulfill({ status: 202, json: operationResponseSchema.parse({ operation: {
      id: "c15cbd1f-dbb6-444d-8b8f-c5d728b94df0", serverId: RUNNING_ID,
      kind: "backup", status: "queued", phase: "queued", createdAt: 0, updatedAt: 0, error: null, result: null,
    } }) });
  });
  await app.open(`/servers/${RUNNING_ID}`);
  await page.getByRole("tab", { name: "Backups", exact: true }).click();
  await expect(page.getByText("Mount a separate backup destination.", { exact: true })).toBeVisible();
  await expect(page.getByText("Choose approved mounted data roots for this server.", { exact: true })).toBeVisible();
  await expect(page.getByText(/Latest successful backup:/)).toBeVisible();
  await expect(page.getByRole("button", { name: "Create backup", exact: true })).toBeDisabled();
  await expect(page.getByRole("link", { name: "Download", exact: true })).toHaveCount(0);
  if (testInfo.project.name === "mobile") await page.setViewportSize({ width: 320, height: 844 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.screenshot({ path: testInfo.outputPath("backup-readiness.png"), fullPage: true });
  expect(creates).toBe(0);
  ready = true;
  await page.getByRole("button", { name: "Check again", exact: true }).focus();
  await page.keyboard.press("Enter");
  await expect(page.getByText("Preflight checks passed.", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Create backup", exact: true })).toBeEnabled();
  expect(creates).toBe(0);
  page.once("dialog", async (dialog) => {
    expect(dialog.message()).toContain(RUNNING_NAME);
    expect(dialog.message()).toContain("stops for the entire copy");
    expect(checks).toBe(3);
    expect(creates).toBe(0);
    await dialog.accept();
  });
  await page.getByRole("button", { name: "Create backup", exact: true }).click();
  await expect(page.getByRole("tab", { name: "Activity", selected: true })).toBeVisible();
  expect(creates).toBe(1);
});
