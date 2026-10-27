// Shared journey helpers. Every page fails its test on uncaught script
// errors; failed requests the journey expects are checked explicitly.
import { test as base, expect } from "@playwright/test";

export { expect };

// Each worker has its own Ludock and fake daemon (setup.mjs); this sets
// LUDOCK_E2E_URL, LUDOCK_E2E_ADMIN_STATE, and LUDOCK_E2E_DIR to its stack.
if (process.env.LUDOCK_E2E_STACKS) {
  Object.assign(process.env, JSON.parse(process.env.LUDOCK_E2E_STACKS)[Number(process.env.TEST_PARALLEL_INDEX)]);
}
export const admin = { username: "admin", password: "correct horse battery staple" };

const offline = new WeakSet();

/** Lets a journey take a page offline; htmx logs the requests that fail. */
export function expectOffline(page) {
  offline.add(page);
}

function watch(page, problems) {
  page.on("pageerror", (error) => problems.push(String(error)));
  page.on("console", (message) => {
    // Browsers log failed responses and closed event streams, and htmx logs
    // the 401 that sends the page to sign in; journeys assert on the page
    // instead.
    if (message.type() !== "error" || /Failed to load resource|\[object Event\]|^Response Status Error Code 401 /.test(message.text())) return;
    if (offline.has(page) && /^htmx:(sendError|afterRequest), /.test(message.text())) return;
    problems.push(message.text());
  });
  // Ludock asks in its own dialogs; a browser dialog is a regression.
  page.on("dialog", (dialog) => {
    problems.push(`browser ${dialog.type()} dialog: ${dialog.message()}`);
    dialog.dismiss();
  });
  return page;
}

export const test = base.extend({
  baseURL: async ({ baseURL }, use) => use(process.env.LUDOCK_E2E_URL || baseURL),
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

/** Waits for the operation the server's page follows to finish. */
export async function expectOperationSucceeded(page) {
  await expect(page).toHaveURL(/[?&]operation=/);
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
