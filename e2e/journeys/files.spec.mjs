import { randomBytes } from "node:crypto";
import { chmodSync, existsSync, mkdirSync, readdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { crc32, gzipSync } from "node:zlib";
import { test, expect, asAdmin, serverRow, signIn, createUser, fake } from "./fixtures.mjs";

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

test("a save or upload whose result is lost is reported as unconfirmed, not failed", async ({ page }) => {
  const properties = join(survivalData(), "server.properties");
  const original = readFileSync(properties);
  try {
    await openFiles(page);
    await page.getByRole("button", { name: "server.properties", exact: true }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: "Edit" }).click();
    const editor = dialog.getByRole("textbox", { name: "Edit server.properties" });
    await editor.fill("motd=Maybe saved\n");
    // The daemon loses the result after the helper wrote the file.
    await fake("/fake/lose-upload");
    await dialog.getByRole("button", { name: "Save" }).click();
    await expect(dialog.locator(".alert--error")).toHaveText(
      "The save of “server.properties” could not be confirmed. It may have completed. Copy your text, then reopen the file to check it before saving again.");
    await expect(editor).toHaveValue("motd=Maybe saved\n");
    expect(readFileSync(properties, "utf8")).toBe("motd=Maybe saved\n");
    await dialog.getByRole("button", { name: "Cancel" }).click();
    await expect(dialog.locator("pre")).toContainText("motd=Maybe saved");
    await dialog.getByRole("button", { name: "Close" }).click();

    await fake("/fake/lose-upload");
    await page.locator("[data-upload-input]").setInputFiles([
      { name: "lost.txt", mimeType: "text/plain", buffer: Buffer.from("lost result\n") },
      { name: "never.txt", mimeType: "text/plain", buffer: Buffer.from("not sent\n") },
    ]);
    await expect(page.locator("#file-feedback")).toHaveText(
      "Uploaded 0 of 2 files. The result for “lost.txt” could not be confirmed. The upload may have completed. Review the folder before uploading again.");
    await expect(page.locator(".file-row__name")).toContainText(["lost.txt"]);
    await expect(page.locator(".file-row__name", { hasText: "never.txt" })).toHaveCount(0);

    // Both are in the audit log, since either may have happened.
    for (const action of ["server.file.edited", "server.file.uploaded"]) {
      await page.goto("/audit?action=" + action);
      await expect(page.locator(".audit-entry").first()).toContainText(action);
    }
  } finally {
    writeFileSync(properties, original);
    rmSync(join(survivalData(), "lost.txt"), { force: true });
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
  const root = await viewer.locator("[data-files-root]").getAttribute("data-files-root");
  const fragment = await viewer.request.get(`${new URL(viewer.url()).pathname.replace("/files/", "/fragments/files/")}/preview?root=${root}&path=server.properties&edit=1`);
  const body = await fragment.text();
  expect(body).toContain("Contents of server.properties");
  expect(body).not.toContain("data-file-editor");

  // Selecting offers downloading only, and archives offer no unpacking.
  writeFileSync(join(survivalData(), "viewer-pack.zip"), zip([{ name: "a.txt", body: "a" }]));
  try {
    await viewer.reload();
    await viewer.getByRole("button", { name: "Select" }).click();
    const bar = viewer.locator("[data-selection-bar]");
    await expect(bar.getByRole("button", { name: "Download" })).toBeVisible();
    await expect(bar.getByRole("button", { name: /Move|Delete/ })).toHaveCount(0);
    await viewer.getByRole("button", { name: "Actions for viewer-pack.zip" }).click();
    await expect(viewer.locator("#file-list").getByRole("link", { name: "Download" }).first()).toBeVisible();
    await expect(viewer.getByRole("button", { name: "Unpack" })).toHaveCount(0);
  } finally {
    rmSync(join(survivalData(), "viewer-pack.zip"), { force: true });
  }
});

// Archives are built here, so the journeys need no archiving tools.
function tar(entries) {
  const blocks = [];
  for (const { name, body = "", type = "0", link = "" } of entries) {
    const data = Buffer.from(body);
    const header = Buffer.alloc(512);
    header.write(name, 0);
    header.write("0000644\0", 100);
    header.write("0000000\0", 108);
    header.write("0000000\0", 116);
    header.write(data.length.toString(8).padStart(11, "0") + "\0", 124);
    header.write("00000000000\0", 136);
    header.write("        ", 148);
    header.write(type, 156);
    header.write(link, 157);
    header.write("ustar\u000000", 257);
    let sum = 0;
    for (const byte of header) sum += byte;
    header.write(sum.toString(8).padStart(6, "0") + "\0 ", 148);
    blocks.push(header, data, Buffer.alloc((512 - (data.length % 512)) % 512));
  }
  blocks.push(Buffer.alloc(1024));
  return Buffer.concat(blocks);
}

function zip(entries) {
  const files = [], directory = [];
  let offset = 0;
  for (const { name, body = "" } of entries) {
    const path = Buffer.from(name), data = Buffer.from(body), sum = crc32(data);
    const local = Buffer.alloc(30);
    local.writeUInt32LE(0x04034b50, 0);
    local.writeUInt16LE(20, 4);
    local.writeUInt32LE(sum, 14);
    local.writeUInt32LE(data.length, 18);
    local.writeUInt32LE(data.length, 22);
    local.writeUInt16LE(path.length, 26);
    const central = Buffer.alloc(46);
    central.writeUInt32LE(0x02014b50, 0);
    central.writeUInt16LE(20, 4);
    central.writeUInt16LE(20, 6);
    central.writeUInt32LE(sum, 16);
    central.writeUInt32LE(data.length, 20);
    central.writeUInt32LE(data.length, 24);
    central.writeUInt16LE(path.length, 28);
    central.writeUInt32LE(offset, 42);
    files.push(local, path, data);
    directory.push(central, path);
    offset += 30 + path.length + data.length;
  }
  const listing = Buffer.concat(directory);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0);
  end.writeUInt16LE(entries.length, 8);
  end.writeUInt16LE(entries.length, 10);
  end.writeUInt32LE(listing.length, 12);
  end.writeUInt32LE(offset, 16);
  return Buffer.concat([...files, listing, end]);
}

