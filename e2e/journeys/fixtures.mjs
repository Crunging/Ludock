// Shared journey helpers. Every page fails its test on uncaught script
// errors; failed requests the journey expects are checked explicitly.
import { test as base, expect } from "@playwright/test";

export { expect };
export const admin = { username: "admin", password: "correct horse battery staple" };

function watch(page, problems) {
  page.on("pageerror", (error) => problems.push(String(error)));
  page.on("console", (message) => {
    // Browsers log failed responses and closed event streams; journeys
    // assert on the page instead.
    if (message.type() === "error" && !/Failed to load resource|\[object Event\]/.test(message.text())) problems.push(message.text());
  });
  page.on("dialog", (dialog) => dialog.accept());
  return page;
}

export const test = base.extend({
  page: async ({ page }, use) => {
    const problems = [];
    await use(watch(page, problems));
    expect(problems, "browser errors").toEqual([]);
  },
  // session(options) opens another browser profile, such as a second user or
  // device, with the same checks as page.
  session: async ({ browser }, use) => {
    const problems = [];
    const contexts = [];
    await use(async (options = {}) => {
      const context = await browser.newContext(options);
      contexts.push(context);
      return watch(await context.newPage(), problems);
    });
    for (const context of contexts) await context.close();
    expect(problems, "browser errors").toEqual([]);
  },
});

/** Journeys in a file run as the setup administrator. */
export function asAdmin() {
  test.use({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
}

export async function signIn(page, username, password) {
  await page.goto("/login");
  await page.getByLabel("Username").fill(username);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
}

export function serverRow(page, name) {
  return page.locator(".server-row", { has: page.getByRole("link", { name, exact: true }) });
}

export async function openServer(page, name) {
  await page.goto("/");
  await page.getByRole("link", { name, exact: true }).click();
  await expect(page.getByRole("heading", { level: 1, name })).toBeVisible();
}

/** Waits for the operation shown in the server's activity to finish. */
export async function expectOperationSucceeded(page) {
  await expect(page).toHaveURL(/tab=activity&operation=/);
  await expect(page.locator("#selected-operation")).toContainText("Succeeded", { timeout: 45_000 });
}

/** Creates an account through the users page; returns nothing. */
export async function createUser(page, username, password, role) {
  await page.goto("/users");
  const form = page.locator("#users form.settings-card");
  await form.getByLabel("Username").fill(username);
  await form.getByLabel("Initial password").fill(password);
  await form.getByLabel("Role").selectOption(role);
  await form.getByRole("button", { name: "Create user" }).click();
  await expect(page.locator("#users .alert--success")).toContainText(`Created ${username}.`);
}
