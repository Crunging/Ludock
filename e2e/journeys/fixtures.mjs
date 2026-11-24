// Shared journey helpers. Every page fails its test on uncaught script
// errors; failed requests the journey expects are checked explicitly.
import { randomBytes } from "node:crypto";
import { rmSync } from "node:fs";
import { request } from "node:http";
import { join } from "node:path";
import { test as base, expect, request as playwrightRequest } from "@playwright/test";

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
  // ownServer is a running Minecraft server like Survival that only this
  // journey sees, with its own console and data folder, for a journey that
  // changes a server. It is gone from Ludock and Docker afterwards.
  ownServer: async ({}, use) => {
    const server = await addServer();
    try {
      await use(server);
    } finally {
      await server.remove();
    }
  },
  // otherServer is a second server like ownServer, so a journey can let two
  // servers' timers run side by side instead of one after another.
  otherServer: async ({}, use) => {
    const server = await addServer();
    try {
      await use(server);
    } finally {
      await server.remove();
    }
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
 * Sends a command from a server's console page. The fake game consoles take
 * fake-join, fake-leave, fake-outage, fake-hang, and fake-said, which change
 * who is online and what their list command answers; a command sent from the
 * console also starts a new reading of who is online.
 */
export async function gameCommand(page, text, server = "Survival") {
  await page.goto("/");
  await serverRow(page, server).getByRole("link", { name: "Console" }).click();
  await expect(page.locator("[data-console-status]")).toHaveText("Connected");
  await page.getByLabel("Game command").fill(text);
  await page.getByRole("button", { name: "Send" }).click();
  await expect(page.locator("[data-console-feedback]")).toHaveText("Command sent. Check the output for its result.");
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

/** Opens the server page's Monitoring settings, unless open, and returns them. */
export async function monitoringSettings(page) {
  const settings = page.locator("#monitoring-settings");
  if (!(await settings.evaluate((details) => details.open))) await settings.locator("summary").click();
  return settings;
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

/**
 * Calls one of the fake daemon's journey controls, such as /fake/disk, on
 * the worker's stack or the one whose folder is dir.
 */
export function fake(path, query = {}, dir = process.env.LUDOCK_E2E_DIR) {
  return new Promise((resolve, reject) => {
    const call = request({ socketPath: join(dir, "docker", "docker.sock"), method: "POST",
      path: path + "?" + new URLSearchParams(query) }, (response) => {
      response.resume();
      response.on("end", resolve);
    });
    call.on("error", reject);
    call.end();
  });
}

/**
 * Adds one of the fake daemon's real-image games for a journey, and takes it
 * away after. Its name is its own: the fake daemon's own factorio, which
 * Ludock leaves out, would otherwise share it.
 */
export async function withGame(page, game, run) {
  const name = `${game}-${randomBytes(3).toString("hex")}`;
  await fake("/fake/game", { game, name });
  try {
    // Ludock finds it from Docker's events, shortly after.
    await expect(async () => {
      await page.goto("/");
      await expect(serverRow(page, name)).toBeVisible({ timeout: 1000 });
    }).toPass();
    await run(name);
  } finally {
    await openServer(page, name);
    await page.getByRole("button", { name: "Remove from Ludock" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Remove server" }).click();
    await expect(page).toHaveURL(/\/$/);
    await fake("/fake/remove", { container: name });
  }
}

/**
 * Adds a running Minecraft server like Survival to the fake daemon and waits
 * for Ludock to find it. Returns its name in Ludock, its container's name,
 * its image, which no other server uses, its Ludock ID and data folder, and
 * remove(), which takes it out of Ludock and then Docker, so nothing waits on
 * it as missing.
 */
export async function addServer() {
  const container = "mc-" + randomBytes(4).toString("hex");
  const name = "Realm " + container;
  const data = join(process.env.LUDOCK_E2E_DIR, "docker", "data", container);
  const api = await playwrightRequest.newContext({ baseURL: process.env.LUDOCK_E2E_URL, storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
  let id;
  const unmanage = (timeout) => expect.poll(async () => {
    const csrf = (await (await api.get("/")).text()).match(/name="csrf-token" content="([^"]+)"/)?.[1];
    const response = await api.post(`/servers/${id}/remove`, { headers: { "HX-Request": "true" }, form: { _csrf: csrf } });
    return response.headers()["hx-redirect"] === "/";
  }, { message: `Ludock removed ${name}`, timeout }).toBe(true);
  const remove = async () => {
    try {
      // Work still finishing, such as a cancelled restart, holds removal
      // back. Work a failed journey left running ends once its container
      // is gone, so a server never outlives its journey into the next.
      const removed = !id || await unmanage(30_000).then(() => true, () => false);
      await fake("/fake/remove", { container });
      if (!removed) await unmanage(30_000);
      rmSync(data, { recursive: true, force: true });
    } finally {
      await api.dispose();
    }
  };
  try {
    await fake("/fake/game", { game: "minecraft", name: container });
    // Ludock finds it from Docker's events, shortly after.
    await expect.poll(async () => {
      const page = await (await api.get("/")).text();
      id = page.match(new RegExp(`href="/servers/([^"/?]+)"[^>]*>${name}<`))?.[1];
      return id;
    }, { message: `Ludock found ${name}`, timeout: 20_000 }).toBeTruthy();
  } catch (error) {
    await remove();
    throw error;
  }
  return { name, container, image: "itzg/minecraft-server:" + container, id, data, remove };
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
  // Hashing the password is deliberately slow, and slower while other
  // workers hash theirs.
  await expect(page.locator("#users .alert--success")).toContainText(`Created ${username}.`, { timeout: 30_000 });
}

/**
 * Shares a server with an existing member from the server's Sharing tab, as
 * an administrator. access is "Can see it", "Can run it", "Full access", or a
 * list of the Custom choices' labels, such as ["Start", "Read logs"].
 */
export async function shareServer(page, server, username, access) {
  await page.goto("/");
  await page.getByRole("link", { name: server, exact: true }).click();
  await page.locator(".section-tabs").getByRole("link", { name: "Sharing" }).click();
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
