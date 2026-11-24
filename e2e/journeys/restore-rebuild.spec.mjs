import { cpSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { test, expect, asAdmin, openServer, expectOperationSucceeded, fake } from "./fixtures.mjs";

asAdmin();

// Each journey backs up and recreates a server of its own.
const marker = (folder) => join(folder, "restore-rebuild.txt");

async function backUp(page, server) {
  await openServer(page, server.name);
  await page.locator(".section-tabs").getByRole("link", { name: "Backups" }).click();
  await page.getByRole("button", { name: "Create backup" }).click();
  await page.getByRole("dialog", { name: `Back up ${server.name}?` }).getByRole("button", { name: "Create backup" }).click();
  await expectOperationSucceeded(page);
  await page.getByRole("link", { name: "Dismiss" }).click();
}

/**
 * Reloads until the newest backup says text. Pages reuse a discovery from
 * the last two seconds, so one can miss a change made outside Ludock.
 */
async function expectNewestBackup(page, text) {
  const row = page.locator("#backup-list tbody tr").first();
  await expect(async () => {
    await page.reload();
    await expect(row).toContainText(text, { timeout: 1_000 });
  }).toPass({ timeout: 15_000 });
  return row;
}

test("restoring a backup taken before the server's data moved to another folder", async ({ page, ownServer }) => {
  const data = ownServer.data;
  const moved = data + "-moved";
  writeFileSync(marker(data), "from the backup");
  await backUp(page, ownServer);
  try {
    // The data moves to another host folder behind the same path in the server.
    cpSync(data, moved, { recursive: true });
    writeFileSync(marker(moved), "after the move");
    writeFileSync(marker(data), "left behind");
    await fake("/fake/recreate", { container: ownServer.container, mount: `${data}=${moved}` });

    const row = await expectNewestBackup(page, "Taken when the server’s data lived somewhere else");

    // Browsing it says so before restoring one file.
    await row.getByRole("button", { name: /^More actions for the backup/ }).click();
    await row.getByRole("link", { name: "Browse files" }).click();
    await expect(page.locator(".backup-browser .alert--info")).toHaveText(
      "This backup was taken when the server’s data lived somewhere else. Restoring from it puts files where the server keeps its data now.");
    await page.getByRole("link", { name: "All backups" }).click();

    await row.getByRole("link", { name: "Restore…" }).click();
    const restore = page.locator("form.danger-panel");
    await expect(restore.locator(".restore-moved")).toHaveText(
      `This backup was taken when the server’s data lived somewhere else. Restoring it replaces the game data in ${data} where the server keeps it now.`);
    await restore.getByLabel(new RegExp(`Type ${ownServer.name} to confirm`)).fill(ownServer.name);
    await restore.getByRole("button", { name: "Restore game data" }).click();
    await expectOperationSucceeded(page);
    await expect(page.locator("#selected-operation")).toContainText("The backup was taken before the server’s data moved.");

    expect(readFileSync(marker(moved), "utf8")).toBe("from the backup");
    expect(readFileSync(marker(data), "utf8")).toBe("left behind");
    // The safety backup is of the data where it is now, so it restores as usual.
    const rows = page.locator("#backup-list tbody tr");
    await expect(rows).toHaveCount(2);
    await expect(rows.first()).toContainText("Taken before a restore");
    await expect(rows.first()).not.toContainText("lived somewhere else");

    await page.goto("/activity");
    await expect(page.locator("main")).toContainText("The backup was taken before the server’s data moved.");
  } finally {
    rmSync(moved, { recursive: true, force: true });
  }
});

test("a backup of other folders inside the server says why it can't be restored", async ({ page, ownServer }) => {
  const data = ownServer.data;
  await backUp(page, ownServer);
  // The server now keeps its data one folder down.
  cpSync(join(data, "world"), join(data, "kept"), { recursive: true });
  await fake("/fake/recreate", { container: ownServer.container, label: `ludock.files=${join(data, "kept")}` });

  const row = await expectNewestBackup(page, `Can’t be restored here. This backup has the folder ${data}, which the server no longer has. ` +
    `The server now has the folder ${join(data, "kept")}, which this backup doesn’t.`);
  await expect(row.getByRole("link", { name: "Restore…" })).toHaveCount(0);
  await expect(row.getByRole("button", { name: "Restore…" })).toHaveCount(0);
});
