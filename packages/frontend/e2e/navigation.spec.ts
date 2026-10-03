import { test, expect, ADMIN, RUNNING_ID } from "./fixtures";

test("signing in continues to the requested server page", async ({ app, page }) => {
  app.user = null;
  await app.open(`/files/${RUNNING_ID}`);
  await expect(page.getByRole("heading", { name: "Sign in to Ludock" })).toBeVisible();
  await expect(page).toHaveURL(`/files/${RUNNING_ID}`);
  expect(app.requests.filter((request) => request.path !== "/auth/status")).toEqual([]);

  await page.getByRole("textbox", { name: "Username", exact: true }).fill(ADMIN.username);
  await page.getByLabel("Password", { exact: true }).fill("fixture-password-123");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByText("server.properties", { exact: true })).toBeVisible();
  await expect(page).toHaveURL(`/files/${RUNNING_ID}`);
  expect(app.requests.filter((request) => request.method !== "GET").map((request) => request.path))
    .toEqual(["/auth/login"]);
});
