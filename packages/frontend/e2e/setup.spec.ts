import { authStatusSchema, authUserResponseSchema, setupRequestSchema } from "@ludock/shared";
import { test, expect, ADMIN } from "./fixtures";

test("expired setup shows the commands to recover and fits a narrow screen", { tag: "@mobile" }, async ({ app, page }) => {
  app.user = null;
  let locked = true;
  await page.route("**/api/v1/auth/status", (route) => route.fulfill({ json: authStatusSchema.parse({
    setupRequired: true, setupLocked: locked,
    setupExpiresAt: Date.now() + (locked ? -1 : 300_000),
    setupRemainingMs: locked ? 0 : 300_000,
    authenticated: false, user: null,
  }) }));
  await app.open("/");
  await expect(page.getByRole("heading", { name: "Setup window expired" })).toBeVisible();
  await expect(page.getByText(/docker compose restart ludock/)).toBeVisible();
  await page.setViewportSize({ width: 320, height: 844 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.screenshot({ path: test.info().outputPath("expired-setup.png"), fullPage: true });
  locked = false;
  await page.getByRole("button", { name: "Check again" }).click();
  await expect(page.getByRole("heading", { name: "Set up Ludock" })).toBeVisible();
  await expect(page.getByLabel("Setup code", { exact: true })).toBeFocused();
  expect(app.requests.filter((request) => request.method !== "GET")).toEqual([]);
});

test("setup keeps rejected drafts and sends its code only in the request body", async ({ app, page }) => {
  app.user = null;
  await page.route("**/api/v1/auth/status", async (route) => {
    await route.fulfill({ json: authStatusSchema.parse({
      setupRequired: true,
      setupLocked: false,
      setupExpiresAt: Date.now() + 300_000,
      setupRemainingMs: 300_000,
      authenticated: false,
      user: null,
    }) });
  });
  const submissions: unknown[] = [];
  await page.route("**/api/v1/auth/setup", async (route) => {
    expect(route.request().method()).toBe("POST");
    expect(new URL(route.request().url()).search).toBe("");
    submissions.push(setupRequestSchema.parse(route.request().postDataJSON()));
    if (submissions.length === 1) {
      await route.fulfill({ status: 403, json: { error: "Setup code is invalid" } });
      return;
    }
    app.user = { ...ADMIN };
    await route.fulfill({ json: authUserResponseSchema.parse({ user: app.user }) });
  });

  await app.open();
  const code = page.getByLabel("Setup code", { exact: true });
  const username = page.getByLabel("Username", { exact: true });
  const password = page.getByLabel("Password", { exact: true });
  const confirmation = page.getByLabel("Confirm password", { exact: true });
  const wrongCode = "rejected-fixture-setup-code-123456789";
  const correctCode = "accepted-fixture-setup-code-123456789";
  const fixturePassword = "fixture-setup-password-123";
  await expect(code).toBeFocused();
  await expect(code).toHaveAttribute("type", "password");
  await code.fill(wrongCode);
  await username.fill(ADMIN.username);
  await password.fill(fixturePassword);
  await confirmation.fill(fixturePassword);
  await confirmation.press("Enter");
  await expect(page.getByRole("alert")).toHaveText("Setup code is invalid");
  await expect(code).toHaveValue(wrongCode);
  await expect(username).toHaveValue(ADMIN.username);
  await expect(password).toHaveValue(fixturePassword);
  await expect(confirmation).toHaveValue(fixturePassword);
  await code.fill(correctCode);
  await page.getByRole("button", { name: "Create administrator", exact: true }).click();
  await expect(page.getByRole("searchbox", { name: "Find a server" })).toBeVisible();
  expect(submissions).toEqual([wrongCode, correctCode].map((bootstrapCode) => ({
    username: ADMIN.username, password: fixturePassword, bootstrapCode,
  })));
  const browserState = await page.evaluate(() => JSON.stringify({
    url: location.href,
    local: { ...localStorage },
    session: { ...sessionStorage },
  }));
  for (const secret of [wrongCode, correctCode, fixturePassword]) {
    expect(browserState).not.toContain(secret);
  }
});
