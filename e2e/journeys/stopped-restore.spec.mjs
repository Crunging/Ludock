import { cpSync, existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { test, expect, asAdmin, openServer, serverRow, expectOperationSucceeded, fake, createUser, shareServer, signIn } from "./fixtures.mjs";

asAdmin();

// Another manager starts the server between a restore moving its data out
// and the backup's in. The restore stops and says where the data is. While
// the server's data folder points elsewhere, the undo is refused and says
// how to get out, and no page offers a backup or an undo that would be
// refused. The game, finding its folder emptied, keeps exiting and
// being restarted by Docker: its page says so and offers Stop, and tells
// someone who can only see it what is happening, without steps. Needs
// attention offers to undo the restore, which stops the server without
// warning anyone, puts the data back, keeps what the game wrote meanwhile,
// and starts the server again. Stop works on a server that keeps restarting,
// even caught in one of the loop's brief runs.
test("undoing a restore that was stopped partway", async ({ page, session, ownServer: server }) => {
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

  // While the server points at another copy of its data, the undo is refused
  // with what to do, and the server's page says so instead of offering
  // backups.
  const moved = server.data + "-moved";
  cpSync(server.data, moved, { recursive: true });
  await fake("/fake/recreate", { container: server.container, mount: `${server.data}=${moved}` });
  await openServer(page, server.name);
  await expect(page.locator(".alert--warning")).toContainText("the server’s data folders or game have changed since");
  await page.locator(".section-tabs").getByRole("link", { name: "Backups" }).click();
  await expect(page.getByRole("button", { name: "Create backup" })).toHaveCount(0);
  await expect(page.locator("#backup-list").getByRole("link", { name: "Restore…" })).toHaveCount(0);
  // Its Schedules form doesn't back up first by default and says what such a
  // schedule would do, and the server list's restart doesn't offer it.
  await page.locator(".section-tabs").getByRole("link", { name: "Schedules" }).click();
  const schedule = page.locator("#schedule-form");
  await expect(schedule.getByLabel("Back up first")).not.toBeChecked();
  await expect(schedule).toContainText("Until the earlier restore is undone, a scheduled backup, or a restart that backs up first, is refused when it runs");
  await page.goto("/");
  const row = serverRow(page, server.name);
  await row.getByRole("button", { name: /More actions/ }).click();
  await row.getByRole("button", { name: "Restart…" }).click();
  const restart = page.getByRole("dialog", { name: `Restart ${server.name}?` });
  await expect(restart.getByRole("button", { name: "Restart server" })).toBeVisible();
  await expect(restart.getByLabel("Back up first")).toHaveCount(0);
  await restart.getByRole("button", { name: "Cancel" }).click();
  // Needs attention says what to do instead of offering an undo that would
  // be refused.
  const item = page.locator(".needs-attention__list li", { hasText: "Restore stopped partway" });
  await expect(item).toContainText(`The restore of ${server.name} stopped partway, and its data folders or game changed since. ` +
    "Put them back as they were where the server is defined, then undo the restore.");
  await expect(item.getByRole("button", { name: "Undo the restore" })).toHaveCount(0);
  await expect(item.getByRole("link", { name: /Review server/ })).toBeVisible();
  // An open Update tab doesn't suggest backing up first, and follows once
  // the folders are back.
  await openServer(page, server.name);
  await page.locator(".section-tabs").getByRole("link", { name: "Update" }).click();
  await expect(page.locator("#update-panel h2")).toBeVisible();
  await expect(page.locator("#update-panel")).not.toContainText("tick Back up first");
  await fake("/fake/recreate", { container: server.container, mount: `${server.data}=${server.data}` });
  await expect(page.locator(".alert--warning")).toHaveCount(0, { timeout: 15_000 });
  await expect(page.locator("#update-panel")).toContainText("tick Back up first");

  // Started on the emptied folder, the game keeps exiting.
  await fake("/fake/crashloop", { container: server.container });
  // Pages reuse discovery for two seconds, and the container is new.
  await expect(async () => {
    await openServer(page, server.name);
    await expect(page.locator(".detail-header__identity")).toContainText("Keeps restarting", { timeout: 1_000 });
  }).toPass({ timeout: 15_000 });
  await expect(page.locator("#detail-live")).toContainText("The game keeps stopping and Docker keeps starting it again.");
  await expect(page.locator(".detail-actions").getByRole("button", { name: "Stop" })).toBeEnabled();
  await expect(page.locator(".detail-actions").getByRole("button", { name: "Restart" })).toHaveCount(0);
  // Someone who can only see it is told what is happening, without steps
  // they can't take.
  const watcher = "watcher-" + server.container;
  await createUser(page, watcher, "watcher password one");
  await shareServer(page, server.name, watcher, "Can see it");
  const member = await session({ storageState: { cookies: [], origins: [] } });
  await signIn(member, watcher, "watcher password one");
  await openServer(member, server.name);
  await expect(member.locator("#detail-live")).toContainText("The game keeps stopping and Docker keeps starting it again.");
  await expect(member.locator("#detail-live")).not.toContainText("Stop it, check its logs");

  await page.goto("/");
  await expect(item).toContainText(`The restore of ${server.name} was stopped partway. Its previous data is safe but not in place: undo the restore to put it back.`);
  await item.getByRole("button", { name: "Undo the restore" }).click();
  const dialog = page.getByRole("dialog", { name: `Undo the restore of ${server.name}?` });
  await expect(dialog).toContainText("even though it was started outside Ludock");
  await expect(dialog).toContainText(`ludock-set-aside-${restoreID}`);
  await dialog.getByRole("button", { name: "Undo the restore" }).click();
  await expect(page.locator("#action-error")).toContainText(`Undoing the restore of ${server.name}.`);

  const aside = join(server.data, `ludock-set-aside-${restoreID}`, "generated.txt");
  // The undo moves the file aside before it puts the previous one back, so
  // for a moment there is none to read.
  await expect.poll(() => {
    try {
      return existsSync(aside) && readFileSync(marker, "utf8");
    } catch {
      return "";
    }
  }, { timeout: 45_000 }).toBe("current");
  expect(readFileSync(aside, "utf8")).toBe("written meanwhile");
  await expect(item).toHaveCount(0, { timeout: 15_000 });
  await expect(page.locator(".needs-attention__list li", { hasText: "Restore failed, then undone" })).toBeVisible();

  await openServer(page, server.name);
  await expect(page.locator(".detail-actions").getByRole("button", { name: "Stop" })).toBeVisible();
  // Caught in one of the loop's brief runs, it still reads as the loop, and
  // Stop doesn't offer to warn players nobody can join.
  await fake("/fake/crashloop", { container: server.container });
  await expect(page.locator(".detail-header__identity")).toContainText("Keeps restarting");
  await fake("/fake/crashloop", { container: server.container, run: "1" });
  // Past the two seconds pages reuse discovery for, so the page sees the run.
  await page.waitForTimeout(2_500);
  await openServer(page, server.name);
  await expect(page.locator(".detail-header__identity")).toContainText("Keeps restarting");
  await expect(page.locator(".detail-actions").getByRole("button", { name: "Restart" })).toHaveCount(0);
  await page.locator(".detail-actions").getByRole("button", { name: "Stop" }).click();
  await expect(page.getByRole("dialog").getByRole("button", { name: "Stop server" })).toBeVisible();
  await expect(page.getByRole("dialog")).not.toContainText("Warn players");
  await page.getByRole("dialog").getByRole("button", { name: "Stop server" }).click();
  await expect(page.locator(".detail-actions").getByRole("button", { name: "Start" })).toBeVisible({ timeout: 15_000 });
  await page.locator(".section-tabs").getByRole("link", { name: "Activity" }).click();
  await page.locator("#activity-list").getByRole("link", { name: /Undo restore/ }).first().click();
  await expect(page.locator("main")).toContainText("Succeeded");
  await expect(page.locator("main")).toContainText(
    `Ludock finished undoing a restore that was stopped partway: the server’s previous data is back in place. Anything Ludock moved out of its way went to the folder ludock-set-aside-${restoreID} beside it.`);
});
