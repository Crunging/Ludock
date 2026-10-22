// The built image against real Docker: hardened runtime, helper containers
// from Ludock's own image, backups, consoles, Compose updates, and recovery.
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { test, expect, asAdmin, openServer, expectOperationSucceeded } from "../journeys/fixtures.mjs";
import { docker } from "../docker-setup.mjs";

asAdmin();

const ludock = () => JSON.parse(process.env.LUDOCK_DOCKER_LUDOCK);
const game = () => JSON.parse(process.env.LUDOCK_DOCKER_GAME);
const stack = () => process.env.LUDOCK_DOCKER_STACK;
const gameContainer = () => docker(...game(), "ps", "--quiet", "game");

test("the image runs hardened and reports healthy", async ({ page }) => {
  const id = docker(...ludock(), "ps", "--quiet", "ludock");
  await expect.poll(() => docker("inspect", "--format", "{{.State.Health.Status}}", id), { timeout: 60_000 }).toBe("healthy");
  const status = docker("exec", id, "cat", "/proc/1/status");
  expect(status).toMatch(/^CapEff:\s+0+2$/m);
  expect(status).toMatch(/^NoNewPrivs:\s+1$/m);
  expect(docker("inspect", "--format", "{{.HostConfig.ReadonlyRootfs}}", id)).toBe("true");
  expect(docker("exec", id, "ludock", "version")).not.toBe("");

  await page.goto("/diagnostics");
  await expect(page.locator(".metadata-list")).toContainText("Docker EngineConnected");
  await expect(page.locator(".metadata-list")).toContainText("Compose updatesAvailable");
  // Compose roots are host paths: settings says they are set, not where.
  await page.goto("/settings");
  await expect(page.locator(".admin-compose-guidance")).toContainText("Compose source roots are set");
  await expect(page.locator("main")).not.toContainText(dirname(stack()));
  await page.goto("/");
  await expect(page.locator(".server-row", { hasText: "Test Game" })).toContainText("Running");
});

test("file helpers from Ludock's image read and change server data", async ({ page }) => {
  await openServer(page, "Test Game");
  await page.locator(".detail-actions").getByRole("link", { name: "Files" }).click();
  await expect(page.locator(".file-row__name")).toContainText(["world", "server.properties"]);
  await page.getByRole("button", { name: "server.properties", exact: true }).click();
  await expect(page.getByRole("dialog").locator("pre")).toContainText("Hello from the container test");
  await page.getByRole("dialog").getByRole("button", { name: "Close" }).click();

  await page.locator("[data-upload-input]").setInputFiles([{ name: "uploaded.txt", mimeType: "text/plain", buffer: Buffer.from("from the browser\n") }]);
  await expect(page.locator("#file-feedback")).toHaveText("1 file uploaded.");
  expect(readFileSync(join(stack(), "data", "uploaded.txt"), "utf8")).toBe("from the browser\n");
  expect(docker("exec", gameContainer(), "cat", "/data/uploaded.txt")).toBe("from the browser");

  await page.locator(".file-row", { hasText: "uploaded.txt" }).getByRole("button", { name: "Actions for uploaded.txt" }).click();
  await page.getByRole("button", { name: "Delete" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Delete file" }).click();
  await expect(page.locator("#file-feedback")).toHaveText("Deleted “uploaded.txt”.");
  // Helper containers are gone once the work is done.
  await expect.poll(() => docker("ps", "--all", "--quiet", "--filter", "label=ludock.internal")).toBe("");
});

test("a backup restores the server's data and running state", async ({ page }) => {
  const properties = join(stack(), "data", "server.properties");
  await openServer(page, "Test Game");
  await page.getByRole("tab", { name: "Backups" }).click();
  await expect(page.locator("#backup-readiness")).toContainText("Ready to back up.");
  await page.getByRole("button", { name: "Create backup" }).click();
  await expectOperationSucceeded(page);

  writeFileSync(properties, "motd=changed after the backup\n");
  await page.getByRole("tab", { name: "Backups" }).click();
  await page.locator("#backup-list tbody tr").first().getByRole("link", { name: "Restore…" }).click();
  await page.locator("form.danger-panel").getByLabel(/Type Test Game to confirm/).fill("Test Game");
  await page.getByRole("button", { name: "Restore game data" }).click();
  await expectOperationSucceeded(page);
  expect(readFileSync(properties, "utf8")).toBe("motd=Hello from the container test\n");
  expect(docker("inspect", "--format", "{{.State.Running}}", gameContainer())).toBe("true");
});

test("the shell runs commands and logs follow the container", async ({ page }) => {
  await openServer(page, "Test Game");
  await page.locator(".detail-actions").getByRole("link", { name: "Console" }).click();
  await page.getByRole("link", { name: "Docker Logs" }).click();
  await expect(page.locator(".xterm-rows")).toContainText("tick ");
  await page.getByRole("link", { name: "Container Shell" }).click();
  await expect(page.locator("[data-console-status]")).toHaveText("Connected");
  await page.getByLabel("Shell command").fill("cat /data/server.properties");
  await page.getByRole("button", { name: "Send" }).click();
  await expect(page.locator(".xterm-rows")).toContainText("motd=Hello from the container test");
});

test("a Compose update recreates the service from its source files", async ({ page }) => {
  const before = gameContainer();
  await openServer(page, "Test Game");
  await page.getByRole("tab", { name: "Update" }).click();
  const form = page.locator("#update-panel form");
  await expect(form).toContainText("game");
  await form.getByLabel(/Recreate anyway/).check();
  await form.getByLabel("Create a stopped-server backup before recreation").uncheck();
  await form.getByLabel(/Type Test Game to skip the backup/).fill("Test Game");
  await form.getByLabel(/I understand/).check();
  await form.getByRole("button").last().click();
  await expectOperationSucceeded(page);
  await expect.poll(gameContainer).not.toBe(before);
  await expect.poll(() => docker("inspect", "--format", "{{.State.Running}}", gameContainer())).toBe("true");
});

// The documented procedure: stop Ludock, run the recovery command, start it.
// Recovery signs every session out, so this journey runs last.
test("an administrator password can be recovered from the console", async ({ page }) => {
  docker(...ludock(), "stop", "ludock");
  docker(...ludock(), "run", "--rm", "--no-deps", "-e", "LUDOCK_RECOVERY_PASSWORD=recovered long password", "ludock", "recover", "admin");
  docker(...ludock(), "start", "ludock");
  await expect.poll(async () => (await fetch("http://127.0.0.1:3000/api/v1/health").catch(() => ({ ok: false }))).ok, { timeout: 60_000 }).toBe(true);

  await page.context().clearCookies();
  await page.goto("/login");
  await page.getByLabel("Username").fill("admin");
  await page.getByLabel("Password", { exact: true }).fill("recovered long password");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Servers", exact: true })).toBeVisible();
});
