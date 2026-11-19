import { test, expect, asAdmin, serverRow, expectOffline } from "./fixtures.mjs";

asAdmin();

function refreshList(page) {
  const response = page.waitForResponse(/\/fragments\/servers(\?|$)/);
  return page.evaluate(() => window.htmx.trigger(document.body, "servers-changed")).then(() => response);
}

test("a list refresh that changed nothing sends no list and keeps an open menu", async ({ page }) => {
  await page.goto("/");
  const survival = serverRow(page, "Survival");
  // The first player readings land in the background and change the list.
  await expect.poll(async () => {
    await refreshList(page);
    return page.locator(".server-row").getByText("Checking…", { exact: true }).count();
  }).toBe(0);
  const first = await refreshList(page);
  const tag = first.headers()["etag"];
  expect(tag).toBeTruthy();

  await survival.getByRole("button", { name: /More actions/ }).click();
  const restart = survival.getByRole("button", { name: "Restart…" });
  await expect(restart).toBeVisible();

  const second = await refreshList(page);
  expect(second.request().headers()["if-none-match"]).toBe(tag);
  expect(second.status()).toBe(304);
  await expect(restart).toBeVisible();
  await expect(page.locator(".server-row")).toHaveCount(3);
});

test("losing the connection says so, and the page catches up when it returns", async ({ page, context }) => {
  expectOffline(page);
  await page.goto("/");
  await expect(page.locator(".server-row")).toHaveCount(3);
  const notice = page.locator("[data-connection-notice]");

  // A refresh that can't be sent keeps the list and says why; back online,
  // the list refreshes and the notice goes.
  await context.setOffline(true);
  await page.evaluate(() => window.htmx.trigger(document.body, "servers-changed"));
  await expect(notice).toContainText("Can’t reach Ludock. Showing the last known state");
  await expect(page.locator(".server-row")).toHaveCount(3);
  let caughtUp = page.waitForResponse(/\/fragments\/servers(\?|$)/);
  await context.setOffline(false);
  await caughtUp;
  await expect(notice).toBeEmpty();

  // A change that can't be sent may not have happened, which stays said
  // after refreshes until it is dismissed.
  await context.setOffline(true);
  const valheim = serverRow(page, "valheim");
  await valheim.getByRole("button", { name: "Start" }).click();
  await expect(notice).toContainText("Ludock didn’t answer, so your change may not have been made.");
  await expect(valheim.getByRole("button", { name: "Start" })).toBeEnabled();
  caughtUp = page.waitForResponse(/\/fragments\/servers(\?|$)/);
  await context.setOffline(false);
  await caughtUp;
  await expect(notice).toContainText("your change may not have been made");
  await notice.getByRole("button", { name: "Dismiss" }).click();
  await expect(notice).toBeEmpty();
  await expect(valheim).toContainText("Stopped");
});

test("a change that fails after a refresh replaced its button still says so", async ({ page }) => {
  expectOffline(page);
  await page.goto("/");
  let fail;
  const failing = new Promise((resolve) => { fail = resolve; });
  await page.route(/\/servers\/[^/]+\/start$/, async (route) => {
    await failing;
    await route.abort("internetdisconnected");
  });
  const valheim = serverRow(page, "valheim");
  await valheim.getByRole("button", { name: "Start" }).click();
  await expect(valheim.getByRole("button", { name: "Working…" })).toBeDisabled();
  // The refresh replaces the busy button with a fresh one.
  expect((await refreshList(page)).status()).toBe(200);
  await expect(valheim.getByRole("button", { name: "Start" })).toBeEnabled();
  fail();
  await expect(page.locator("[data-connection-notice]")).toContainText("your change may not have been made");
});

test("a refresh queued behind another is checked against what that one showed", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator(".server-row")).toHaveCount(3);
  await expect.poll(async () => (await refreshList(page)).status()).toBe(304);
  // The next refresh shows other content; one more queues behind it while it loads.
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  let requests = 0, queuedTag;
  await page.route(/\/fragments\/servers(\?|$)/, async (route) => {
    if (++requests > 1) {
      queuedTag ??= route.request().headers()["if-none-match"];
      return route.continue();
    }
    await held;
    await route.fulfill({ status: 200, contentType: "text/html; charset=utf-8", headers: { ETag: '"other"' }, body: "<p>Other content</p>" });
  });
  await page.evaluate(() => window.htmx.trigger(document.body, "servers-changed"));
  await expect.poll(() => requests).toBe(1);
  await page.evaluate(() => window.htmx.trigger(document.body, "servers-changed"));
  release();
  await expect.poll(() => queuedTag).toBe('"other"');
  await expect(page.locator(".server-row")).toHaveCount(3);
  await page.unroute(/\/fragments\/servers(\?|$)/);
});
