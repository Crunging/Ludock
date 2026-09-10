import { test, expect, RUNNING_NAME } from "./fixtures";

test("server tools return to the remembered detail tab and filtered list", async ({ app, page }) => {
  await app.open();
  const search = page.getByRole("searchbox", { name: "Find a server" });
  await search.fill("survival");
  await page.getByRole("combobox", { name: "State", exact: true }).selectOption("running");
  await page.getByRole("link", { name: RUNNING_NAME, exact: true }).click();
  await page.getByRole("tab", { name: "Schedules", exact: true }).click();
  await page.getByRole("link", { name: "Files", exact: true }).click();
  await expect(page.getByText("server.properties", { exact: true })).toBeVisible();
  await page.getByRole("link", { name: /Back to server/ }).click();
  await expect(page.getByRole("tab", { name: "Schedules", selected: true })).toBeVisible();
  await page.getByRole("link", { name: "All servers", exact: true }).click();
  await expect(search).toHaveValue("survival");
  await expect(page.getByRole("combobox", { name: "State", exact: true })).toHaveValue("running");
  await expect(page.getByRole("article")).toHaveCount(1);
});

test("signing out clears the previous account’s remembered list filters", async ({ app, page }) => {
  await app.open();
  await page.getByRole("searchbox", { name: "Find a server" }).fill("factorio");
  await page.getByRole("combobox", { name: "State", exact: true }).selectOption("exited");
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Sign in to Ludock" })).toBeVisible();
  await page.getByRole("textbox", { name: "Username", exact: true }).fill("another-admin");
  await page.getByLabel("Password", { exact: true }).fill("fixture-password-123");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("searchbox", { name: "Find a server" })).toHaveValue("");
  await expect(page.getByRole("combobox", { name: "State", exact: true })).toHaveValue("all");
  await expect(page.getByRole("article")).toHaveCount(2);
});

test("an unavailable page download keeps navigation and filters available until explicit reload", async ({ app, page }) => {
  await app.open();
  const search = page.getByRole("searchbox", { name: "Find a server" });
  await search.fill("survival");
  await page.getByRole("combobox", { name: "State", exact: true }).selectOption("running");
  await page.getByRole("link", { name: RUNNING_NAME, exact: true }).click();
  await expect(page.getByRole("link", { name: "Files", exact: true })).toBeVisible();
  const sessionReads = app.requests.filter((request) => request.path === "/auth/status").length;
  let failedDownloads = 0;
  await page.route("**/*.js", (route) => {
    failedDownloads += 1;
    return route.abort();
  });
  await page.getByRole("link", { name: "Files", exact: true }).click();
  const main = page.getByRole("main");
  await expect(main.getByRole("heading", { name: "Page unavailable" })).toBeVisible();
  expect(failedDownloads).toBeGreaterThan(0);
  await expect(page.getByRole("navigation", { name: "Primary navigation" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Sign out", exact: true })).toBeVisible();
  await expect(main.getByRole("button", { name: "Reload page", exact: true })).toBeVisible();
  await expect(main).not.toContainText(/Failed to fetch|TypeError|\.js/);

  await main.getByRole("link", { name: "Servers", exact: true }).click();
  await expect(search).toHaveValue("survival");
  await expect(page.getByRole("combobox", { name: "State", exact: true })).toHaveValue("running");
  await expect(page.getByRole("article")).toHaveCount(1);
  expect(app.requests.filter((request) => request.path === "/auth/status")).toHaveLength(sessionReads);

  await page.unroute("**/*.js");
  await page.getByRole("link", { name: RUNNING_NAME, exact: true }).click();
  await page.getByRole("link", { name: "Files", exact: true }).click();
  await expect(main.getByRole("heading", { name: "Page unavailable" })).toBeVisible();
  await main.getByRole("button", { name: "Reload page", exact: true }).click();
  await expect(page.getByText("server.properties", { exact: true })).toBeVisible();
  await expect(main.getByRole("heading", { name: "Page unavailable" })).toHaveCount(0);
  expect(app.requests.filter((request) => request.path === "/auth/status")).toHaveLength(sessionReads + 1);
});
