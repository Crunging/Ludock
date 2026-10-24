import { readFileSync } from "node:fs";
import { test, expect, asAdmin, openServer, expectOperationSucceeded } from "./fixtures.mjs";

asAdmin();

test("backing up, downloading, restoring, and deleting a backup", async ({ page }) => {
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page.locator("#backup-readiness")).toContainText("Ready to back up.");
  await page.getByRole("button", { name: "Create backup" }).click();
  await page.getByRole("dialog", { name: "Back up Survival?" }).getByRole("button", { name: "Create backup" }).click();
  await expectOperationSucceeded(page);

  await page.getByRole("tab", { name: "Backups" }).click();
  const rows = page.locator("#backup-list tbody tr");
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText("complete");

  const [download] = await Promise.all([page.waitForEvent("download"), rows.first().getByRole("link", { name: "Download" }).click()]);
  expect(download.suggestedFilename()).toMatch(/^ludock-.*\.tar$/);
  const archive = readFileSync(await download.path());
  expect(archive.subarray(257, 262).toString()).toBe("ustar");

  await rows.first().getByRole("link", { name: "Restore…" }).click();
  const restore = page.locator("form.danger-panel");
  const submit = restore.getByRole("button", { name: "Restore game data" });
  await expect(submit).toBeDisabled();
  await restore.getByLabel(/Type Survival to confirm/).fill("Survival");
  await submit.click();
  await expectOperationSucceeded(page);

  // The restore kept a safety backup of the replaced data.
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(rows).toHaveCount(2);
  await rows.last().getByRole("button", { name: "Delete" }).click();
  await page.getByRole("dialog", { name: "Delete this backup?" }).getByRole("button", { name: "Delete backup" }).click();
  await expect(page.locator("#detail-status")).toHaveText("Backup deleted.");
  await expect(rows).toHaveCount(1);
});
