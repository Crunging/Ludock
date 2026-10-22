import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
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
