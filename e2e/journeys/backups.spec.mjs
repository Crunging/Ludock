import { existsSync, mkdirSync, readdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { test, expect, asAdmin, serverRow, openServer, expectOperationSucceeded, signIn, createUser, shareServer } from "./fixtures.mjs";

asAdmin();

// Other journeys list Survival's data, so a folder added here is removed.
const restoreFolder = () => join(process.env.LUDOCK_E2E_DIR, "docker", "data", "minecraft", "restore-one");
test.afterEach(() => rmSync(restoreFolder(), { recursive: true, force: true }));

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
  const folder = restoreFolder();
  const level = join(folder, "world", "level.dat");
  const notes = join(folder, "notes.txt");
  rmSync(folder, { recursive: true, force: true });
  mkdirSync(join(folder, "world"), { recursive: true });
  writeFileSync(level, "yesterday's world");
  writeFileSync(notes, "yesterday's notes");
  mkdirSync(join(folder, "chunks"));
  for (let index = 1; index <= 501; index++) writeFileSync(join(folder, "chunks", `chunk-${index}.dat`), "");

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
  await expect(names.filter({ hasText: "restore-one" })).toContainText("505 items");
  await page.getByRole("link", { name: "restore-one", exact: true }).click();
  await expect(names).toHaveText(["chunks 501 items", "world 1 item", "notes.txt"]);

  // Items past the first page can be restored too.
  await page.getByRole("link", { name: "chunks", exact: true }).click();
  await expect(page.locator("#backup-folder [role=status]")).toHaveText("Showing 500 of 501 items");
  await page.getByRole("button", { name: "Show 1 more" }).click();
  const last = page.locator("#backup-folder .file-row", { hasText: "chunk-501.dat" });
  await last.getByRole("button", { name: "Actions for chunk-501.dat" }).click();
  await expect(last.getByRole("link", { name: "Restore…" })).toBeVisible();
  await page.locator(".file-breadcrumbs").getByRole("link", { name: "restore-one" }).click();
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
  await createUser(page, "backupmaker", "backup maker password");
  await shareServer(page, "Survival", "backupmaker", ["Create backups"]);
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

test("keeping a backup with a note, seeing it outlast retention, and releasing it", async ({ page }) => {
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Backups" }).click();
  const retention = page.locator("#backup-retention");
  await expect(retention).toContainText("Ludock keeps the 3 newest backups, plus the last one of each of the 7 most recent days");
  await expect(retention).toContainText("Backups you keep are never removed.");
  const rows = page.locator("#backup-list tbody tr");
  const backUp = async () => {
    // Work an earlier journey started on Survival may still be finishing.
    await expect(page.locator("#create-backup")).toBeEnabled({ timeout: 45_000 });
    await page.getByRole("button", { name: "Create backup" }).click();
    await page.getByRole("dialog", { name: "Back up Survival?" }).getByRole("button", { name: "Create backup" }).click();
    await expectOperationSucceeded(page);
    await page.getByRole("link", { name: "Dismiss" }).click();
  };
  await backUp();

  await rows.first().getByRole("link", { name: "Keep…" }).click();
  const keep = page.locator("#keep-backup");
  await expect(keep).toContainText("Ludock never removes a kept backup");
  await keep.getByLabel(/Note/).fill("before 1.21 update");
  await keep.getByRole("button", { name: "Keep backup" }).click();
  await expect(keep).toHaveCount(0);
  const kept = rows.filter({ hasText: "before 1.21 update" });
  await expect(kept).toContainText("kept");
  await expect(kept.getByRole("button", { name: "Delete" })).toBeDisabled();
  await expect(page.locator("#backup-usage")).toContainText("of it in kept backups");
  const browse = await kept.getByRole("link", { name: "Browse" }).getAttribute("href");

  // Four newer backups leave the three newest and the kept one.
  for (let count = 0; count < 4; count++) await backUp();
  await expect(rows).toHaveCount(4);
  await expect(kept).toHaveCount(1);
  await expect(rows.first()).not.toContainText("before 1.21 update");

  // Released, it is the oldest, so the next backup removes it.
  await kept.getByRole("button", { name: "Release" }).click();
  await expect(page.locator("#detail-status")).toContainText("Backup released.");
  await expect(kept).toHaveCount(0);
  await expect(page.locator(`#backup-list a[href="${browse}"]`)).toHaveCount(1);
  await expect(page.locator("#backup-usage")).not.toContainText("kept");
  await backUp();
  await expect(page.locator(`#backup-list a[href="${browse}"]`)).toHaveCount(0);
  await expect(rows).toHaveCount(3);
});

// savesIn lists the saved copies of Ludock's own data in folder.
const savesIn = (folder) => readdirSync(folder).filter((name) => /^ludock-data-\d{8}-\d{6}\.tar$/.test(name));

test("Ludock saving its own data beside the backups without being asked", async ({ page }) => {
  const backupFolder = join(process.env.LUDOCK_E2E_DIR, "backups");
  await page.goto("/settings");
  const storage = page.locator("#backup-storage");
  const line = storage.locator("#ludock-data-save");
  await expect(line).toContainText("Ludock’s own data");
  // The first save follows backup storage turning itself on.
  await expect(async () => {
    await storage.getByRole("button", { name: "Refresh" }).click();
    await expect(line).toContainText(/Saved (just now|\d+ minutes? ago)/, { timeout: 1_000 });
  }).toPass({ timeout: 45_000 });
  await expect(storage.locator(".backup-storage__issues")).toHaveCount(0);

  // It is one owner-only file in the backup folder, and no server lists it.
  const saves = savesIn(backupFolder);
  expect(saves.length).toBeGreaterThan(0);
  expect(statSync(join(backupFolder, saves[0])).mode & 0o777).toBe(0o600);
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page.locator("#backup-list")).toBeVisible();
  await expect(page.locator("main")).not.toContainText("ludock-data");
});

