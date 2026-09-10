import type { Route } from "@playwright/test";
import { apiErrorSchema } from "@ludock/shared";
import { test, expect, RUNNING_ID, RUNNING_NAME } from "./fixtures";

test("folder creation and deletion name the target and require explicit confirmation", async ({ app, page }) => {
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

test("rename failures retain the dialog and draft for correction", async ({ app, page }) => {
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

test("canceling an upload stops the batch and keeps the selected folder", async ({ app, page }) => {
  const uploads: Route[] = [];
  await page.route(`**/api/v1/servers/${RUNNING_ID}/files/upload?*`, (route) => { uploads.push(route); });
  await app.open(`/files/${RUNNING_ID}`);
  await expect(page.getByRole("button", { name: "Choose files", exact: true })).toBeEnabled();
  await page.getByLabel("Upload files", { exact: true }).setInputFiles([
    { name: "first.txt", mimeType: "text/plain", buffer: Buffer.from("First fixture file") },
    { name: "second.txt", mimeType: "text/plain", buffer: Buffer.from("Second fixture file") },
  ]);
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
