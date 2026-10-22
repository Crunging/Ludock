import { randomBytes } from "node:crypto";
import { mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { test, expect, asAdmin, serverRow } from "./fixtures.mjs";

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
  await expect(dialog.locator(".alert--error")).toBeVisible();
  await dialog.getByLabel("Folder name").fill("mods");
  await dialog.getByRole("button", { name: "Create folder" }).click();
  await expect(page.locator("#file-feedback")).toHaveText("Created “mods”.");

  await page.locator(".file-row", { hasText: "mods" }).getByRole("button", { name: "Actions for mods" }).click();
  await page.getByRole("button", { name: "Rename" }).click();
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