test("keeping a second copy of every backup and restoring from it", async ({ page }) => {
  const backupFolder = join(process.env.LUDOCK_E2E_DIR, "backups");
  const copyFolder = join(process.env.LUDOCK_E2E_DIR, "copies");
  const settings = page.locator("#backup-settings-form");
  const chooseCopy = async (label) => {
    await page.goto("/settings");
    await settings.getByText("Advanced").click();
    await settings.getByLabel("Also copy backups to").selectOption({ label });
    await settings.getByRole("button", { name: "Save backup settings" }).click();
    await expect(settings.locator(".alert--success")).toContainText("Backup settings saved.");
  };
  await chooseCopy(copyFolder);
  await expect(settings.locator("#backup-storage")).toContainText(`Second copy in${copyFolder}`);
  // Ludock's own data goes there too, the newest save first.
  await expect.poll(() => savesIn(copyFolder).length, { timeout: 45_000 }).toBeGreaterThan(0);
  const saved = savesIn(copyFolder).at(-1);
  expect(readFileSync(join(copyFolder, saved))).toEqual(readFileSync(join(backupFolder, saved)));

  // Each backup is copied there once its server is back.
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page.locator("#create-backup")).toBeEnabled({ timeout: 45_000 });
  await page.getByRole("button", { name: "Create backup" }).click();
  await page.getByRole("dialog", { name: "Back up Survival?" }).getByRole("button", { name: "Create backup" }).click();
  await expectOperationSucceeded(page);
  await page.getByRole("link", { name: "Dismiss" }).click();
  const rows = page.locator("#backup-list tbody tr");
  const href = await rows.first().getByRole("link", { name: "Download" }).getAttribute("href");
  const id = href.split("/")[4];
  const row = page.locator("#backup-list tbody tr", { has: page.locator(`a[href="${href}"]`) });
  await expect(async () => {
    await page.reload();
    await expect(row).toContainText("Second copy saved", { timeout: 1_000 });
  }).toPass({ timeout: 30_000 });
  const copy = join(copyFolder, `${id}.tar`);
  expect(readFileSync(copy)).toEqual(readFileSync(join(backupFolder, `${id}.tar`)));

  // With the archive gone from the backup folder, the copy stands in for it.
  rmSync(join(backupFolder, `${id}.tar`));
  await page.reload();
  await expect(row).toContainText("Missing from the backup folder; using the second copy");
  const [download] = await Promise.all([page.waitForEvent("download"), row.getByRole("link", { name: "Download" }).click()]);
  expect(readFileSync(await download.path())).toEqual(readFileSync(copy));
  await row.getByRole("link", { name: "Browse" }).click();
  await expect(page.locator(".backup-browser .alert--warning")).toContainText("reading the second copy");
  await page.getByRole("link", { name: "All backups" }).click();
  await row.getByRole("link", { name: "Restore…" }).click();
  const restore = page.locator("form.danger-panel");
  await expect(restore.locator(".alert--warning")).toContainText("restores it from the second copy");
  await restore.getByLabel(/Type Survival to confirm/).fill("Survival");
  await restore.getByRole("button", { name: "Restore game data" }).click();
  await expectOperationSucceeded(page);
  await page.getByRole("link", { name: "Dismiss" }).click();

  // Deleting the backup deletes its copy too.
  await row.getByRole("button", { name: "Delete" }).click();
  await page.getByRole("dialog", { name: "Delete this backup?" }).getByRole("button", { name: "Delete backup" }).click();
  await expect(page.locator("#detail-status")).toHaveText("Backup deleted.");
  await expect(row).toHaveCount(0);
  // The copy is removed beside the deletion, so it may take a moment.
  await expect.poll(() => existsSync(copy)).toBe(false);

  await chooseCopy("No second copy");
  await expect(settings.locator("#backup-storage")).not.toContainText("Second copy in");
});
