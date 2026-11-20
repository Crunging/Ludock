import { existsSync, mkdirSync, readdirSync, readFileSync, renameSync, rmSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { test, expect, asAdmin, serverRow, openServer, expectOperationSucceeded, signIn, createUser, shareServer, fake, restoreServer } from "./fixtures.mjs";

asAdmin();

/** Opens a backup row's menu and returns the named item in it. */
async function menuItem(row, role, name) {
  await row.getByRole("button", { name: /^More actions for the backup/ }).click();
  return row.locator(".action-menu__content").getByRole(role, { name, exact: true });
}

// Each journey backs up a server of its own; the backup settings they
// change are shared, so the file runs in order.

test("backing up, downloading, restoring, and deleting a backup", async ({ page, ownServer }) => {
  await openServer(page, ownServer.name);
  await page.getByRole("tab", { name: "Backups" }).click();
  // A server that is ready says nothing about readiness.
  await expect(page.locator("#create-backup")).toBeEnabled();
  await expect(page.locator("#backup-readiness")).toBeEmpty();
  await expect(page.locator("main")).not.toContainText("How backups work");
  // A readiness check that answers while the confirmation is open leaves
  // the button it would send in place, so confirming still starts the backup.
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  await page.route("**/backups/preflight", async (route) => {
    await held;
    await route.continue();
  });
  await page.evaluate(() => {
    htmx.ajax("GET", location.pathname.replace("/servers/", "/fragments/servers/") + "/backups/preflight",
      { target: "#backup-readiness", swap: "outerHTML" });
  });
  await page.getByRole("button", { name: "Create backup" }).click();
  const confirm = page.getByRole("dialog", { name: `Back up ${ownServer.name}?` });
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
  // The copy holds what the game last saved, so it is asked to save first.
  await expect(page.locator("#selected-operation")).toContainText("Save command sent before stopping.");
  const rows = page.locator("#backup-list tbody tr");
  await expect(rows).toHaveCount(1);
  // A complete backup needs no state of its own; the stored value never shows.
  await expect(rows.first().locator("td").nth(2)).not.toContainText("complete");
  await expect(rows.first().getByRole("link", { name: "Restore…" })).toBeVisible();
  await expect(page.locator("#create-backup")).toBeEnabled();
  await expect(page.locator("#backup-readiness")).toBeEmpty();

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
  await restore.getByLabel(`Type ${ownServer.name} to confirm`).fill(ownServer.name);
  await submit.click();
  await expect(page).toHaveURL(/\?tab=backups&operation=/);
  await expectOperationSucceeded(page);

  // The restore kept a safety backup of the replaced data.
  await expect(rows).toHaveCount(2);
  await expect(page.locator("form.danger-panel")).toHaveCount(0);
  // Each backup has one button; the rest are in its menu.
  await expect(rows.last().locator(".backup-row__actions > .secondary-btn")).toHaveText(["Restore…"]);
  // The notice comes with the refreshed list, not ahead of a slower read of it.
  await page.route(/\/fragments\/servers\/[^/]+\/backups$/, async (route) => {
    await new Promise((resolve) => setTimeout(resolve, 1500));
    await route.continue();
  });
  await (await menuItem(rows.last(), "button", "Delete")).click();
  await page.getByRole("dialog", { name: "Delete this backup?" }).getByRole("button", { name: "Delete backup" }).click();
  await expect(page.locator("#detail-status")).toHaveText("Backup deleted.");
  expect(await rows.count()).toBe(1);
  await page.unrouteAll();

  // A restart from the server list can take a backup first.
  await page.goto("/");
  const row = serverRow(page, ownServer.name);
  await row.getByRole("button", { name: /More actions/ }).click();
  await row.getByRole("button", { name: "Restart…" }).click();
  const restart = page.getByRole("dialog", { name: `Restart ${ownServer.name}?` });
  await expect(row.locator(".action-menu__content")).toBeHidden();
  await restart.getByLabel("Back up first").check();
  // Leaving the page before the restart is accepted would cancel it.
  const queued = page.waitForResponse((response) => response.request().method() === "POST" && /\/restart$/.test(response.url()));
  await restart.getByRole("button", { name: "Restart server" }).click();
  expect((await queued).ok()).toBe(true);
  await openServer(page, ownServer.name);
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page).toHaveURL(/\?tab=backups$/);
  // The backup appears once the restart has taken it.
  await expect(async () => {
    await page.reload();
    await expect(rows).toHaveCount(2, { timeout: 1_000 });
  }).toPass({ timeout: 45_000 });
});

