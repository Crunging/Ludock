import { operationsResponseSchema, updateCapabilityResponseSchema } from "@ludock/shared";
import { test, expect, RUNNING_ID } from "./fixtures";

test("operation progress stays reachable from blocked server controls", async ({ app, page }) => {
  await page.route(`**/api/v1/servers/${RUNNING_ID}/operations`, (route) => route.fulfill({
    json: operationsResponseSchema.parse({ operations: [{
      id: "c15cbd1f-dbb6-444d-8b8f-c5d728b94df0",
      serverId: RUNNING_ID,
      kind: "backup",
      status: "running",
      phase: "copying_data",
      createdAt: 0,
      updatedAt: 0,
      error: null,
      result: null,
    }] }),
  }));
  await app.open(`/servers/${RUNNING_ID}`);
  await page.getByRole("tab", { name: "Backups", exact: true }).click();
  await expect(page.getByRole("status")).toContainText("backup in progress");
  await expect(page.getByRole("button", { name: "Stop", exact: true })).toBeDisabled();
  await expect(page.getByRole("button", { name: "Create backup", exact: true })).toBeDisabled();
  await expect(page.getByRole("link", { name: "backup settings", exact: true })).toHaveAttribute("href", "/settings");
  await page.getByRole("button", { name: "View progress", exact: true }).focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("tabpanel", { name: "Activity", exact: true })).toBeFocused();
  await expect(page.getByRole("status")).toHaveText("copying data");
  expect(app.requests.filter((request) => request.method !== "GET")).toEqual([]);
  await page.getByRole("tab", { name: "Backups", exact: true }).click();
  await page.setViewportSize({ width: 320, height: 844 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.screenshot({ path: test.info().outputPath("operation-progress.png"), fullPage: true });
});

test("checking update availability preserves monitoring drafts and confirmation", async ({ app, page }) => {
  let checks = 0;
  await page.route(`**/api/v1/servers/${RUNNING_ID}/update-capability`, (route) => {
    checks += 1;
    return route.fulfill({ json: updateCapabilityResponseSchema.parse({
      capability: checks === 1
        ? { available: false, actionLabel: "Update server", manager: "compose", unavailableReason: "Wait for the active operation to finish before updating" }
        : { available: true, actionLabel: "Update server", manager: "compose", projectName: "fixture-games", serviceName: "minecraft", image: app.servers[0].image },
    }) });
  });
  await app.open(`/servers/${RUNNING_ID}`);
  await page.getByRole("tab", { name: "Availability", exact: true }).click();
  await page.getByRole("spinbutton", { name: "Failure grace period (seconds)", exact: true }).fill("240");
  await page.getByRole("tab", { name: "Update", exact: true }).click();
  await expect(page.getByRole("link", { name: "Check source access", exact: true })).toHaveAttribute("href", "/settings");
  await page.screenshot({ path: test.info().outputPath("update-unavailable.png"), fullPage: true });
  await page.getByRole("button", { name: "Check again", exact: true }).click();
  await expect(page.getByRole("button", { name: "Update server", exact: true })).toBeDisabled();
  await expect(page.getByRole("tabpanel", { name: "Update", exact: true })).toBeFocused();
  await expect(page.getByRole("checkbox", { name: /I understand/ })).not.toBeChecked();
  expect(checks).toBe(2);
  await page.getByRole("tab", { name: "Availability", exact: true }).click();
  await expect(page.getByRole("spinbutton", { name: "Failure grace period (seconds)", exact: true })).toHaveValue("240");
  expect(app.requests.filter((request) => request.path.endsWith("/availability"))).toHaveLength(1);
  expect(app.requests.filter((request) => request.method !== "GET")).toEqual([]);
});

test("paused containers explain recovery and keep permitted logs reachable", async ({ app, page }) => {
  app.servers[0].state = "paused";
  await app.open(`/servers/${RUNNING_ID}`);
  await expect(page.getByRole("status")).toContainText("Paused in Docker. Resume it through Docker or its owning manager.");
  for (const action of ["Start", "Stop", "Restart"])
    await expect(page.getByRole("button", { name: action, exact: true })).toHaveCount(0);
  await page.getByRole("link", { name: "Console", exact: true }).click();
  await expect(page.getByRole("textbox", { name: "Game command", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Send", exact: true })).toBeDisabled();
  await expect(page.getByRole("link", { name: "Open server controls", exact: true })).toHaveCount(0);
  await expect(page.getByText(/Paused in Docker. Resume it through Docker/)).toBeVisible();
  await page.screenshot({ path: test.info().outputPath("paused-console.png"), fullPage: true });
  await page.getByRole("button", { name: "View logs", exact: true }).click();
  await expect(page.getByRole("tab", { name: "Docker Logs", exact: true })).toBeFocused();
  await expect.poll(() => app.sockets.map((socket) => socket.path)).toEqual([`/ws/v1/logs/${RUNNING_ID}`]);
  expect(app.sockets.flatMap((socket) => socket.messages)).toEqual([]);
  expect(app.requests.filter((request) => request.method !== "GET")).toEqual([]);
});

test("a stopped console links to the permitted Start control", async ({ app, page }) => {
  app.servers[0].state = "exited";
  await app.open(`/console/${RUNNING_ID}`);
  await expect(page.getByRole("button", { name: "Send", exact: true })).toBeDisabled();
  await page.screenshot({ path: test.info().outputPath("stopped-console.png"), fullPage: true });
  await page.getByRole("link", { name: "Open server controls", exact: true }).click();
  await page.getByRole("button", { name: "Start", exact: true }).click();
  await expect(page.getByText("Server started.", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Stop", exact: true })).toBeEnabled();
  expect(app.requests.filter((request) => request.method !== "GET").map((request) => request.path))
    .toEqual([`/servers/${RUNNING_ID}/start`]);
  expect(app.sockets.flatMap((socket) => socket.messages)).toEqual([]);
});

test("Settings explains automatic updates without a project registration form", async ({ app, page }) => {
  await page.route("**/api/v1/settings/deployment", (route) => route.fulfill({
    json: { backupRoots: ["/backups"], composeRoots: ["/srv/games"], composeAvailable: true },
  }));
  await page.route("**/api/v1/settings/backups", (route) => route.fulfill({ json: { settings: null } }));
  await page.route("**/api/v1/notifications", (route) => route.fulfill({ json: { configured: false, enabled: false } }));
  await app.open("/settings");
  const heading = page.getByRole("heading", { name: "Compose updates", exact: true });
  await heading.scrollIntoViewIfNeeded();
  await expect(heading).toBeVisible();
  await expect(page.getByText(/No project registration is needed/)).toBeVisible();
  await expect(page.getByLabel("Compose project name")).toHaveCount(0);
  expect(app.requests.some((request) => request.path === "/compose-projects")).toBe(false);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});
