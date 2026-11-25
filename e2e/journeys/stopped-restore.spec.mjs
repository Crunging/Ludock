import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { test, expect, asAdmin, openServer, expectOperationSucceeded, fake } from "./fixtures.mjs";

asAdmin();

// Another manager starts the server between a restore moving its data out
// and the backup's in, and the game, finding its folder emptied, keeps
// exiting and being restarted by Docker. Its page says so and offers Stop.
// The restore stops and says where the data is; Needs attention offers to
// undo it, which stops the server without warning anyone, puts the data
// back, keeps what the game wrote meanwhile, and starts the server again.
// Stop works on a server that keeps restarting.
test("undoing a restore that was stopped partway", async ({ page, ownServer: server }) => {
  const marker = join(server.data, "stopped-restore.txt");
  writeFileSync(marker, "from the backup");
  await openServer(page, server.name);
  await page.locator(".section-tabs").getByRole("link", { name: "Backups" }).click();
  await page.getByRole("button", { name: "Create backup" }).click();
  await page.getByRole("dialog", { name: `Back up ${server.name}?` }).getByRole("button", { name: "Create backup" }).click();
  await expectOperationSucceeded(page);
  await page.getByRole("link", { name: "Dismiss" }).click();
  writeFileSync(marker, "current");

  await fake("/fake/start-after", { container: server.container, step: "moveOld" });
  await page.locator("#backup-list tbody tr").first().getByRole("link", { name: "Restore…" }).click();
  const form = page.locator("form.danger-panel");
  await form.getByLabel(/Type .* to confirm/).fill(server.name);
  await form.getByRole("button", { name: "Restore game data" }).click();
  await expect(page).toHaveURL(/[?&]operation=/);
  const restoreID = new URL(page.url()).searchParams.get("operation");
  const details = page.locator("#selected-operation");
  await expect(details).toContainText("Failed", { timeout: 45_000 });
  await expect(details).toContainText(`The restore was stopped partway. The server’s previous data is safe in the folder .ludock-restore-${restoreID}`);
  // The game, started on the emptied folder, writes a new world.
  writeFileSync(join(server.data, "generated.txt"), "written meanwhile");
  await fake("/fake/crashloop", { container: server.container });
  await openServer(page, server.name);
  await expect(page.locator(".detail-header__identity")).toContainText("Keeps restarting");
  await expect(page.locator("#detail-live")).toContainText("The game keeps stopping and Docker keeps starting it again.");
  await expect(page.locator(".detail-actions").getByRole("button", { name: "Stop" })).toBeEnabled();
  await expect(page.locator(".detail-actions").getByRole("button", { name: "Restart" })).toHaveCount(0);

  await page.goto("/");
  const item = page.locator(".needs-attention__list li", { hasText: "Restore stopped partway" });
  await expect(item).toContainText(`The restore of ${server.name} was stopped partway. Its previous data is safe but not in place: undo the restore to put it back.`);
  await item.getByRole("button", { name: "Undo the restore" }).click();
  const dialog = page.getByRole("dialog", { name: `Undo the restore of ${server.name}?` });
  await expect(dialog).toContainText("even though it was started outside Ludock");
  await expect(dialog).toContainText(`ludock-set-aside-${restoreID}`);
  await dialog.getByRole("button", { name: "Undo the restore" }).click();
  await expect(page.locator("#action-error")).toContainText(`Undoing the restore of ${server.name}.`);

  const aside = join(server.data, `ludock-set-aside-${restoreID}`, "generated.txt");
  await expect.poll(() => existsSync(aside) && readFileSync(marker, "utf8"), { timeout: 45_000 }).toBe("current");
  expect(readFileSync(aside, "utf8")).toBe("written meanwhile");
  await expect(item).toHaveCount(0, { timeout: 15_000 });
  await expect(page.locator(".needs-attention__list li", { hasText: "Restore failed, then undone" })).toBeVisible();

  await openServer(page, server.name);
  await expect(page.locator(".detail-actions").getByRole("button", { name: "Stop" })).toBeVisible();
  await fake("/fake/crashloop", { container: server.container });
  await expect(page.locator(".detail-header__identity")).toContainText("Keeps restarting");
  await page.locator(".detail-actions").getByRole("button", { name: "Stop" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Stop server" }).click();
  await expect(page.locator(".detail-actions").getByRole("button", { name: "Start" })).toBeVisible({ timeout: 15_000 });
  await page.locator(".section-tabs").getByRole("link", { name: "Activity" }).click();
  await page.locator("#activity-list").getByRole("link", { name: /Undo restore/ }).first().click();
  await expect(page.locator("main")).toContainText("Succeeded");
  await expect(page.locator("main")).toContainText(
    `Ludock finished undoing a restore that was stopped partway: the server’s previous data is back in place. Anything Ludock moved out of its way went to the folder ludock-set-aside-${restoreID} beside it.`);
});