test("browsing a backup, downloading one file, and restoring it alone", async ({ page, session, ownServer }) => {
  const folder = join(ownServer.data, "restore-one");
  const level = join(folder, "world", "level.dat");
  const notes = join(folder, "notes.txt");
  rmSync(folder, { recursive: true, force: true });
  mkdirSync(join(folder, "world"), { recursive: true });
  writeFileSync(level, "yesterday's world");
  writeFileSync(notes, "yesterday's notes");
  mkdirSync(join(folder, "chunks"));
  for (let index = 1; index <= 501; index++) writeFileSync(join(folder, "chunks", `chunk-${index}.dat`), "");

  await openServer(page, ownServer.name);
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page.locator("#create-backup")).toBeEnabled();
  await page.getByRole("button", { name: "Create backup" }).click();
  await page.getByRole("dialog", { name: `Back up ${ownServer.name}?` }).getByRole("button", { name: "Create backup" }).click();
  await expectOperationSucceeded(page);
  await page.getByRole("link", { name: "Dismiss" }).click();
  writeFileSync(level, "today's world");
  writeFileSync(notes, "today's notes");

  // Browsing a backup changes nothing and walks it folder by folder.
  const backupCount = await page.locator("#backup-list tbody tr").count();
  await (await menuItem(page.locator("#backup-list tbody tr").first(), "link", "Browse files")).click();
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
  await expect(last.getByRole("button", { name: "Restore…" })).toBeVisible();
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

  // Restoring one file names it in the usual confirmation, backs up the
  // server first unless told not to, replaces only that file, and starts
  // the server again.
  await row.getByRole("button", { name: "Actions for level.dat" }).click();
  await row.getByRole("button", { name: "Restore…" }).click();
  const confirm = page.getByRole("dialog", { name: "Restore the file “level.dat”?" });
  await expect(confirm).toContainText(`replaces ${level}`);
  await expect(confirm.getByLabel("Back up first")).toBeChecked();
  await confirm.getByRole("button", { name: "Restore file" }).click();
  await expectOperationSucceeded(page);
  await expect(page.locator("#selected-operation")).toContainText("restore-one/world/level.dat");
  expect(readFileSync(level, "utf8")).toBe("yesterday's world");
  expect(readFileSync(notes, "utf8")).toBe("today's notes");
  await expect(page.locator(".detail-header")).toContainText("Running");
  // The backup it took first is the newest; retention may have removed an
  // older one.
  await page.getByRole("link", { name: "Dismiss" }).click();
  await expect(page.locator("#backup-list tbody tr").first()).toContainText("Taken before a restore");

  // Someone who may create backups but isn't an administrator is offered
  // none of a backup's contents.
  const backupmaker = "backups-" + ownServer.container;
  await createUser(page, backupmaker, "backup maker password");
  await shareServer(page, ownServer.name, backupmaker, ["Create backups"]);
  const operator = await session({ storageState: { cookies: [], origins: [] } });
  await signIn(operator, backupmaker, "backup maker password");
  await openServer(operator, ownServer.name);
  await operator.getByRole("tab", { name: "Backups" }).click();
  await expect(operator.locator("#backup-retention")).toBeVisible();
  await expect(operator.locator("#backup-list")).toHaveCount(0);
  await expect(operator.getByRole("link", { name: "Browse files" })).toHaveCount(0);
  const backupID = new URL(fileURL, page.url()).pathname.split("/")[4];
  await operator.goto(`${new URL(operator.url()).pathname}?tab=backups&browse=${backupID}`);
  await expect(operator.locator(".backup-browser")).toHaveCount(0);
  for (const path of [fileURL, fileURL.replace(/^\/servers\/([^/]+)\/backups\/([^/]+)\/file/, "/fragments/servers/$1/backups/$2/folder")]) {
    const refused = await operator.request.get(path, { maxRedirects: 0 });
    expect(refused.status()).not.toBe(200);
  }
  expect(backupCount).toBeGreaterThan(0);
});

