// The built image against real Docker: hardened runtime, helper containers
// from Ludock's own image, backups, consoles, image updates, and recovery.
import { spawnSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { test, expect, asAdmin, openServer, expectOperationSucceeded } from "../journeys/fixtures.mjs";
import { docker, gameImage } from "../docker-setup.mjs";

asAdmin();

const ludock = () => JSON.parse(process.env.LUDOCK_DOCKER_LUDOCK);
const game = () => JSON.parse(process.env.LUDOCK_DOCKER_GAME);
const stack = () => process.env.LUDOCK_DOCKER_STACK;
const gameContainer = () => docker(...game(), "ps", "--quiet", "game");
const healthy = async () => (await fetch("http://127.0.0.1:3000/api/v1/health").catch(() => ({ ok: false }))).ok;

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
  // Helper containers are gone once the work is done; a read-only listing
  // helper stays idle for 30 seconds first.
  await expect.poll(() => docker("ps", "--all", "--quiet", "--filter", "label=ludock.internal"), { timeout: 45_000 }).toBe("");
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

test("an update recreates the server with its settings and unnamed volume", async ({ page }) => {
  const before = gameContainer();
  docker("exec", before, "sh", "-c", "echo kept > /state/marker");
  const inspect = (id) => JSON.parse(docker("inspect", id))[0];
  const old = inspect(before);
  await openServer(page, "Test Game");
  await page.getByRole("tab", { name: "Update" }).click();
  const form = page.locator("#update-panel form");
  // The previous journey's shell command may hold the server for a moment.
  const checkAgain = page.locator("#update-panel").getByRole("button", { name: "Check again" });
  await expect(async () => {
    if (await checkAgain.count()) await checkAgain.click();
    await expect(form).toContainText(gameImage, { timeout: 2_000 });
  }).toPass();
  await form.getByLabel(/Recreate anyway/).check();
  await form.getByLabel("Create a stopped-server backup before recreation").uncheck();
  await form.getByLabel(/Type Test Game to skip the backup/).fill("Test Game");
  await form.getByLabel(/I understand/).check();
  await form.getByRole("button").last().click();
  await expectOperationSucceeded(page);
  await expect.poll(gameContainer).not.toBe(before);
  const replacement = inspect(gameContainer());
  expect(replacement.State.Running).toBe(true);
  expect(replacement.Name).toBe(old.Name);
  expect(replacement.Config.Cmd).toEqual(old.Config.Cmd);
  // Compose's image label is checked by asking Compose below.
  const labels = ({ Config: { Labels } }) => ({ ...Labels, "com.docker.compose.image": undefined, "ludock.update": undefined });
  expect(labels(replacement)).toEqual(labels(old));
  expect(replacement.Config.Labels["ludock.update"]).toMatch(/^[0-9a-f-]{36}$/);
  expect(Object.keys(replacement.NetworkSettings.Networks)).toEqual(Object.keys(old.NetworkSettings.Networks));
  const volume = (container) => container.Mounts.find((mount) => mount.Destination === "/state").Name;
  expect(volume(replacement)).toBe(volume(old));
  expect(docker("exec", replacement.Id, "cat", "/state/marker")).toBe("kept");
  expect(docker("ps", "--all", "--quiet", "--filter", `id=${before}`)).toBe("");
  // Compose sees the replacement as its own, up-to-date container.
  const plan = spawnSync("docker", [...game(), "up", "--detach", "--dry-run", "game"], { encoding: "utf8" });
  expect(plan.status).toBe(0);
  expect(plan.stdout + plan.stderr).not.toMatch(/Recreat/);
});

// A file command interrupted by a restart may still be writing through its
// helper, so Ludock removes every file helper before work resumes.
test("a file helper left by an interrupted run is removed when Ludock starts", async ({ page }) => {
  docker(...ludock(), "stop", "ludock");
  const leftover = docker("run", "--detach", "--label", "ludock.enable=false", "--label", "ludock.internal=file-helper",
    "--volume", `${join(stack(), "data")}:/data`, gameImage, "sh", "-c", "while true; do date > /data/leftover; sleep 1; done");
  const unrelated = docker("run", "--detach", "--label", "ludock.internal=other", gameImage, "sleep", "600");
  try {
    docker(...ludock(), "start", "ludock");
    await expect.poll(healthy, { timeout: 60_000 }).toBe(true);
    await expect.poll(() => docker("ps", "--all", "--quiet", "--filter", `id=${leftover}`), { timeout: 30_000 }).toBe("");
    expect(docker("ps", "--quiet", "--filter", `id=${unrelated}`)).not.toBe("");
    expect(docker("inspect", "--format", "{{.State.Running}}", gameContainer())).toBe("true");
    await openServer(page, "Test Game");
  } finally {
    docker("rm", "--force", unrelated);
  }
});

// The documented procedure: stop Ludock, run the recovery command, start it.
// Recovery signs every session out, so this journey runs last.
test("an administrator password can be recovered from the console", async ({ page }) => {
  docker(...ludock(), "stop", "ludock");
  docker(...ludock(), "run", "--rm", "--no-deps", "-e", "LUDOCK_RECOVERY_PASSWORD=recovered long password", "ludock", "recover", "admin");
  docker(...ludock(), "start", "ludock");
  await expect.poll(healthy, { timeout: 60_000 }).toBe(true);

  await page.context().clearCookies();
  await page.goto("/login");
  await page.getByLabel("Username").fill("admin");
  await page.getByLabel("Password", { exact: true }).fill("recovered long password");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Servers", exact: true })).toBeVisible();
});