async function rowAction(page, name, action) {
  await page.getByRole("button", { name: `Actions for ${name}`, exact: true }).click();
  await page.getByRole("button", { name: action, exact: true }).click();
}

test("uploading a world archive and unpacking it into a new folder", async ({ page }) => {
  const data = survivalData();
  try {
    await openFiles(page);
    const names = page.locator(".file-row__name");
    await page.locator("[data-upload-input]").setInputFiles([
      { name: "Skyblock.zip", mimeType: "application/zip", buffer: zip([
        { name: "Skyblock/level.dat", body: "level" }, { name: "Skyblock/region/r.0.0.mca", body: "region" }]) },
      { name: "plugins.tar.gz", mimeType: "application/gzip", buffer: gzipSync(tar([
        { name: "essentials.jar", body: "jar" }, { name: "config/essentials.yml", body: "motd: hi" },
        { name: "escape", type: "2", link: "/etc" }])) },
    ]);
    await expect(page.locator("#file-feedback")).toHaveText("2 files uploaded.");

    // An archive with one folder inside becomes that folder.
    await rowAction(page, "Skyblock.zip", "Unpack");
    const dialog = page.getByRole("dialog");
    await expect(dialog).toContainText("Nothing that exists is replaced.");
    await dialog.getByRole("button", { name: "Unpack" }).click();
    await expect(page.locator("#file-feedback")).toHaveText("Unpacked “Skyblock.zip” into “Skyblock”.");
    await page.getByRole("link", { name: "Skyblock", exact: true }).click();
    await expect(names).toHaveText(["region", "level.dat"]);
    expect(readFileSync(join(data, "Skyblock", "region", "r.0.0.mca"), "utf8")).toBe("region");
    await page.locator(".file-breadcrumbs").getByRole("link", { name: "minecraft" }).click();

    // Unpacking again never replaces what's there.
    await rowAction(page, "Skyblock.zip", "Unpack");
    await dialog.getByRole("button", { name: "Unpack" }).click();
    await expect(page.locator("#file-feedback")).toHaveText("Unpacked “Skyblock.zip” into “Skyblock (2)”.");

    // Several top-level entries go into a folder named after the archive;
    // links inside it are left out.
    await rowAction(page, "plugins.tar.gz", "Unpack");
    await dialog.getByRole("button", { name: "Unpack" }).click();
    await expect(page.locator("#file-feedback")).toHaveText(
      "Unpacked “plugins.tar.gz” into “plugins”. Left out links and special files: 1 item.");
    expect(readdirSync(join(data, "plugins")).sort()).toEqual(["config", "essentials.jar"]);
    await expect(names).toContainText(["plugins", "Skyblock", "Skyblock (2)"]);
    await page.goto("/audit?action=server.file.unpacked");
    await expect(page.locator(".audit-entry").first()).toContainText("server.file.unpacked");
  } finally {
    for (const name of ["Skyblock", "Skyblock (2)", "plugins", "Skyblock.zip", "plugins.tar.gz"]) rmSync(join(data, name), { recursive: true, force: true });
  }
});

