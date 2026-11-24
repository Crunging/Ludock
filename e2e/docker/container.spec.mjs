// The built image against real Docker: helper containers from Ludock's own
// image, backups, and the hardened runtime. lifecycle.spec.mjs covers the
// rest; CI runs the two files in separate jobs.
import { chmodSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { test, expect, asAdmin, openServer, expectOperationSucceeded } from "../journeys/fixtures.mjs";
import { docker, ludock, stack, gameContainer } from "../docker-setup.mjs";

asAdmin();

test("the image runs hardened and reports healthy", async ({ page }) => {
  const id = docker(...ludock(), "ps", "--quiet", "ludock");
  await expect.poll(() => docker("inspect", "--format", "{{.State.Health.Status}}", id), { timeout: 60_000 }).toBe("healthy");
  const status = docker("exec", id, "cat", "/proc/1/status");
  expect(status).toMatch(/^CapEff:\s+0+2$/m);
  expect(status).toMatch(/^NoNewPrivs:\s+1$/m);
  expect(docker("inspect", "--format", "{{.HostConfig.ReadonlyRootfs}}", id)).toBe("true");
  expect(docker("exec", id, "ludock", "version")).not.toBe("");

  await page.goto("/settings/system");
  await expect(page.locator(".metadata-list")).toContainText("DockerConnected");
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
  // Helper containers are gone once the work is done, except the read-only
  // listing helper, kept for the next listing until it has been idle for 30
  // seconds (the files package's tests cover removal). Only helpers with
  // this game's data count: other games on the daemon get their own.
  const helpers = () => docker("ps", "--all", "--quiet", "--filter", "label=ludock.internal", "--filter", `volume=${join(stack(), "data")}`)
    .split("\n").filter(Boolean).map((id) => docker("inspect", "--format", '{{index .Config.Labels "ludock.internal"}} {{range .Mounts}}{{.RW}}{{end}}', id));
  await expect.poll(helpers, { timeout: 20_000 }).toEqual(["file-helper false"]);
});

test("a backup restores the server's data and running state", async ({ page }) => {
  const data = join(stack(), "data");
  const properties = join(data, "server.properties");
  // Only here does a restore run as root: the setgid data folder gets its
  // bit back after its owner.
  chmodSync(data, 0o2755);
  await openServer(page, "Test Game");
  await page.locator(".section-tabs").getByRole("link", { name: "Backups" }).click();
  await expect(page.locator("#create-backup")).toBeEnabled();
  await page.getByRole("button", { name: "Create backup" }).click();
  await page.getByRole("dialog", { name: "Back up Test Game?" }).getByRole("button", { name: "Create backup" }).click();
  await expectOperationSucceeded(page);

  writeFileSync(properties, "motd=changed after the backup\n");
  chmodSync(data, 0o755);
  await page.locator("#backup-list tbody tr").first().getByRole("link", { name: "Restore…" }).click();
  await page.locator("form.danger-panel").getByLabel(/Type Test Game to confirm/).fill("Test Game");
  await page.getByRole("button", { name: "Restore game data" }).click();
  await expectOperationSucceeded(page);
  expect(readFileSync(properties, "utf8")).toBe("motd=Hello from the container test\n");
  expect(statSync(data).mode & 0o7777).toBe(0o2755);
  expect(docker("inspect", "--format", "{{.State.Running}}", gameContainer())).toBe("true");
  // The restarted game passes its healthcheck again, and its page, which
  // refreshes every 10 seconds, shows it running.
  await expect.poll(() => docker("inspect", "--format", "{{.State.Health.Status}}", gameContainer()), { timeout: 30_000 }).toBe("healthy");
  await openServer(page, "Test Game");
  await expect(page.locator("#detail-live .server-state")).toContainText("Running", { timeout: 15_000 });
});
