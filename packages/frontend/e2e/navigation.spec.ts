import { serversResponseSchema } from "@ludock/shared";
import { test, expect, ADMIN, RUNNING_ID, RUNNING_NAME } from "./fixtures";

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

test("a late server list from a signed-out account is never shown to the next account", async ({ app, page }) => {
  let releaseOldList = () => {};
  const oldList = new Promise<void>((resolve) => { releaseOldList = resolve; });
  let holdNextList = false;
  let oldListDelivered = false;
  await page.route("**/api/v1/servers", async (route) => {
    if (!holdNextList) {
      await route.fallback();
      return;
    }
    holdNextList = false;
    const servers = app.servers;
    await oldList;
    await route.fulfill({ json: serversResponseSchema.parse({ servers }) }).catch(() => {});
    oldListDelivered = true;
  });
  await app.open();
  await expect(page.getByRole("article", { name: RUNNING_NAME })).toBeVisible();
  await expect.poll(() => app.sockets.length).toBe(1);
  holdNextList = true;
  await app.sockets[0].route.send(JSON.stringify({ type: "container_event", action: "refresh", time: Date.now() }));
  await expect.poll(() => holdNextList).toBe(false);

  await page.getByRole("button", { name: "Sign out" }).first().click();
  await expect(page.getByRole("heading", { name: "Sign in to Ludock" })).toBeVisible();
  // The next account was granted only the Factorio server.
  app.servers = app.servers.filter((server) => server.id !== RUNNING_ID);
  await page.getByRole("textbox", { name: "Username", exact: true }).fill("friend");
  await page.getByLabel("Password", { exact: true }).fill("fixture-password-123");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("article", { name: "Factorio weekend" })).toBeVisible();
  releaseOldList();
  await expect.poll(() => oldListDelivered).toBe(true);
  await expect(page.getByRole("article", { name: "Factorio weekend" })).toBeVisible();
  await expect(page.getByRole("article", { name: RUNNING_NAME })).toHaveCount(0);
});