test("an archive that escapes its folder or unpacks too large is refused and leaves nothing behind", async ({ page }) => {
  const data = survivalData();
  const before = readdirSync(data).sort();
  try {
    await openFiles(page);
    await page.locator("[data-upload-input]").setInputFiles([
      { name: "escape.tar", mimeType: "application/x-tar", buffer: tar([{ name: "fine.txt", body: "fine" }, { name: "../../escape.txt", body: "x" }]) },
      // Four MiB of zeros compress to a few KiB: far past 100 times.
      { name: "bomb.tar.gz", mimeType: "application/gzip", buffer: gzipSync(tar([{ name: "zeros.bin", body: "\0".repeat(4 << 20) }])) },
      { name: "notes.zip", mimeType: "application/zip", buffer: Buffer.from("not really an archive") },
    ]);
    await expect(page.locator("#file-feedback")).toHaveText("3 files uploaded.");
    const dialog = page.getByRole("dialog");
    for (const [name, message] of [
      ["escape.tar", "The archive wasn’t unpacked: it has entries with unsafe names, such as paths that lead outside its folder."],
      ["bomb.tar.gz", "The archive wasn’t unpacked: it unpacks to more than 20 GiB or 100 times its own size, or has too many entries or nested folders."],
      ["notes.zip", "This file isn’t a zip, tar, or tar.gz archive, or it is damaged."],
    ]) {
      await rowAction(page, name, "Unpack");
      await dialog.getByRole("button", { name: "Unpack" }).click();
      await expect(dialog.locator(".alert--error")).toHaveText(message);
      await dialog.getByRole("button", { name: "Cancel" }).click();
    }
    expect(readdirSync(data).sort()).toEqual([...before, "bomb.tar.gz", "escape.tar", "notes.zip"].sort());
    expect(existsSync(join(data, "..", "escape.txt")) || existsSync(join(data, "..", "..", "escape.txt"))).toBe(false);
  } finally {
    for (const name of ["escape.tar", "bomb.tar.gz", "notes.zip"]) rmSync(join(data, name), { force: true });
  }
});

