import { test, expect, asAdmin, serverRow, expectOffline } from "./fixtures.mjs";

asAdmin();

function refreshList(page) {
  const response = page.waitForResponse(/\/fragments\/servers(\?|$)/);
  return page.evaluate(() => window.htmx.trigger(document.body, "servers-changed")).then(() => response);
}

test("a list refresh that changed nothing sends no list and keeps an open menu", async ({ page }) => {
  await page.goto("/");
  const first = await refreshList(page);
  expect(first.status()).toBe(200);
  const tag = first.headers()["etag"];
  expect(tag).toBeTruthy();

  const survival = serverRow(page, "Survival");
  await survival.getByRole("button", { name: /More actions/ }).click();
  const restart = survival.getByRole("button", { name: "Restart…" });
  await expect(restart).toBeVisible();

  const second = await refreshList(page);
  expect(second.request().headers()["if-none-match"]).toBe(tag);
  expect(second.status()).toBe(304);
  await expect(restart).toBeVisible();
  await expect(page.locator(".server-row")).toHaveCount(2);
});

test("losing the connection says so, and the page catches up when it returns", async ({ page, context }) => {
  expectOffline(page);
  await page.goto("/");
  await expect(page.locator(".server-row")).toHaveCount(2);
  const notice = page.locator("[data-connection-notice]");

  // A refresh that can't be sent keeps the list and says why; back online,
  // the list refreshes and the notice goes.
  await context.setOffline(true);
  await page.evaluate(() => window.htmx.trigger(document.body, "servers-changed"));
  await expect(notice).toContainText("Can’t reach Ludock. Showing the last known state");
  await expect(page.locator(".server-row")).toHaveCount(2);
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
