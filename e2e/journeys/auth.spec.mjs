import { test, expect, admin, signIn } from "./fixtures.mjs";

test("signing in, returning to the requested page, and signing out", async ({ page }) => {
  await page.goto("/operations?status=failed");
  await expect(page).toHaveURL(/\/login\?next=/);
  await expect(page.getByRole("heading", { name: "Sign in to Ludock" })).toBeVisible();

  await page.getByLabel("Username").fill(admin.username);
  await page.getByLabel("Password", { exact: true }).fill("not the right password");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.locator(".login-card__error")).toContainText("Invalid username or password");

  await page.getByLabel("Password", { exact: true }).fill(admin.password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page).toHaveURL(/\/operations\?status=failed$/);
  await expect(page.getByRole("heading", { name: "Operation history" })).toBeVisible();

  await page.getByRole("button", { name: "Sign out" }).first().click();
  await expect(page).toHaveURL(/\/login$/);
  await page.goto("/");
  await expect(page).toHaveURL(/\/login/);
});

test("setup is closed once an administrator exists", async ({ page }) => {
  await signIn(page, admin.username, admin.password);
  await expect(page.getByRole("heading", { name: "Servers", exact: true })).toBeVisible();
  const response = await page.request.post("/setup", {
    form: { bootstrapCode: "e2e-setup-code-0123456789abcdef-0123", username: "second", password: "another long password", confirmation: "another long password" },
    maxRedirects: 0,
  });
  expect(response.status()).toBeGreaterThanOrEqual(400);
});