test("keeping a backup with a note, seeing it outlast retention, and releasing it", async ({ page, ownServer }) => {
  await openServer(page, ownServer.name);
  await page.getByRole("tab", { name: "Backups" }).click();
  const retention = page.locator("#backup-retention");
  await retention.getByText("Which backups Ludock keeps").click();
  await expect(retention).toContainText("The 3 newest, plus the last one of each of the 7 most recent days");
  await expect(retention).toContainText("Backups you keep are never removed.");
  const rows = page.locator("#backup-list tbody tr");
  const backUp = async () => {
    await expect(page.locator("#create-backup")).toBeEnabled();
    await page.getByRole("button", { name: "Create backup" }).click();
    await page.getByRole("dialog", { name: `Back up ${ownServer.name}?` }).getByRole("button", { name: "Create backup" }).click();
    await expectOperationSucceeded(page);
    await page.getByRole("link", { name: "Dismiss" }).click();
  };
  // Retention keeps the kept ones, the three newest of the rest, and the
  // last of the rest from each earlier day (Ludock and this journey share the
  // local time zone), so a run that crosses midnight keeps one more. Rows are
  // newest first.
  const listed = async () => (await rows.evaluateAll((trs) => trs.map((tr) => ({
    at: tr.querySelector("time").getAttribute("datetime"), kept: tr.classList.contains("backup-row--kept") }))))
    .map((row) => ({ ...row, day: new Date(row.at).toDateString() }));
  const expectRetained = (keptCount) => expect.poll(async () => {
    const all = await listed();
    const unkept = all.filter((row) => !row.kept);
    const lastOfDay = unkept.slice(3).every((row, index) => unkept[index + 2].day !== row.day);
    return lastOfDay && all.filter((row) => row.kept).length === keptCount && unkept.length >= 3;
  }).toBe(true);
  await backUp();

  await (await menuItem(rows.first(), "link", "Keep…")).click();
  const keep = page.locator("#keep-backup");
  await expect(keep).toContainText("Ludock never removes a kept backup");
  await keep.getByLabel(/Note/).fill("before 1.21 update");
  await keep.getByRole("button", { name: "Keep backup" }).click();
  await expect(keep).toHaveCount(0);
  const kept = rows.filter({ hasText: "before 1.21 update" });
  await expect(kept.locator("td").nth(2)).toContainText("Kept");
  await expect(kept.getByRole("button", { name: "Delete", includeHidden: true })).toBeDisabled();
  await expect(page.locator("#backup-usage")).toContainText("of it in kept backups");
  const browse = await kept.getByRole("link", { name: "Browse files", includeHidden: true }).getAttribute("href");

  // Four newer backups leave the three newest and the kept one.
  for (let count = 0; count < 4; count++) await backUp();
  await expectRetained(1);
  await expect(kept).toHaveCount(1);
  await expect(rows.first()).not.toContainText("before 1.21 update");

  // Released, it is the oldest, so the next backup removes it, unless it
  // is the last of an earlier day.
  const before = await listed();
  const released = before.find((row) => row.kept);
  const lastOfItsDay = !before.some((row) => !row.kept && row.at > released.at && row.day === released.day);
  await (await menuItem(kept, "button", "Release")).click();
  await expect(page.locator("#detail-status")).toContainText("Backup released.");
  await expect(kept).toHaveCount(0);
  await expect(page.locator(`#backup-list a[href="${browse}"]`)).toHaveCount(1);
  await expect(page.locator("#backup-usage")).not.toContainText("kept");
  await backUp();
  await expect(page.locator(`#backup-list a[href="${browse}"]`)).toHaveCount(lastOfItsDay ? 1 : 0);
  await expectRetained(0);
});

// savesIn lists the saved copies of Ludock's own data in folder.
const savesIn = (folder) => readdirSync(folder).filter((name) => /^ludock-data-\d{8}-\d{6}\.tar$/.test(name));

