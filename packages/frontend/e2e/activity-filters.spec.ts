import { operationsResponseSchema } from "@ludock/shared";
import { test, expect, RUNNING_ID } from "./fixtures";

const operations = operationsResponseSchema.parse({ operations: [
  { id: "11111111-1111-4111-8111-111111111111", serverId: RUNNING_ID, kind: "backup", status: "running", phase: "Copying game data", createdAt: 0, updatedAt: 0, error: null, result: null },
  { id: "22222222-2222-4222-8222-222222222222", serverId: RUNNING_ID, kind: "restart", status: "failed", phase: "Restarting", createdAt: 0, updatedAt: 0, error: "Restart failed", result: null },
  { id: "33333333-3333-4333-8333-333333333333", serverId: RUNNING_ID, kind: "stop", status: "succeeded", phase: "Complete", createdAt: 0, updatedAt: 0, error: null, result: null },
] }).operations;

test("activity filters reveal active work and restore keyboard focus", { tag: "@responsive" }, async ({ app, page }) => {
  await page.route(`**/api/v1/servers/${RUNNING_ID}/operations`, (route) => route.fulfill({ json: operationsResponseSchema.parse({ operations }) }));
  await app.open(`/servers/${RUNNING_ID}`);
  const status = page.getByRole("combobox", { name: "Status", exact: true });
  const kind = page.getByRole("combobox", { name: "Operation", exact: true });
  const table = page.getByRole("table", { name: "Recent operations" });
  await expect(page.getByText("Showing 3 of 3 recent operations.")).toBeVisible();
  await status.selectOption("failed");
  await kind.selectOption("restart");
  await expect(table.getByRole("row")).toHaveCount(2);
  await expect(table).toContainText("Restart failed");
  await expect(page.getByRole("button", { name: "Stop", exact: true })).toBeDisabled();
  const reveal = page.getByRole("button", { name: "Show active work", exact: true });
  await reveal.focus();
  await page.keyboard.press("Enter");
  await expect(status).toBeFocused();
  await expect(status).toHaveValue("all");
  await expect(kind).toHaveValue("all");
  await expect(table).toContainText("Copying game data");
  expect(app.requests.filter((request) => request.method !== "GET")).toEqual([]);
});

test("activity filter controls fit narrow screens", { tag: "@mobile" }, async ({ app, page }, testInfo) => {
  await page.route(`**/api/v1/servers/${RUNNING_ID}/operations`, (route) => route.fulfill({ json: operationsResponseSchema.parse({ operations }) }));
  await page.setViewportSize({ width: 320, height: 844 });
  await app.open(`/servers/${RUNNING_ID}`);
  await page.getByRole("combobox", { name: "Status", exact: true }).selectOption("interrupted");
  const filters = page.getByRole("group", { name: "Filter recent operations" });
  await expect(filters).toBeVisible();
  const heights = await filters.locator("select, button").evaluateAll((elements) => elements.map((element) => element.getBoundingClientRect().height));
  for (const height of heights) expect(height).toBeGreaterThanOrEqual(44);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.screenshot({ path: testInfo.outputPath("activity-filters-320.png"), fullPage: true });
  await page.getByRole("button", { name: "Clear filters", exact: true }).click();
  await expect(page.getByText("Showing 3 of 3 recent operations.")).toBeVisible();
});
