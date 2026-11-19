import { mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { test, expect, asAdmin, serverRow, openServer, expectOperationSucceeded, signIn, createUser } from "./fixtures.mjs";

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
  // Leaving the page before the restart is accepted would cancel it.
  const queued = page.waitForResponse((response) => response.request().method() === "POST" && /\/restart$/.test(response.url()));
  await restart.getByRole("button", { name: "Restart server" }).click();
  expect((await queued).ok()).toBe(true);
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page).toHaveURL(/\?tab=backups$/);
  // The backup appears once the restart has taken it.
  await expect(async () => {
    await page.reload();
    await expect(rows).toHaveCount(2, { timeout: 1_000 });
  }).toPass({ timeout: 45_000 });
});

test("browsing a backup, downloading one file, and restoring it alone", async ({ page, session }) => {
  const folder = join(process.env.LUDOCK_E2E_DIR, "docker", "data", "minecraft", "restore-one");
  const level = join(folder, "world", "level.dat");
  const notes = join(folder, "notes.txt");
  rmSync(folder, { recursive: true, force: true });
  mkdirSync(join(folder, "world"), { recursive: true });
  writeFileSync(level, "yesterday's world");
  writeFileSync(notes, "yesterday's notes");

  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Backups" }).click();
  // Work an earlier journey started on Survival may still be finishing.
  await expect(page.locator("#create-backup")).toBeEnabled({ timeout: 45_000 });
  await page.getByRole("button", { name: "Create backup" }).click();
  await page.getByRole("dialog", { name: "Back up Survival?" }).getByRole("button", { name: "Create backup" }).click();
  await expectOperationSucceeded(page);
  await page.getByRole("link", { name: "Dismiss" }).click();
  writeFileSync(level, "today's world");
  writeFileSync(notes, "today's notes");

  // Browsing a backup changes nothing and walks it folder by folder.
  await page.locator("#backup-list tbody tr").first().getByRole("link", { name: "Browse" }).click();
  await expect(page.getByRole("heading", { name: /^Backup from/ })).toBeVisible();
  const names = page.locator("#backup-folder .file-row__name");
  await expect(names.filter({ hasText: "restore-one" })).toContainText("3 items");
  await page.getByRole("link", { name: "restore-one", exact: true }).click();
  await expect(names).toHaveText(["world 1 item", "notes.txt"]);
  await page.getByRole("link", { name: "world", exact: true }).click();
  await expect(page.locator(".file-breadcrumbs a[aria-current=page]")).toHaveText("world");
  const row = page.locator("#backup-folder .file-row", { hasText: "level.dat" });
  await expect(row).toContainText("17 B");

  await row.getByRole("button", { name: "Actions for level.dat" }).click();
  const [download] = await Promise.all([page.waitForEvent("download"), row.getByRole("link", { name: "Download" }).click()]);
  expect(download.suggestedFilename()).toBe("level.dat");
  expect(readFileSync(await download.path(), "utf8")).toBe("yesterday's world");
  const fileURL = await row.locator("a[download]").getAttribute("href");

  // Restoring one file names it, replaces only it, and starts the server again.
  await row.getByRole("button", { name: "Actions for level.dat" }).click();
  await row.getByRole("link", { name: "Restore…" }).click();
  const confirm = page.locator("#restore-item");
  await expect(confirm.getByRole("heading")).toHaveText("Restore the file “level.dat”?");
  await expect(confirm.locator("code")).toHaveText(level);
  const submit = confirm.getByRole("button", { name: "Restore file" });
  await expect(submit).toBeDisabled();
  await confirm.getByLabel(/Type Survival to confirm/).fill("Survival");
  await submit.click();
  await expectOperationSucceeded(page);
  await expect(page.locator("#selected-operation")).toContainText("restore-one/world/level.dat");
  expect(readFileSync(level, "utf8")).toBe("yesterday's world");
  expect(readFileSync(notes, "utf8")).toBe("today's notes");
  await expect(page.locator(".detail-header")).toContainText("Running");
  // No safety backup of everything was taken.
  await page.getByRole("link", { name: "Dismiss" }).click();
  const backupCount = await page.locator("#backup-list tbody tr").count();

  // Someone who may create backups but isn't an administrator is offered
  // none of a backup's contents.
  await createUser(page, "backupmaker", "backup maker password", "operator");
  const grants = page.locator("[data-grants] .grants-server", { hasText: "Survival" });
  await grants.getByRole("button", { name: "Status only" }).click();
  await grants.getByLabel("Create backups").check();
  await page.locator("[data-grants]").getByRole("button", { name: "Save server access" }).click();
  await expect(page.locator("[data-grants] .alert--success")).toContainText("Server access saved for backupmaker.");
  const operator = await session({ storageState: { cookies: [], origins: [] } });
  await signIn(operator, "backupmaker", "backup maker password");
  await openServer(operator, "Survival");
  await operator.getByRole("tab", { name: "Backups" }).click();
  await expect(operator.getByText("Archive access and restoration require an administrator.")).toBeVisible();
  await expect(operator.getByRole("link", { name: "Browse" })).toHaveCount(0);
  const backupID = new URL(fileURL, page.url()).pathname.split("/")[4];
  await operator.goto(`${new URL(operator.url()).pathname}?tab=backups&browse=${backupID}`);
  await expect(operator.locator(".backup-browser")).toHaveCount(0);
  for (const path of [fileURL, fileURL.replace(/^\/servers\/([^/]+)\/backups\/([^/]+)\/file/, "/fragments/servers/$1/backups/$2/folder")]) {
    const refused = await operator.request.get(path, { maxRedirects: 0 });
    expect(refused.status()).not.toBe(200);
  }
  expect(backupCount).toBeGreaterThan(0);
});
