import { readFileSync } from "node:fs";
import { test, expect, asAdmin, serverRow, openServer, expectOperationSucceeded } from "./fixtures.mjs";

asAdmin();

test("backing up, downloading, restoring, and deleting a backup", async ({ page }) => {
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page.locator("#backup-readiness")).toContainText("Ready to back up.");
  // A readiness check that answers while the confirmation is open leaves
  // the button it would send in place, so confirming still starts the backup.
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  await page.route("**/backups/preflight", async (route) => {
    await held;
    await route.continue();
  });
  await page.getByRole("button", { name: "Check again" }).click();
  await page.getByRole("button", { name: "Create backup" }).click();
  const confirm = page.getByRole("dialog", { name: "Back up Survival?" });
  await expect(confirm).toBeVisible();
  const preflight = page.waitForResponse("**/backups/preflight");
  release();
  await preflight;
  await page.unroute("**/backups/preflight");
  await confirm.getByRole("button", { name: "Create backup" }).click();
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

  // A restart from the server list can take a backup first.
  await page.goto("/");
  const survival = serverRow(page, "Survival");
  await survival.getByRole("button", { name: /More actions/ }).click();
  await survival.getByRole("button", { name: "Restart…" }).click();
  const restart = page.getByRole("dialog", { name: "Restart Survival?" });
  await expect(survival.locator(".action-menu__content")).toBeHidden();
  await restart.getByLabel("Take a backup first").check();
  await restart.getByRole("button", { name: "Restart server" }).click();
  await expect(restart).toBeHidden();
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(rows).toHaveCount(2, { timeout: 45_000 });
});
