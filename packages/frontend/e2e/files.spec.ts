import type { Route } from "@playwright/test";
import { apiErrorSchema } from "@ludock/shared";
import { test, expect, RUNNING_ID, RUNNING_NAME } from "./fixtures";

test("filename filters and sort controls work locally with keyboard and touch", { tag: "@responsive" }, async ({ app, page }) => {
  app.files.set("data:", [
    { name: "world", type: "directory", size: 0, modifiedAt: 3_000 },
    { name: "server.properties", type: "file", size: 342, modifiedAt: 1_000 },
    { name: "backup10.zip", type: "file", size: 2_048, modifiedAt: 2_000 },
    { name: "backup2.zip", type: "file", size: 4_096, modifiedAt: 3_000 },
  ]);
  await app.open(`/files/${RUNNING_ID}`);
  await expect(page.getByText("Showing 4 of 4 entries", { exact: true })).toBeVisible();
  const search = page.getByRole("searchbox", { name: "Filter filenames", exact: true });
  const sort = page.getByRole("combobox", { name: "Sort by", exact: true });
  const names = page.locator(".file-row__name > span:not(.file-row__icon), .file-row__name > button");
  const reads = app.requests.filter((request) => request.path.endsWith("/files")).length;
  await sort.selectOption("size-desc");
  await expect(names).toHaveText(["world", "backup2.zip", "backup10.zip", "server.properties"]);
  await search.fill("BACKUP");
  await expect(names).toHaveText(["backup2.zip", "backup10.zip"]);
  await expect(page.getByText("Showing 2 of 4 entries", { exact: true })).toBeVisible();
  expect(app.requests.filter((request) => request.path.endsWith("/files"))).toHaveLength(reads);

  const clear = page.getByRole("button", { name: "Clear filter", exact: true });
  for (const control of [search, sort, clear]) {
    const box = await control.boundingBox();
    expect(box?.height).toBeGreaterThanOrEqual(44);
  }
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);

  await search.fill("missing-file");
  await expect(page.getByText("No filenames match “missing-file”.", { exact: true })).toBeVisible();
  await expect(page.getByText("This folder is empty.", { exact: true })).toHaveCount(0);
  await clear.focus();
  await page.keyboard.press("Enter");
  await expect(search).toBeFocused();
  await expect(search).toHaveValue("");
  await expect(names).toHaveCount(4);
  expect(app.requests.filter((request) => request.method !== "GET")).toEqual([]);
});

test("folder creation and deletion name the target and require explicit confirmation", { tag: "@responsive" }, async ({ app, page }) => {
  await app.open(`/files/${RUNNING_ID}`);
  const create = page.getByRole("button", { name: "New folder", exact: true });
  await create.click();
  const dialog = page.getByRole("dialog", { name: "New folder", exact: true });
  await expect(dialog.getByRole("textbox", { name: "Folder name" })).toBeFocused();
  await expect(dialog).toHaveAccessibleDescription(new RegExp(`${RUNNING_NAME}.*Data/?`));
  await dialog.getByRole("textbox", { name: "Folder name" }).fill("plugins");
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(create).toBeFocused();
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
  await expect(deletion.getByRole("button", { name: "Cancel", exact: true })).toBeFocused();
  await expect(deletion.getByText(/everything inside it/)).toBeVisible();
  expect(app.requests.filter((request) => request.method === "DELETE")).toEqual([]);
  await deletion.getByRole("button", { name: "Delete folder", exact: true }).click();
  await expect(row).toHaveCount(0);
  expect(app.requests.find((request) => request.method === "DELETE")).toMatchObject({
    path: `/servers/${RUNNING_ID}/files`, query: { root: "data", path: "plugins" },
  });
});

test("rename failures retain the dialog and draft for correction", { tag: "@responsive" }, async ({ app, page }) => {
  let failedOnce = false;
  await page.route(`**/api/v1/servers/${RUNNING_ID}/files/rename`, async (route) => {
    if (!failedOnce) {
      failedOnce = true;
      await route.fulfill({ status: 409, json: apiErrorSchema.parse({ error: "That name already exists." }) });
    } else await route.fallback();
  });
  await app.open(`/files/${RUNNING_ID}`);
  const row = page.locator(".file-row").filter({ has: page.getByText("server.properties", { exact: true }) });
  await row.getByRole("button", { name: "Rename", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Rename “server.properties”", exact: true });
  const name = dialog.getByRole("textbox", { name: "New name", exact: true });
  await expect(name).toBeFocused();
  await expect(name).toHaveValue("server.properties");
  await name.fill("existing.properties");
  await dialog.getByRole("button", { name: "Rename", exact: true }).click();
  await expect(dialog.getByRole("alert")).toHaveText("That name already exists.");
  await expect(name).toHaveValue("existing.properties");
  await name.fill("renamed.properties");
  await dialog.getByRole("button", { name: "Rename", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(page.getByText("renamed.properties", { exact: true })).toBeVisible();
  expect(app.requests.find((request) => request.method === "PATCH")).toMatchObject({
    path: `/servers/${RUNNING_ID}/files/rename`,
    body: { root: "data", path: "server.properties", newName: "renamed.properties" },
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
  await expect(page.getByText("Loading folder…", { exact: true })).toBeVisible();
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
  await expect(page.getByText("Uploading 1 of 2: first.txt", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "New folder", exact: true })).toBeDisabled();
  await expect(page.getByRole("combobox", { name: "Storage location", exact: true })).toBeDisabled();
  await page.getByRole("button", { name: "Cancel upload", exact: true }).click();
  await expect(page.getByText(/Upload canceled\. 0 of 2 files confirmed complete/)).toBeVisible();
  await expect.poll(() => uploads[0].request().failure()).not.toBeNull();
  await uploads[0].abort();
  await expect(page.getByRole("button", { name: "Choose files", exact: true })).toBeEnabled();
  await expect(page.getByRole("combobox", { name: "Storage location", exact: true })).toHaveValue("data");
  expect(uploads).toHaveLength(1);
  expect(Object.fromEntries(new URL(uploads[0].request().url()).searchParams)).toEqual({
    root: "data", path: "", name: "first.txt",
  });
});
