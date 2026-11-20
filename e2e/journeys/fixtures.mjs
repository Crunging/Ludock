// Shared journey helpers. Every page fails its test on uncaught script
// errors; failed requests the journey expects are checked explicitly.
import { request } from "node:http";
import { join } from "node:path";
import { test as base, expect } from "@playwright/test";

export { expect };

// Each worker has its own Ludock and fake daemon (setup.mjs); this sets
// LUDOCK_E2E_URL, LUDOCK_E2E_ADMIN_STATE, and LUDOCK_E2E_DIR to its stack.
if (process.env.LUDOCK_E2E_STACKS) {
  Object.assign(process.env, JSON.parse(process.env.LUDOCK_E2E_STACKS)[Number(process.env.TEST_PARALLEL_INDEX)]);
}
export const admin = { username: "admin", password: "correct horse battery staple" };

const offline = new WeakSet();

/** Lets a journey take a page offline; htmx logs the requests and event streams that fail. */
export function expectOffline(page) {
  offline.add(page);
}

function watch(page, problems) {
  page.on("pageerror", (error) => problems.push(String(error)));
  page.on("console", (message) => {
    // Browsers log failed responses; journeys assert on the page instead.
    if (message.type() !== "error" || /Failed to load resource/.test(message.text())) return;
    if (offline.has(page) && /^htmx: htmx:(sse:)?error: /.test(message.text())) return;
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

/**
 * Leaves a server as other files expect it: stopped through Ludock, so
 * monitoring stays quiet, with monitoring on at its default levels.
 */
export async function restoreServer(page, name) {
  await openServer(page, name);
  const stop = page.locator(".detail-actions").getByRole("button", { name: "Stop" });
  if (await stop.count()) {
    await stop.click();
    await page.getByRole("dialog").getByRole("button", { name: "Stop server" }).click();
    await expectOperationSucceeded(page);
  }
  const id = page.url().match(/\/servers\/([^/?]+)/)[1];
  const csrf = await page.locator('meta[name="csrf-token"]').getAttribute("content");
  const response = await page.request.post(`/servers/${id}/availability`, { headers: { "HX-Request": "true" }, form: { _csrf: csrf,
    enabled: "true", graceSeconds: "120", diskAlert: "true", diskPercent: "90", memoryAlert: "true", memoryPercent: "90", memoryMinutes: "5" } });
  expect(response.ok()).toBe(true);
}

/** Waits for the operation the server's page follows to finish. */
export async function expectOperationSucceeded(page) {
  await expect(page).toHaveURL(/[?&]operation=/);
  await expect(page.locator("#selected-operation")).toContainText("Succeeded", { timeout: 45_000 });
}

/** Changes a container behind Ludock's back, as another manager would. */
export function dockerAction(container, action) {
  return new Promise((resolve, reject) => {
    const call = request({ socketPath: join(process.env.LUDOCK_E2E_DIR, "docker", "docker.sock"), method: "POST",
      path: `/v1.41/containers/${container}/${action}` }, (response) => {
      response.resume();
      response.on("end", resolve);
    });
    call.on("error", reject);
    call.end();
  });
}

/** Calls one of the fake daemon's journey controls, such as /fake/disk. */
export function fake(path, query = {}) {
  return new Promise((resolve, reject) => {
    const call = request({ socketPath: join(process.env.LUDOCK_E2E_DIR, "docker", "docker.sock"), method: "POST",
      path: path + "?" + new URLSearchParams(query) }, (response) => {
      response.resume();
      response.on("end", resolve);
    });
    call.on("error", reject);
    call.end();
  });
}

/**
 * Creates an account with a password through the users page. role is
 * "member" or "admin"; the former "operator" and "viewer" make a member.
 */
export async function createUser(page, username, password, role = "member") {
  await page.goto("/users");
  const form = page.locator("#users form.settings-card");
  await form.getByLabel("Username").fill(username);
  await form.getByLabel("Set a password now").check();
  await form.getByLabel("Initial password").fill(password);
  await form.getByLabel("Account").selectOption(role === "admin" ? "admin" : "member");
  await form.getByRole("button", { name: "Create user" }).click();
  await expect(page.locator("#users .alert--success")).toContainText(`Created ${username}.`);
}

/**
 * Shares a server with an existing member from the server's Sharing tab, as
 * an administrator. access is "Can see it", "Can run it", "Full access", or a
 * list of the Custom choices' labels, such as ["Start", "Read logs"].
 */
export async function shareServer(page, server, username, access) {
  await page.goto("/");
  await page.getByRole("link", { name: server, exact: true }).click();
  await page.getByRole("tab", { name: "Sharing" }).click();
  const sharing = page.locator("#sharing");
  await expect(sharing).toBeVisible();
  const existing = sharing.locator(".share-row", { has: page.getByText(username, { exact: true }) });
  const form = (await existing.count()) ? existing.locator("form") : sharing.locator("form.share-form");
  if (!(await existing.count())) await form.getByLabel("Person").selectOption({ label: username });
  await chooseAccess(form, access);
  await form.getByRole("button", { name: (await existing.count()) ? "Save" : "Share", exact: true }).click();
  await expect(sharing.locator(".alert--success")).toBeVisible();
}

/** Chooses a level, or Custom with exactly the given choices, in a sharing form. */
export async function chooseAccess(form, access) {
  const level = form.locator('select[name="level"]');
  if (typeof access === "string") return level.selectOption({ label: access });
  await level.selectOption({ label: "Custom" });
  for (const box of await form.locator('input[name="capability"]').all()) await box.uncheck();
  for (const label of access) await form.getByLabel(label, { exact: true }).check();
}
