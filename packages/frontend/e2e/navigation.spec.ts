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