test("selecting several entries and deleting them", async ({ page }) => {
  const folder = join(survivalData(), "logs");
  mkdirSync(folder);
  try {
    for (const name of ["2026-01.log", "2026-02.log", "latest.log"]) writeFileSync(join(folder, name), name);
    await openFiles(page);
    // Selecting stays out of the way until someone asks for it.
    await expect(page.locator(".file-row__select").first()).toBeHidden();
    await expect(page.locator("[data-selection-bar]")).toBeHidden();
    await page.getByRole("link", { name: "logs", exact: true }).click();
    await page.getByRole("button", { name: "Select" }).click();
    const bar = page.locator("[data-selection-bar]");
    await expect(bar).toContainText("0 selected");
    await expect(bar.getByRole("button", { name: "Delete…" })).toBeDisabled();
    await page.getByRole("checkbox", { name: "Select 2026-01.log" }).check();
    await page.getByRole("checkbox", { name: "Select 2026-02.log" }).check();
    await expect(bar).toContainText("2 selected");
    await bar.getByRole("button", { name: "Delete…" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByRole("heading")).toHaveText("Delete 2 items?");
    await expect(dialog.locator("li")).toHaveText(["2026-01.log", "2026-02.log"]);
    await dialog.getByRole("button", { name: "Delete 2 items" }).click();
    await expect(page.locator("#file-feedback")).toHaveText("Deleted 2 items.");
    await expect(page.locator(".file-row__name")).toHaveText(["latest.log"]);
    await expect(bar).toBeHidden();
    expect(readdirSync(folder)).toEqual(["latest.log"]);

    // Select all picks every entry shown; Cancel clears the selection.
    await page.getByRole("button", { name: "Select" }).click();
    await page.getByRole("checkbox", { name: "Select all shown" }).check();
    await expect(bar).toContainText("1 selected");
    await bar.getByRole("button", { name: "Cancel" }).click();
    await expect(bar).toBeHidden();
    await expect(page.locator("[data-file-select]")).not.toBeChecked();
  } finally {
    rmSync(folder, { recursive: true, force: true });
  }
});

test("moving and downloading a selection", async ({ page }) => {
  const folder = join(survivalData(), "move-test");
  mkdirSync(join(folder, "dest", "inner"), { recursive: true });
  try {
    writeFileSync(join(folder, "a.txt"), "a");
    writeFileSync(join(folder, "b.txt"), "b");
    writeFileSync(join(folder, "dest", "b.txt"), "older b");
    await openFiles(page);
    await page.getByRole("link", { name: "move-test" }).click();
    await page.getByRole("button", { name: "Select" }).click();
    await page.getByRole("checkbox", { name: "Select a.txt" }).check();
    await page.getByRole("checkbox", { name: "Select b.txt" }).check();
    const bar = page.locator("[data-selection-bar]");

    const [download] = await Promise.all([page.waitForEvent("download"), bar.getByRole("button", { name: "Download" }).click()]);
    expect(download.suggestedFilename()).toBe("move-test selection.tar");
    const archive = readFileSync(await download.path());
    expect(archive.subarray(0, 5).toString()).toBe("a.txt");
    expect(archive.includes(Buffer.from("b.txt"))).toBe(true);

    await bar.getByRole("button", { name: "Move…" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByRole("heading")).toHaveText("Move 2 items");
    const move = dialog.getByRole("button", { name: "Move here" });
    await expect(move).toBeDisabled();
    await dialog.getByRole("button", { name: "dest", exact: true }).click();
    await expect(dialog.getByRole("button", { name: "inner" })).toBeVisible();
    // A name already in the folder is never replaced.
    await move.click();
    await expect(dialog.locator(".alert--error")).toHaveText("A file or folder with that name already exists");
    expect(readFileSync(join(folder, "dest", "b.txt"), "utf8")).toBe("older b");
    await dialog.getByRole("button", { name: "inner" }).click();
    await dialog.getByRole("button", { name: "Move here" }).click();
    await expect(page.locator("#file-feedback")).toHaveText("Moved 2 items to “move-test/dest/inner”.");
    await expect(page.locator(".file-row__name")).toHaveText(["dest"]);
    expect(readdirSync(join(folder, "dest", "inner")).sort()).toEqual(["a.txt", "b.txt"]);
  } finally {
    rmSync(folder, { recursive: true, force: true });
  }
});
