import { test, expect, asAdmin, serverRow } from "./fixtures.mjs";

asAdmin();

test("sending game commands, reading logs, and using the shell", async ({ page }) => {
  await page.goto("/");
  await serverRow(page, "Survival").getByRole("link", { name: "Console" }).click();
  const status = page.locator("[data-console-status]");
  const output = page.locator(".xterm-rows");
  await expect(status).toHaveText("Connected");
  await expect(status.locator(".marker")).toHaveClass(/marker--ok/);
  await expect(output).toContainText("Connected to Minecraft RCON");

  await page.getByLabel("Game command").fill("list");
  await page.getByRole("button", { name: "Send" }).click();
  await expect(output).toContainText("There are 0 of a max of 10 players online:");
  await expect(page.locator("[data-console-feedback]")).toHaveText("Command sent. Check the output for its result.");
  await page.getByLabel("Game command").press("ArrowUp");
  await expect(page.getByLabel("Game command")).toHaveValue("list");

  await page.getByRole("link", { name: "Docker Logs" }).click();
  await expect(status).toHaveText("Connected");
  await expect(output).toContainText("Following Docker logs");
  await expect(output).toContainText("Starting minecraft server version 1.21");
  // Credentials in the container's environment never reach the browser,
  // whatever the variable is called.
  await expect(output).toContainText("Stats plugin using [redacted] for postgres://stats:[redacted]@db/stats");
  await expect(output).not.toContainText("dev-db-secret");
  await expect(output).not.toContainText("dev-url-secret");
  await page.getByRole("button", { name: "Pause" }).click();
  await expect(page.getByText("· live display paused")).toBeVisible();

  await page.getByRole("link", { name: "Container Shell" }).click();
  await expect(output).toContainText("Administrator shell.");
  await page.getByLabel("Shell command").fill("echo hello");
  await page.getByRole("button", { name: "Send" }).click();
  await expect(output).toContainText("> echo hello");
});

test("a stopped server offers no command connection", async ({ page }) => {
  await page.goto("/");
  await serverRow(page, "valheim").getByRole("link", { name: "Console" }).click();
  await page.getByRole("link", { name: "Container Shell" }).click();
  await expect(page.locator(".console-warning")).toContainText("Commands require a running server.");
  await expect(page.getByRole("button", { name: "Send" })).toBeDisabled();
  await expect(page.locator("[data-console-status]")).toHaveText("Unavailable");
});

test("a command sent just before leaving the page still runs", async ({ page }) => {
  const open = async () => {
    await page.goto("/");
    await serverRow(page, "Survival").getByRole("link", { name: "Console" }).click();
    await expect(page.locator("[data-console-status]")).toHaveText("Connected");
  };
  const send = async (command) => {
    await page.getByLabel("Game command").fill(command);
    await page.getByRole("button", { name: "Send" }).click();
    await expect(page.locator("[data-console-feedback]")).toHaveText("Command sent. Check the output for its result.");
  };
  await open();
  await send("fake-join Pat");
  await page.goto("/");

  await open();
  await send("list");
  await expect(page.locator(".xterm-rows")).toContainText("There are 1 of a max of 10 players online: Pat");
  await send("fake-leave Pat");
  await expect(page.locator(".xterm-rows")).toContainText("Pat left the game");
});
