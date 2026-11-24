// Takes the README screenshots: builds Ludock and the fake Docker daemon,
// starts them on fresh demo data (fakedocker -demo), and saves the pages to
// docs/screenshots. Run with `cd e2e && node screenshots.mjs`; set
// LUDOCK_E2E_CHROMIUM to use an installed Chromium.
import { execFileSync, spawn } from "node:child_process";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { chromium } from "@playwright/test";

const root = resolve(import.meta.dirname, "..");
const out = join(root, "docs", "screenshots");
// A short path: the fake daemon's Unix socket must fit in 108 bytes.
const dir = mkdtempSync(join(tmpdir(), "ludock-shots-"));
const port = 43299;
// The browser opens Ludock by a name, as people do, so invite links show it.
const base = `http://ludock.example.com:${port}`;
const setupCode = "screenshots-setup-code-0123456789abcdef";
const admin = { username: "Crunging", password: "correct horse battery staple" };

async function waitFor(check, what) {
  for (const deadline = Date.now() + 30_000; Date.now() < deadline; await new Promise((done) => setTimeout(done, 100))) {
    try {
      if (await check()) return;
    } catch {
      // Not ready yet.
    }
  }
  throw new Error(`timed out waiting for ${what}`);
}

const children = [];
try {
  execFileSync("go", ["build", "-o", join(dir, "bin") + "/", "./cmd/ludock", "./cmd/fakedocker"],
    { cwd: root, env: { ...process.env, CGO_ENABLED: "0" }, stdio: "inherit" });
  const backups = join(dir, "backups");
  children.push(spawn(join(dir, "bin", "fakedocker"), ["-demo", "-dir", join(dir, "docker"), "-backups", backups], { stdio: "ignore" }));
  const socket = join(dir, "docker", "docker.sock");
  await waitFor(() => existsSync(socket), "the fake Docker socket");
  children.push(spawn(join(dir, "bin", "ludock"), ["serve"], { stdio: "ignore", env: { ...process.env,
    LUDOCK_LISTEN: `127.0.0.1:${port}`, LUDOCK_DATA_DIR: join(dir, "data"), LUDOCK_DOCKER_SOCKET: socket,
    LUDOCK_SETUP_CODE: setupCode, LUDOCK_SELF_CONTAINER: "ludock-dev", LUDOCK_BACKUP_ROOTS: backups,
    LUDOCK_HELPER_IMAGE: "ghcr.io/crunging/ludock@sha256:" + "ab".repeat(32) } }));
  await waitFor(async () => (await fetch(`http://127.0.0.1:${port}/api/v1/health`)).ok, "Ludock's health check");

  const browser = await chromium.launch({ executablePath: process.env.LUDOCK_E2E_CHROMIUM || undefined,
    args: ["--host-resolver-rules=MAP ludock.example.com 127.0.0.1"] });
  const page = await browser.newPage({ baseURL: base, viewport: { width: 1440, height: 900 }, colorScheme: "dark" });
  const shot = async (name) => {
    // Let live regions and relative times settle, then hide the caret.
    await page.waitForTimeout(1500);
    await page.evaluate(() => document.activeElement?.blur());
    await page.screenshot({ path: join(out, name) });
  };

  await page.goto("/login");
  await page.getByLabel("Setup code").fill(setupCode);
  await page.getByLabel("Username").fill(admin.username);
  await page.getByLabel("Password", { exact: true }).fill(admin.password);
  await page.getByLabel("Confirm password").fill(admin.password);
  await page.getByRole("button", { name: "Create administrator" }).click();
  await page.waitForURL(base + "/");

  await page.goto("/settings");
  await page.getByLabel("Public address").fill("play.example.com");
  await Promise.all([page.waitForResponse("**/settings/connection"), page.getByRole("button", { name: "Save address" }).click()]);

  // The first page after setup: what's left to do, then the servers.
  await page.goto("/");
  await page.locator(".checklist li").first().waitFor();
  await shot("setup.png");

  const minecraft = "Friends’ survival world";
  const openServer = async (tab, name = minecraft) => {
    await page.goto("/");
    await page.getByRole("link", { name, exact: true }).click();
    if (tab) await page.locator(".section-tabs").getByRole("link", { name: tab }).click();
  };
  const backUp = async () => {
    await page.locator("#create-backup").click();
    // Demo players are online; the screenshots needn't wait out their warning.
    const warn = page.getByRole("dialog").getByLabel("Warn players and wait 1 minute");
    if (await warn.count()) await warn.uncheck();
    await page.getByRole("dialog").getByRole("button", { name: "Create backup" }).click();
    await page.locator("#selected-operation").getByText("Succeeded").waitFor({ timeout: 45_000 });
    await page.getByRole("link", { name: "Dismiss" }).click();
  };

  for (const name of ["Factorio weekend", "Valheim co-op"]) {
    await openServer("Backups", name);
    await backUp();
  }
  // Three backups, the oldest kept for good.
  await openServer("Backups");
  for (let i = 0; i < 3; i++) {
    await backUp();
    await page.locator("#backup-list tbody tr").nth(i).waitFor();
  }
  const oldest = page.locator("#backup-list tbody tr").last();
  await oldest.getByRole("button", { name: /^More actions for the backup/ }).click();
  await oldest.getByRole("link", { name: "Keep…" }).click();
  await page.locator("#keep-backup").getByLabel("Note").fill("Before 1.21.5");
  await page.locator("#keep-backup").getByRole("button", { name: "Keep backup" }).click();
  await page.locator("#backup-list").getByText("Before 1.21.5").waitFor();
  await page.goto(page.url().split("&")[0]);
  await shot("backups.png");

  await page.goto("/");
  await page.getByRole("link", { name: minecraft, exact: true }).click();
  await page.getByRole("link", { name: "Files" }).first().click();
  await page.getByText("server.properties").waitFor();
  await shot("files.png");

  // Sharing with a friend who has no account yet.
  await openServer("Sharing");
  const share = page.locator("form.share-form");
  await share.getByLabel("Their username").fill("Theo");
  await share.getByRole("button", { name: "Share" }).click();
  await page.getByRole("region", { name: "Invite link for Theo" }).waitFor();
  await shot("sharing.png");

  // The everyday server list, once the setup list is done.
  await page.goto("/");
  for (let item = page.locator(".checklist li"); await item.count(); ) {
    await item.first().getByRole("button", { name: "Dismiss" }).click();
    await page.waitForTimeout(300);
  }
  await shot("servers.png");
  await browser.close();
} finally {
  for (const child of children.reverse()) child.kill("SIGTERM");
  await new Promise((done) => setTimeout(done, 500));
  rmSync(dir, { recursive: true, force: true });
}
