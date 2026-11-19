import { randomBytes } from "node:crypto";
import { chmodSync, mkdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { test, expect, asAdmin, serverRow, signIn, createUser } from "./fixtures.mjs";

asAdmin();

test("browsing, previewing, uploading, and changing server files", async ({ page }) => {
  await page.goto("/");
  await serverRow(page, "Survival").getByRole("button", { name: /More actions/ }).click();
  await serverRow(page, "Survival").getByRole("link", { name: "Files" }).click();
  const names = page.locator(".file-row__name");
  await expect(names).toHaveText(["world", "server.properties"]);

  await page.getByRole("button", { name: "server.properties", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.locator("pre")).toContainText("motd=Hello from minecraft");
  await dialog.getByRole("button", { name: "Close" }).click();

  // Upload a text file and one past the preview limit.
  await page.locator("[data-upload-input]").setInputFiles([
    { name: "notes.txt", mimeType: "text/plain", buffer: Buffer.from("uploaded notes\n") },
    { name: "big.log", mimeType: "text/plain", buffer: Buffer.from(randomBytes(400 * 1024).toString("hex")) },
  ]);
  await expect(page.locator("#file-feedback")).toHaveText("2 files uploaded.");
  await expect(names).toHaveText(["world", "big.log", "notes.txt", "server.properties"]);
  await page.getByRole("button", { name: "big.log", exact: true }).click();
  await expect(dialog).toContainText("Files over 512 KiB aren’t shown here.");
  await dialog.getByRole("button", { name: "Close" }).click();

  // A file over the upload limit is refused.
  await page.locator("[data-upload-input]").setInputFiles([{ name: "huge.bin", mimeType: "application/octet-stream", buffer: Buffer.alloc(6 * 1024 * 1024) }]);
  await expect(page.locator("#file-feedback")).toContainText("Could not upload “huge.bin”: File exceeds the upload size limit of 5 MiB");

  const [download] = await Promise.all([page.waitForEvent("download"), (async () => {
    await page.locator(".file-row", { hasText: "notes.txt" }).getByRole("button", { name: "Actions for notes.txt", exact: true }).click();
    await page.getByRole("link", { name: "Download" }).click();
  })()]);
  expect(download.suggestedFilename()).toBe("notes.txt");
  expect(readFileSync(await download.path(), "utf8")).toBe("uploaded notes\n");

  // Folders: browse into one, sort naturally, and filter.
  await page.getByRole("link", { name: "world" }).click();
  await expect(names).toHaveText(["level.dat"]);
  await page.getByRole("button", { name: "level.dat", exact: true }).click();
  await expect(dialog).toContainText("This file can’t be shown as text.");
  await dialog.getByRole("button", { name: "Close" }).click();
  await page.locator(".file-breadcrumbs").getByRole("link", { name: "minecraft" }).click();

  await page.getByRole("button", { name: "New folder" }).click();
  await dialog.getByLabel("Folder name").fill("world");
  await dialog.getByRole("button", { name: "Create folder" }).click();
  await expect(dialog.locator(".alert--error")).toHaveText("A file or folder with that name already exists");
  await dialog.getByLabel("Folder name").fill("mods");
  await dialog.getByRole("button", { name: "Create folder" }).click();
  await expect(page.locator("#file-feedback")).toHaveText("Created “mods”.");

  await page.locator(".file-row", { hasText: "mods" }).getByRole("button", { name: "Actions for mods" }).click();
  await page.getByRole("button", { name: "Rename" }).click();
  await dialog.getByLabel("New name").fill("world");
  await dialog.getByRole("button", { name: "Rename" }).click();
  await expect(dialog.locator(".alert--error")).toHaveText("A file or folder with that name already exists");
  await dialog.getByLabel("New name").fill("plugins");
  await dialog.getByRole("button", { name: "Rename" }).click();
  await expect(page.locator("#file-feedback")).toHaveText("Renamed “mods” to “plugins”.");

  for (const name of ["plugins", "notes.txt", "big.log"]) {
    await page.locator(".file-row", { hasText: name }).getByRole("button", { name: `Actions for ${name}` }).click();
    await page.getByRole("button", { name: "Delete" }).click();
    await dialog.getByRole("button", { name: /^Delete (folder|file)$/ }).click();
    await expect(page.locator("#file-feedback")).toHaveText(`Deleted “${name}”.`);
  }
  await page.getByLabel("Filter filenames").fill("server");
  await expect(names).toHaveText(["server.properties"]);

  // Refreshing keeps the filter and order chosen since the page loaded.
  await page.getByLabel("Sort by").selectOption("name-desc");
  await expect(page).toHaveURL(/sort=name-desc/);
  await page.getByRole("button", { name: "Refresh" }).click();
  await expect(page).toHaveURL(/q=server/);
  await expect(names).toHaveText(["server.properties"]);
  await expect(page.locator(".file-filter-summary")).toContainText("Showing 1 of 2 entries");

  // The page names the server it belongs to and leads back to it.
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Files");
  await page.getByRole("link", { name: "Survival", exact: true }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Survival" })).toBeVisible();
});

test("browsing a large folder a page at a time", async ({ page }) => {
  // The journeys' fake daemon keeps Survival's data under the e2e directory.
  const folder = join(process.env.LUDOCK_E2E_DIR, "docker", "data", "minecraft", "crowded");
  mkdirSync(folder);
  try {
    for (let index = 1; index <= 520; index++) writeFileSync(join(folder, `chunk-${index}.dat`), "");
    await page.goto("/");
    await serverRow(page, "Survival").getByRole("button", { name: /More actions/ }).click();
    await serverRow(page, "Survival").getByRole("link", { name: "Files" }).click();
    await page.getByRole("link", { name: "crowded" }).click();
    const names = page.locator(".file-row__name");
    await expect(page.getByRole("status").filter({ hasText: "entries" })).toHaveText("Showing 500 of 520 entries");
    await expect(names).toHaveCount(500);
    await expect(names.last()).toHaveText("chunk-500.dat");
    await page.getByRole("button", { name: "Show 20 more" }).click();
    await expect(names).toHaveCount(520);
    await expect(names.last()).toHaveText("chunk-520.dat");
    await expect(page.getByRole("button", { name: /Show \d+ more/ })).toHaveCount(0);
    // A filter applies to the whole folder, not just the page shown.
    await page.getByLabel("Filter filenames").fill("chunk-51");
    await expect(names).toHaveText(["chunk-51.dat", ...Array.from({ length: 10 }, (_, index) => `chunk-${510 + index}.dat`)]);
  } finally {
    rmSync(folder, { recursive: true, force: true });
  }
});

test("the last sort chosen while a filter is loading applies", async ({ page }) => {
  await page.goto("/");
  await serverRow(page, "Survival").getByRole("button", { name: /More actions/ }).click();
  await serverRow(page, "Survival").getByRole("link", { name: "Files" }).click();
  await expect(page.locator(".file-row__name").first()).toBeVisible();
  // Holds the filter's answer, so two sorts are chosen while it loads.
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  let waiting = false;
  await page.route("**/fragments/files/**", async (route) => {
    if (!waiting) {
      waiting = true;
      await held;
    }
    await route.continue();
  });
  await page.getByLabel("Filter filenames").fill("o");
  await expect.poll(() => waiting).toBe(true);
  await page.getByLabel("Sort by").selectOption("name-desc");
  await page.getByLabel("Sort by").selectOption("size-desc");
  release();
  await expect(page).toHaveURL(/q=o&root=[^&]+&sort=size-desc$/);
  await expect(page.getByLabel("Sort by")).toHaveValue("size-desc");
  await page.unroute("**/fragments/files/**");
});

// The journeys' fake daemon keeps Survival's data under the e2e directory.
const survivalData = () => join(process.env.LUDOCK_E2E_DIR, "docker", "data", "minecraft");

async function openFiles(page) {
  await page.goto("/");
  await serverRow(page, "Survival").getByRole("button", { name: /More actions/ }).click();
  await serverRow(page, "Survival").getByRole("link", { name: "Files" }).click();
  await expect(page.locator(".file-row__name").first()).toBeVisible();
}

test("editing a text file in the browser and saving it", async ({ page }) => {
  const properties = join(survivalData(), "server.properties");
  const original = readFileSync(properties);
  const crlf = join(survivalData(), "ops.cfg");
  writeFileSync(crlf, "﻿op=alex\r\nlevel=4\r\n");
  chmodSync(crlf, 0o640);
  try {
    await openFiles(page);
    await page.getByRole("button", { name: "server.properties", exact: true }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: "Edit" }).click();
    await expect(dialog).toContainText("A running game may overwrite this file, or ignore the change until it restarts.");
    const editor = dialog.getByRole("textbox", { name: "Edit server.properties" });
    await expect(editor).toHaveValue(original.toString("utf8"));
    // Text over the limit is refused and stays in the editor to shorten.
    const long = "x".repeat(520 * 1024);
    await editor.fill(long);
    await dialog.getByRole("button", { name: "Save" }).click();
    await expect(dialog.locator(".alert--error")).toHaveText("The file would be over 512 KiB, so it can’t be saved here.");
    expect(await editor.inputValue()).toBe(long);
    expect(readFileSync(properties)).toEqual(original);
    // Exactly what is typed is saved, including no final newline.
    await editor.fill("\nmotd=Edited in the browser\nmax-players=5");
    await dialog.getByRole("button", { name: "Save" }).click();
    await expect(dialog.locator(".alert--success")).toHaveText("Saved “server.properties”.");
    await expect(dialog.locator("pre")).toContainText("motd=Edited in the browser");
    expect(readFileSync(properties, "utf8")).toBe("\nmotd=Edited in the browser\nmax-players=5");
    await dialog.getByRole("button", { name: "Close" }).click();

    // A file with CRLF line endings and a byte order mark keeps both, and its mode.
    await page.getByRole("button", { name: "Refresh" }).click();
    await page.getByRole("button", { name: "ops.cfg", exact: true }).click();
    await dialog.getByRole("button", { name: "Edit" }).click();
    await expect(dialog.getByRole("textbox")).toHaveValue("op=alex\nlevel=4\n");
    await dialog.getByRole("textbox").fill("op=alex\nlevel=2\n");
    await dialog.getByRole("button", { name: "Save" }).click();
    await expect(dialog.locator(".alert--success")).toBeVisible();
    expect(readFileSync(crlf, "utf8")).toBe("﻿op=alex\r\nlevel=2\r\n");
    expect(statSync(crlf).mode & 0o777).toBe(0o640);

    // Cancel leaves the file as it was, and Save waits while it loads.
    await dialog.getByRole("button", { name: "Edit" }).click();
    await dialog.getByRole("textbox").fill("discarded");
    let release;
    const held = new Promise((resolve) => { release = resolve; });
    await page.route("**/fragments/files/**/preview?*", async (route) => { await held; await route.continue(); });
    await dialog.getByRole("button", { name: "Cancel" }).click();
    await expect(dialog.getByRole("button", { name: "Save" })).toBeDisabled();
    release();
    await expect(dialog.locator("pre")).toContainText("level=2");
    await page.unroute("**/fragments/files/**/preview?*");
    expect(readFileSync(crlf, "utf8")).toBe("﻿op=alex\r\nlevel=2\r\n");
  } finally {
    writeFileSync(properties, original);
    rmSync(crlf, { force: true });
  }
});

test("a save is refused when the file changed after the editor opened", async ({ page }) => {
  const properties = join(survivalData(), "server.properties");
  const original = readFileSync(properties);
  try {
    await openFiles(page);
    await page.getByRole("button", { name: "server.properties", exact: true }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: "Edit" }).click();
    const editor = dialog.getByRole("textbox", { name: "Edit server.properties" });
    await editor.fill("motd=Mine\n");
    // The game rewrites the file meanwhile.
    writeFileSync(properties, "motd=Written by the game\n");
    await dialog.getByRole("button", { name: "Save" }).click();
    await expect(dialog.locator(".alert--error")).toContainText("The file changed since you opened it, so your edit was not saved.");
    await expect(editor).toHaveValue("motd=Mine\n");
    expect(readFileSync(properties, "utf8")).toBe("motd=Written by the game\n");
    // Escape doesn't throw the unsaved text away.
    await editor.press("Escape");
    await expect(editor).toHaveValue("motd=Mine\n");
    await dialog.getByRole("button", { name: "Cancel" }).click();
    await expect(dialog.locator("pre")).toContainText("motd=Written by the game");
  } finally {
    writeFileSync(properties, original);
  }
});

test("a viewer who can read files is not offered editing", async ({ page, session }) => {
  await createUser(page, "fileviewer", "file viewer password", "viewer");
  const survival = page.locator("[data-grants] .grants-server", { hasText: "Survival" });
  await survival.getByRole("button", { name: "Status only" }).click();
  await survival.getByLabel("Read and download files").check();
  await page.locator("[data-grants]").getByRole("button", { name: "Save server access" }).click();
  await expect(page.locator("[data-grants] .alert--success")).toContainText("Server access saved for fileviewer.");

  const viewer = await session({ storageState: { cookies: [], origins: [] } });
  await signIn(viewer, "fileviewer", "file viewer password");
  await openFiles(viewer);
  await viewer.getByRole("button", { name: "server.properties", exact: true }).click();
  const dialog = viewer.getByRole("dialog");
  await expect(dialog.locator("pre")).toContainText("motd=");
  await expect(dialog.getByRole("link", { name: "Download" })).toBeVisible();
  await expect(dialog.getByRole("button", { name: "Edit" })).toHaveCount(0);
  // Asking for the editor directly still shows only the preview.
  const fragment = await viewer.request.get(`${new URL(viewer.url()).pathname.replace("/files/", "/fragments/files/")}/preview?root=root-0&path=server.properties&edit=1`);
  const body = await fragment.text();
  expect(body).toContain("Contents of server.properties");
  expect(body).not.toContain("data-file-editor");
});
