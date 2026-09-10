import { test, expect, RUNNING_ID, RUNNING_NAME } from "./fixtures";

test("a restart-only operator can restart without receiving stop or file access", async ({ app, page }) => {
  app.user = { id: "22222222-2222-4222-8222-222222222222", username: "casey", role: "operator" };
  app.servers = [{ ...app.servers[0], permissions: ["server.view", "server.restart"] }];
  await app.open();
  const row = page.getByRole("article", { name: RUNNING_NAME });
  await expect(row.getByRole("button", { name: "Stop", exact: true })).toHaveCount(0);
  await expect(row.getByRole("link", { name: /Console|Logs/ })).toHaveCount(0);
  await expect(page.getByRole("link", { name: "Users", exact: true })).toHaveCount(0);
  await row.getByRole("button", { name: /More actions/ }).click();
  await expect(row.getByRole("button", { name: "Files", exact: true })).toHaveCount(0);
  await row.getByRole("button", { name: "Restart…", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: `Restart ${RUNNING_NAME}?` });
  await expect(dialog.getByRole("button", { name: "Cancel", exact: true })).toBeFocused();
  expect(app.requests.filter((request) => request.method === "POST")).toEqual([]);
  await dialog.getByRole("button", { name: "Restart server", exact: true }).click();
  await expect.poll(() => app.requests.filter((request) => request.method === "POST").map((request) => request.path))
    .toEqual([`/servers/${RUNNING_ID}/restart`]);
});

test("viewer role caps stale mutation grants while retaining independently granted logs", async ({ app, page }) => {
  app.user = { id: "33333333-3333-4333-8333-333333333333", username: "sam", role: "viewer" };
  app.servers = [{
    ...app.servers[0],
    permissions: ["server.view", "server.start", "server.stop", "server.restart", "logs.read", "files.write"],
  }];
  await app.open();
  const row = page.getByRole("article", { name: RUNNING_NAME });
  await expect(row.getByRole("button", { name: /^(Start|Stop|Restart)$/ })).toHaveCount(0);
  await expect(row.getByRole("link", { name: "Logs", exact: true })).toBeVisible();
  await expect(row.getByRole("link", { name: "Console", exact: true })).toHaveCount(0);
  await expect(row.getByRole("button", { name: /More actions/ })).toHaveCount(0);
  await row.getByRole("link", { name: RUNNING_NAME }).click();
  await expect(page.getByRole("tab", { name: "Activity", exact: true })).toBeVisible();
  await expect(page.getByRole("tab")).toHaveCount(1);
  await expect(page.getByRole("button", { name: /^(Start|Stop|Restart)$/ })).toHaveCount(0);
  expect(app.requests.filter((request) => request.method !== "GET")).toEqual([]);
});

test("file read access exposes downloads without write controls", async ({ app, page }) => {
  app.user = { id: "22222222-2222-4222-8222-222222222222", username: "casey", role: "operator" };
  app.servers = [{ ...app.servers[0], permissions: ["server.view", "files.read"] }];
  await app.open(`/files/${RUNNING_ID}`);
  await expect(page.getByText("server.properties", { exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: "Download", exact: true })).toHaveCount(2);
  await expect(page.getByRole("button", { name: /^(Rename|Delete|New folder|Choose files)$/ })).toHaveCount(0);
  await expect(page.getByText("Your account can browse and download files but cannot change them.")).toBeVisible();
  expect(app.requests.filter((request) => request.method !== "GET")).toEqual([]);
});
