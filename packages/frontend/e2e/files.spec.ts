import type { Route } from "@playwright/test";
import { test, expect, RUNNING_ID } from "./fixtures";

test("files can be previewed, renamed in the chosen storage, and changed only after confirmation", async ({ app, page }) => {
  await page.route(`**/api/v1/servers/${RUNNING_ID}/files/download?*`, async (route) => {
    const path = new URL(route.request().url()).searchParams.get("path");
    // settings.yml grew past the preview limit after it was listed.
    await route.fulfill({ body: path === "settings.yml" ? "x".repeat(600 * 1024) : "motd=Friends\nmax-players=10\n" });
  });
  await app.open(`/files/${RUNNING_ID}`);
  await page.getByRole("button", { name: "server.properties", exact: true }).click();
  const preview = page.getByRole("dialog", { name: "server.properties" });
  await expect(preview.getByLabel("Contents of server.properties")).toContainText("max-players=10");
  await preview.getByRole("button", { name: "Close", exact: true }).click();
  await expect(preview).toHaveCount(0);

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
  await row.getByRole("button", { name: "Actions for plugins", exact: true }).click();
  await row.getByRole("button", { name: "Delete", exact: true }).click();
  const deletion = page.getByRole("dialog", { name: "Delete “plugins”?", exact: true });
  expect(app.requests.filter((request) => request.method === "DELETE")).toEqual([]);
  await deletion.getByRole("button", { name: "Delete folder", exact: true }).click();
  await expect(row).toHaveCount(0);
  expect(app.requests.find((request) => request.method === "DELETE")).toMatchObject({
    path: `/servers/${RUNNING_ID}/files`, query: { root: "data", path: "plugins" },
  });

  await page.getByRole("combobox", { name: "Storage location", exact: true }).selectOption("config");
  await page.getByRole("button", { name: "settings.yml", exact: true }).click();
  await expect(page.getByRole("dialog", { name: "settings.yml" })).toContainText("aren’t shown here");
  await page.getByRole("dialog", { name: "settings.yml" }).getByRole("button", { name: "Close", exact: true }).click();
  const settings = page.locator(".file-row").filter({ has: page.getByRole("button", { name: "settings.yml", exact: true }) });
  await settings.getByRole("button", { name: "Actions for settings.yml", exact: true }).click();
  await expect(settings.getByRole("link", { name: "Download", exact: true }))
    .toHaveAttribute("href", /root=config&path=settings\.yml/);
  await settings.getByRole("button", { name: "Rename", exact: true }).click();
  const rename = page.getByRole("dialog").filter({ hasText: "Friends’ survival world · Configuration" });
  await rename.getByRole("textbox", { name: "New name", exact: true }).fill("settings.old.yml");
  await rename.getByRole("button", { name: "Rename", exact: true }).click();
  await expect(page.getByRole("button", { name: "settings.old.yml", exact: true })).toBeVisible();
  expect(app.requests.find((request) => request.method === "PATCH")).toMatchObject({
    path: `/servers/${RUNNING_ID}/files/rename`,
    body: { root: "config", path: "settings.yml", newName: "settings.old.yml" },
  });
});

test("a change whose result is lost is reconciled from the folder before another action", async ({ app, page }) => {
  let listingFails = false;
  const writes: string[] = [];
  await page.route(`**/api/v1/servers/${RUNNING_ID}/files?*`, async (route) => {
    const method = route.request().method();
    if (method === "DELETE") {
      // The deletion completes, but its response never arrives.
      writes.push(method);
      app.files.set("data:", app.files.get("data:")!.filter((entry) => entry.name !== "server.properties"));
      await route.abort("connectionreset");
    } else if (listingFails) {
      await route.fulfill({ status: 503, json: { error: "Storage is temporarily unavailable." } });
    } else await route.fallback();
  });
  await page.route(`**/api/v1/servers/${RUNNING_ID}/files/directory`, async (route) => {
    writes.push(route.request().method());
    listingFails = true;
    await route.fulfill({ status: 201, json: { unexpected: true } });
  });
  await app.open(`/files/${RUNNING_ID}`);
  const properties = page.locator(".file-row").filter({ has: page.getByRole("button", { name: "server.properties", exact: true }) });
  await properties.getByRole("button", { name: "Actions for server.properties", exact: true }).click();
  await properties.getByRole("button", { name: "Delete", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Delete file", exact: true }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(page.getByRole("alert")).toContainText("Review the folder before starting another action");
  await expect(properties).toHaveCount(0);
  await expect(page.getByRole("button", { name: "world", exact: true })).toBeVisible();

  const create = page.getByRole("button", { name: "New folder", exact: true });
  await create.click();
  await page.getByRole("textbox", { name: "Folder name", exact: true }).fill("backups");
  await page.getByRole("button", { name: "Create folder", exact: true }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(page.getByText(/Unable to load this folder/)).toBeVisible();
  await expect(create).toBeDisabled();
  listingFails = false;
  await page.getByRole("button", { name: "Try again", exact: true }).click();
  await expect(page.getByRole("button", { name: "world", exact: true })).toBeVisible();
  await create.click();
  await expect(page.getByRole("textbox", { name: "Folder name", exact: true })).toHaveValue("backups");
  expect(writes).toEqual(["DELETE", "POST"]);
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
  await expect(page.getByRole("button", { name: "world", exact: true })).toBeDisabled();
  const back = page.getByRole("link", { name: "Back to server", exact: true });
  await expect(back).toHaveAttribute("aria-disabled", "true");
  await back.click({ force: true });
  await expect(page).toHaveURL(`/files/${RUNNING_ID}`);
  await page.getByRole("button", { name: "Cancel upload", exact: true }).click();
  await expect(page.getByRole("status").filter({ hasText: "Upload canceled." })).toBeVisible();
  await expect.poll(() => uploads[0].request().failure()).not.toBeNull();
  await uploads[0].abort();
  await expect(page.getByRole("button", { name: "Choose files", exact: true })).toBeEnabled();
  await expect(page.getByRole("combobox", { name: "Storage location", exact: true })).toHaveValue("data");
  expect(uploads).toHaveLength(1);
  expect(Object.fromEntries(new URL(uploads[0].request().url()).searchParams)).toEqual({
    root: "data", path: "", name: "first.txt",
  });
});
