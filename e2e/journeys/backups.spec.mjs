import { readFileSync } from "node:fs";
import { test, expect, asAdmin, openServer, expectOperationSucceeded } from "./fixtures.mjs";

asAdmin();

test("backing up, downloading, restoring, and deleting a backup", async ({ page }) => {
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page.locator("#backup-readiness")).toContainText("Ready to back up.");
  await page.getByRole("button", { name: "Create backup" }).click();
  await page.getByRole("dialog", { name: "Back up Survival?" }).getByRole("button", { name: "Create backup" }).click();
  // Progress shows where the backup started, and the tab catches up once
  // it finishes.
  await expect(page).toHaveURL(/\?tab=backups&operation=/);
  await expect(page.getByRole("tab", { name: "Backups" })).toHaveAttribute("aria-selected", "true");
  await expectOperationSucceeded(page);
  const rows = page.locator("#backup-list tbody tr");
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText("complete");
  await expect(page.locator("#backup-readiness")).toContainText("Ready to back up.");
  await expect(page.locator("#create-backup")).toBeEnabled();

  const [download] = await Promise.all([page.waitForEvent("download"), page.getByRole("link", { name: "Download backup" }).click()]);
  expect(download.suggestedFilename()).toMatch(/^ludock-.*\.tar$/);
  const archive = readFileSync(await download.path());
  expect(archive.subarray(257, 262).toString()).toBe("ustar");

  await page.getByRole("link", { name: "Dismiss" }).click();
  await expect(page.locator("#selected-operation")).toHaveCount(0);
  await expect(page).toHaveURL(/\?tab=backups$/);

  await rows.first().getByRole("link", { name: "Restore…" }).click();
  const restore = page.locator("form.danger-panel");
  const submit = restore.getByRole("button", { name: "Restore game data" });
  await expect(submit).toBeDisabled();
  await restore.getByLabel(/Type Survival to confirm/).fill("Survival");
  await submit.click();
  await expect(page).toHaveURL(/\?tab=backups&operation=/);
  await expectOperationSucceeded(page);

  // The restore kept a safety backup of the replaced data.
  await expect(rows).toHaveCount(2);
  await expect(page.locator("form.danger-panel")).toHaveCount(0);
  await rows.last().getByRole("button", { name: "Delete" }).click();
  await page.getByRole("dialog", { name: "Delete this backup?" }).getByRole("button", { name: "Delete backup" }).click();
  await expect(page.locator("#detail-status")).toHaveText("Backup deleted.");
  await expect(rows).toHaveCount(1);
});