test("Ludock saving its own data beside the backups without being asked", async ({ page, ownServer }) => {
  const backupFolder = join(process.env.LUDOCK_E2E_DIR, "backups");
  await page.goto("/settings");
  const storage = page.locator("#backup-storage");
  const line = storage.locator("#ludock-data-save");
  await expect(line).toContainText("Ludock’s own data");
  // The first save follows backup storage turning itself on.
  await expect(async () => {
    await page.reload();
    await expect(line).toContainText(/Saved (just now|\d+ minutes? ago)/, { timeout: 1_000 });
  }).toPass({ timeout: 45_000 });
  await expect(storage.locator(".backup-storage__issues")).toHaveCount(0);

  // It is one owner-only file in the backup folder, and no server lists it.
  const saves = savesIn(backupFolder);
  expect(saves.length).toBeGreaterThan(0);
  expect(statSync(join(backupFolder, saves[0])).mode & 0o777).toBe(0o600);
  await openServer(page, ownServer.name);
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page.locator("#backup-list")).toBeVisible();
  await expect(page.locator("main")).not.toContainText("ludock-data");
});

test("keeping a second copy of every backup and restoring from it", async ({ page, ownServer }) => {
  const backupFolder = join(process.env.LUDOCK_E2E_DIR, "backups");
  const copyFolder = join(process.env.LUDOCK_E2E_DIR, "copies");
  const settings = page.locator("#backup-settings-form");
  const chooseCopy = async (label) => {
    await page.goto("/settings");
    await page.locator("#advanced-settings > summary").click();
    await settings.getByLabel("Also copy backups to").selectOption({ label });
    await settings.getByRole("button", { name: "Save backup settings" }).click();
    await expect(settings.locator(".alert--success")).toContainText("Backup settings saved.");
  };
  await chooseCopy(copyFolder);
  await expect(page.locator("#backup-storage")).toContainText(`Second copy in${copyFolder}`);
  // A folder chosen by hand isn't one Ludock found.
  await expect(page.locator("#backup-storage")).not.toContainText("Ludock found this folder");
  // Ludock's own data goes there too, the newest save first.
  await expect.poll(() => savesIn(copyFolder).length, { timeout: 45_000 }).toBeGreaterThan(0);
  const saved = savesIn(copyFolder).at(-1);
  expect(readFileSync(join(copyFolder, saved))).toEqual(readFileSync(join(backupFolder, saved)));

  // Each backup is copied there once its server is back.
  await openServer(page, ownServer.name);
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page.locator("#create-backup")).toBeEnabled();
  await page.getByRole("button", { name: "Create backup" }).click();
  await page.getByRole("dialog", { name: `Back up ${ownServer.name}?` }).getByRole("button", { name: "Create backup" }).click();
  await expectOperationSucceeded(page);
  await page.getByRole("link", { name: "Dismiss" }).click();
  const rows = page.locator("#backup-list tbody tr");
  const href = await rows.first().getByRole("link", { name: "Download", includeHidden: true }).getAttribute("href");
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
  const downloadLink = await menuItem(row, "link", "Download");
  const [download] = await Promise.all([page.waitForEvent("download"), downloadLink.click()]);
  expect(readFileSync(await download.path())).toEqual(readFileSync(copy));
  await (await menuItem(row, "link", "Browse files")).click();
  await expect(page.locator(".backup-browser .alert--warning")).toContainText("reading the second copy");
  await page.getByRole("link", { name: "All backups" }).click();
  await row.getByRole("link", { name: "Restore…" }).click();
  const restore = page.locator("form.danger-panel");
  await expect(restore.locator(".alert--warning")).toContainText("restores it from the second copy");
  await restore.getByLabel(`Type ${ownServer.name} to confirm`).fill(ownServer.name);
  await restore.getByRole("button", { name: "Restore game data" }).click();
  await expectOperationSucceeded(page);
  await page.getByRole("link", { name: "Dismiss" }).click();

  // Deleting the backup deletes its copy too.
  await (await menuItem(row, "button", "Delete")).click();
  await page.getByRole("dialog", { name: "Delete this backup?" }).getByRole("button", { name: "Delete backup" }).click();
  await expect(page.locator("#detail-status")).toHaveText("Backup deleted.");
  await expect(row).toHaveCount(0);
  // The copy is removed beside the deletion, so it may take a moment.
  await expect.poll(() => existsSync(copy)).toBe(false);

  await chooseCopy("No second copy");
  await expect(page.locator("#backup-storage")).not.toContainText("Second copy in");
});

