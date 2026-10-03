import type { Route } from "@playwright/test";
import { test, expect, RUNNING_ID } from "./fixtures";

test("folder creation and deletion require explicit confirmation", async ({ app, page }) => {
  await app.open(`/files/${RUNNING_ID}`);
  const create = page.getByRole("button", { name: "New folder", exact: true });
  await create.click();
  const dialog = page.getByRole("dialog", { name: "New folder", exact: true });
  await dialog.getByRole("textbox", { name: "Folder name" }).fill("plugins");
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  expect(app.requests.filter((request) => request.method !== "GET")).toEqual([]);
  await create.click();
  await dialog.getByRole("textbox", { name: "Folder name" }).fill("plugins");
  await dialog.getByRole("button", { name: "Create folder" }).click();
  const row = page.locator(".file-row").filter({ has: page.getByRole("button", { name: "plugins", exact: true }) });
  await expect(row).toBeVisible();
  expect(app.requests.find((request) => request.method === "POST")).toMatchObject({
    path: `/servers/${RUNNING_ID}/files/directory`,
    body: { root: "data", path: "", name: "plugins" },
  });
  await row.getByRole("button", { name: "Delete", exact: true }).click();
  const deletion = page.getByRole("dialog", { name: "Delete “plugins”?", exact: true });
  expect(app.requests.filter((request) => request.method === "DELETE")).toEqual([]);
  await deletion.getByRole("button", { name: "Delete folder", exact: true }).click();
  await expect(row).toHaveCount(0);
  expect(app.requests.find((request) => request.method === "DELETE")).toMatchObject({
    path: `/servers/${RUNNING_ID}/files`, query: { root: "data", path: "plugins" },
  });
});

test("changing storage roots cancels an older folder request and hides stale entries", async ({ app, page }) => {
  let pending: Route | undefined;
  await page.route(`**/api/v1/servers/${RUNNING_ID}/files?*`, async (route) => {
    const query = new URL(route.request().url()).searchParams;
    if (query.get("root") === "data" && query.get("path") === "world") pending = route;
    else await route.fallback();
  });
  await app.open(`/files/${RUNNING_ID}`);
  await page.getByRole("button", { name: "world", exact: true }).click();
  await expect.poll(() => Boolean(pending)).toBe(true);
  await expect(page.getByText("server.properties", { exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "New folder", exact: true })).toBeDisabled();
  await page.getByRole("combobox", { name: "Storage location", exact: true }).selectOption("config");
  await expect(page.getByText("settings.yml", { exact: true })).toBeVisible();
  await expect.poll(() => pending?.request().failure()).not.toBeNull();
  await pending!.abort();
  await expect(page.getByRole("combobox", { name: "Storage location", exact: true })).toHaveValue("config");
  await expect(page.getByText("level.dat", { exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "New folder", exact: true })).toBeEnabled();
});

test("canceling an upload stops the batch and keeps the selected folder", async ({ app, page }, testInfo) => {
  const uploads: Route[] = [];
  await page.route(`**/api/v1/servers/${RUNNING_ID}/files/upload?*`, (route) => { uploads.push(route); });
  await app.open(`/files/${RUNNING_ID}`);
  await expect(page.getByRole("button", { name: "Choose files", exact: true })).toBeEnabled();
  const first = testInfo.outputPath("first.txt");
  const second = testInfo.outputPath("second.txt");
  await Bun.write(first, "First fixture file");
  await Bun.write(second, "Second fixture file");
  await page.getByLabel("Upload files", { exact: true }).setInputFiles([first, second]);
  await expect.poll(() => uploads.length).toBe(1);
  await expect(page.getByRole("button", { name: "New folder", exact: true })).toBeDisabled();
  await expect(page.getByRole("combobox", { name: "Storage location", exact: true })).toBeDisabled();
  await page.getByRole("button", { name: "Cancel upload", exact: true }).click();
  await expect.poll(() => uploads[0].request().failure()).not.toBeNull();
  await uploads[0].abort();
  await expect(page.getByRole("button", { name: "Choose files", exact: true })).toBeEnabled();
  await expect(page.getByRole("combobox", { name: "Storage location", exact: true })).toHaveValue("data");
  expect(uploads).toHaveLength(1);
  expect(Object.fromEntries(new URL(uploads[0].request().url()).searchParams)).toEqual({
    root: "data", path: "", name: "first.txt",
  });
});
