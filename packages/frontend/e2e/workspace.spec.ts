import { test, expect, RUNNING_ID, RUNNING_NAME } from "./fixtures";

test("lifecycle cancellation sends nothing and confirmation submits once", async ({ app, page }) => {
  await app.open();
  const row = page.getByRole("article", { name: RUNNING_NAME });
  const stop = row.getByRole("button", { name: "Stop", exact: true });
  await stop.click();
  const dialog = page.getByRole("dialog", { name: `Stop ${RUNNING_NAME}?` });
  await expect(dialog).toBeVisible();
  expect(app.requests.filter((request) => request.method === "POST")).toEqual([]);
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  expect(app.requests.filter((request) => request.method === "POST")).toEqual([]);
  await stop.click();
  await page.getByRole("dialog").getByRole("button", { name: "Stop server", exact: true }).click();
  await expect.poll(() => app.requests.filter((request) => request.method === "POST")).toEqual([
    { method: "POST", path: `/servers/${RUNNING_ID}/stop`, query: {}, body: undefined },
  ]);
  await expect(row.getByRole("button", { name: "Start", exact: true })).toBeVisible();
});
