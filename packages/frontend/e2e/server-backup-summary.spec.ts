import { test, expect, RUNNING_NAME } from "./fixtures";

test("server backup summaries remain readable and refresh after a successful backup", async ({ app, page }, testInfo) => {
  const createdAt = Date.UTC(2026, 8, 14, 10, 30);
  app.servers[0].latestBackup = { createdAt, size: 1024 };
  await app.open();
  const running = page.getByRole("article", { name: RUNNING_NAME });
  await expect(running.getByText(/Latest successful backup:/)).toBeVisible();
  await expect(running.locator("time")).toHaveAttribute("datetime", new Date(createdAt).toISOString());
  await expect(page.getByRole("article", { name: "Factorio weekend" }).getByText("No successful backup retained", { exact: true })).toBeVisible();

  for (const width of testInfo.project.name === "mobile" ? [390, 320] : [1360, 900]) {
    await page.setViewportSize({ width, height: 900 });
    await expect(running.getByText(/Latest successful backup:/)).toBeVisible();
    await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  }

  const nextCreatedAt = createdAt + 60_000;
  app.servers[0].latestBackup = { createdAt: nextCreatedAt, size: 2048 };
  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect(running.locator("time")).toHaveAttribute("datetime", new Date(nextCreatedAt).toISOString());
});

test("backup creator visibility follows each server grant without archive access", async ({ app, page }) => {
  app.user = { ...app.user!, role: "operator" };
  app.servers[0].permissions = ["server.view", "backups.create"];
  app.servers[0].latestBackup = { createdAt: Date.UTC(2026, 8, 14, 10, 30), size: 1024 };
  app.servers[1].permissions = ["server.view", "server.start"];
  // Even an obsolete summary must disappear as soon as the grant disappears.
  app.servers[1].latestBackup = app.servers[0].latestBackup;
  await app.open();
  await expect(page.getByRole("article", { name: RUNNING_NAME }).getByText(/Latest successful backup:/)).toBeVisible();
  await expect(page.getByRole("article", { name: "Factorio weekend" }).getByText(/successful backup/)).toHaveCount(0);
  await expect(page.getByRole("link", { name: "Download", exact: true })).toHaveCount(0);

  app.servers[0].permissions = ["server.view"];
  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect(page.getByText(/successful backup/)).toHaveCount(0);
});
