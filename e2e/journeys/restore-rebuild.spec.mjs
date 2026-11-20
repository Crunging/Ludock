import { cpSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { test, expect, asAdmin, openServer, expectOperationSucceeded, fake } from "./fixtures.mjs";

asAdmin();

const valheim = () => join(process.env.LUDOCK_E2E_DIR, "docker", "data", "valheim");
const moved = () => join(process.env.LUDOCK_E2E_DIR, "docker", "data", "valheim-moved");
const marker = (folder) => join(folder, "restore-rebuild.txt");

async function backUpValheim(page) {
  await openServer(page, "valheim");
  await page.getByRole("tab", { name: "Backups" }).click();
  await page.getByRole("button", { name: "Create backup" }).click();
  await page.getByRole("dialog", { name: "Back up valheim?" }).getByRole("button", { name: "Create backup" }).click();
  await expectOperationSucceeded(page);
  await page.getByRole("link", { name: "Dismiss" }).click();
}

/** Deletes every Valheim backup, so other journeys find none. */
async function deleteBackups(page) {
  await page.goto(page.url().replace(/\?.*$/, "") + "?tab=backups");
  const rows = page.locator("#backup-list tbody tr:has(.action-menu)");
  for (let count = await rows.count(); count > 0; count--) {
    await rows.first().getByRole("button", { name: /^More actions for the backup/ }).click();
    await rows.first().getByRole("button", { name: "Delete", exact: true }).click();
    await page.getByRole("dialog", { name: "Delete this backup?" }).getByRole("button", { name: "Delete backup" }).click();
    await expect(page.locator("#detail-status")).toHaveText("Backup deleted.");
    await expect(rows).toHaveCount(count - 1);
  }
}

test.afterEach(async () => {
  await fake("/fake/recreate", { container: "valheim", mount: `${valheim()}=${valheim()}` });
  await fake("/fake/recreate", { container: "valheim", label: "ludock.files" });
  rmSync(moved(), { recursive: true, force: true });
  rmSync(marker(valheim()), { force: true });
  rmSync(join(valheim(), "kept"), { recursive: true, force: true });
});

test("restoring a backup taken before the server's data moved to another folder", async ({ page }) => {
  writeFileSync(marker(valheim()), "from the backup");
  await backUpValheim(page);
  // The data moves to another host folder behind the same path in the server.
  cpSync(valheim(), moved(), { recursive: true });
  writeFileSync(marker(moved()), "after the move");
  writeFileSync(marker(valheim()), "left behind");
  await fake("/fake/recreate", { container: "valheim", mount: `${valheim()}=${moved()}` });

  await page.reload();
  const row = page.locator("#backup-list tbody tr").first();
  await expect(row).toContainText("Taken when the server’s data lived somewhere else");

  // Browsing it says so before restoring one file.
  await row.getByRole("button", { name: /^More actions for the backup/ }).click();
  await row.getByRole("link", { name: "Browse files" }).click();
  await expect(page.locator(".backup-browser .alert--info")).toHaveText(
    "This backup was taken when the server’s data lived somewhere else. Restoring from it puts files where the server keeps its data now.");
  await page.getByRole("link", { name: "All backups" }).click();

  await row.getByRole("link", { name: "Restore…" }).click();
  const restore = page.locator("form.danger-panel");
  await expect(restore.locator(".restore-moved")).toHaveText(
    `This backup was taken when the server’s data lived somewhere else. Restoring it replaces the game data in ${valheim()} where the server keeps it now.`);
  await restore.getByLabel(/Type valheim to confirm/).fill("valheim");
  await restore.getByRole("button", { name: "Restore game data" }).click();
  await expectOperationSucceeded(page);
  await expect(page.locator("#selected-operation")).toContainText("The backup was taken before the server’s data moved.");

  expect(readFileSync(marker(moved()), "utf8")).toBe("from the backup");
  expect(readFileSync(marker(valheim()), "utf8")).toBe("left behind");
  // The safety backup is of the data where it is now, so it restores as usual.
  const rows = page.locator("#backup-list tbody tr");
  await expect(rows).toHaveCount(2);
  await expect(rows.first()).toContainText("Taken before a restore");
  await expect(rows.first()).not.toContainText("lived somewhere else");

  await page.goto("/activity");
  await expect(page.locator("main")).toContainText("The backup was taken before the server’s data moved.");
  await openServer(page, "valheim");
  await deleteBackups(page);
});

test("a backup of other folders inside the server says why it can't be restored", async ({ page }) => {
  await backUpValheim(page);
  // The server now keeps its data one folder down.
  cpSync(join(valheim(), "world"), join(valheim(), "kept"), { recursive: true });
  await fake("/fake/recreate", { container: "valheim", label: `ludock.files=${join(valheim(), "kept")}` });

  await page.reload();
  const row = page.locator("#backup-list tbody tr").first();
  await expect(row).toContainText(`Can’t be restored here. This backup has the folder ${valheim()}, which the server no longer has. ` +
    `The server now has the folder ${join(valheim(), "kept")}, which this backup doesn’t.`);
  await expect(row.getByRole("link", { name: "Restore…" })).toHaveCount(0);
  await expect(row.getByRole("button", { name: "Restore…" })).toHaveCount(0);
  await deleteBackups(page);
});