test("removing a backup whose folder was lost", async ({ page, ownServer }) => {
  const backupFolder = join(process.env.LUDOCK_E2E_DIR, "backups");
  const oldFolder = join(process.env.LUDOCK_E2E_DIR, "copies");
  const lost = oldFolder + "-lost";
  const settings = page.locator("#backup-settings-form");
  const saveIn = async (folder) => {
    await page.goto("/settings");
    await page.locator("#advanced-settings > summary").click();
    await settings.getByLabel("Backup folder").fill(folder);
    await settings.getByRole("button", { name: "Save backup settings" }).click();
    await expect(settings.locator(".alert--success")).toContainText("Backup settings saved.");
  };
  // A backup saved in a folder on a disk that is later lost.
  await saveIn(oldFolder);
  await openServer(page, ownServer.name);
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page.locator("#create-backup")).toBeEnabled();
  await page.getByRole("button", { name: "Create backup" }).click();
  await page.getByRole("dialog", { name: `Back up ${ownServer.name}?` }).getByRole("button", { name: "Create backup" }).click();
  await expectOperationSucceeded(page);
  const href = await page.locator("#backup-list tbody tr").first().getByRole("link", { name: "Download", includeHidden: true }).getAttribute("href");
  const id = href.split("/")[4];
  await saveIn(backupFolder);
  renameSync(oldFolder, lost);
  try {
    await openServer(page, ownServer.name);
    await page.getByRole("tab", { name: "Backups" }).click();
    const row = page.locator("#backup-list tbody tr", { has: page.locator(`a[href="${href}"]`) });
    await expect(row).toContainText("Its backup folder can’t be reached");
    await (await menuItem(row, "button", "Delete")).click();
    const dialog = page.getByRole("dialog", { name: "Remove this backup from Ludock?" });
    await expect(dialog).toContainText("its archive may still be there");
    await dialog.getByRole("button", { name: "Remove backup" }).click();
    await expect(page.locator("#detail-status")).toContainText("Backup removed from Ludock.");
    await expect(row).toHaveCount(0);
    expect(existsSync(join(lost, `${id}.tar`))).toBe(true);
  } finally {
    renameSync(lost, oldFolder);
    // Later journeys expect only their own files in that folder.
    for (const name of readdirSync(oldFolder)) rmSync(join(oldFolder, name), { force: true });
  }
});

test("a backup Docker fails says so plainly, and the Backups tab shows it until one succeeds", async ({ page }) => {
  await openServer(page, "valheim");
  await page.locator(".detail-actions").getByRole("button", { name: "Start" }).click();
  await expect(page.locator(".detail-actions").getByRole("button", { name: "Stop" })).toBeVisible();
  try {
    await page.getByRole("tab", { name: "Backups" }).click();
    await expect(page.locator("#create-backup")).toBeEnabled({ timeout: 45_000 });
    await fake("/fake/fault", { match: "^POST .*/containers/create", status: "500" });
    try {
      await page.getByRole("button", { name: "Create backup" }).click();
      await page.getByRole("dialog", { name: "Back up valheim?" }).getByRole("button", { name: "Create backup" }).click();
      const selected = page.locator("#selected-operation");
      await expect(selected).toContainText("Failed", { timeout: 45_000 });
      // Docker failed, not the server's folders.
      await expect(selected).toContainText("Docker reported an error when Ludock started its helper for the server’s files");
      await expect(selected).not.toContainText("data folders");
    } finally {
      await fake("/fake/fault", { clear: "1" });
    }
    await page.getByRole("link", { name: "Dismiss" }).click();
    const failed = page.locator("#backup-last-failed");
    await expect(failed).toContainText("The last backup failed");
    await failed.getByRole("link", { name: "See what happened" }).click();
    await expect(page).toHaveURL(/\?tab=activity&operation=/);
    await expect(page.locator("#selected-operation")).toContainText("Docker reported an error");

    // The server went back to running, and Valheim saves on its own stop.
    await page.getByRole("tab", { name: "Backups" }).click();
    await expect(page.locator("#create-backup")).toBeEnabled({ timeout: 45_000 });
    await page.getByRole("button", { name: "Create backup" }).click();
    await page.getByRole("dialog", { name: "Back up valheim?" }).getByRole("button", { name: "Create backup" }).click();
    await expectOperationSucceeded(page);
    await expect(page.locator("#selected-operation")).toContainText("The game saves its world when it stops.");
    await expect(page.locator("#selected-operation")).not.toContainText("No save command");
    await expect(failed).toHaveCount(0);
  } finally {
    await restoreServer(page, "valheim");
  }
});
