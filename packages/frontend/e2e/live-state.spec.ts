import { test, expect, RUNNING_ID, RUNNING_NAME } from "./fixtures";

test("a disconnected list keeps an explicit stale snapshot and refreshes before enabling controls", async ({ app, page }) => {
  await page.clock.install();
  await app.open();
  const row = page.getByRole("article", { name: RUNNING_NAME });
  await expect(row.getByRole("button", { name: "Stop", exact: true })).toBeEnabled();
  await expect.poll(() => app.sockets.filter((socket) => socket.path === "/ws/v1/events").length).toBe(1);
  const initialRequests = app.requests.filter((request) => request.path === "/servers").length;
  await app.sockets[0].route.close({ code: 1011, reason: "Fixture connection interruption" });
  await expect(page.getByText(/Showing the last known state/)).toBeVisible();
  await expect(row.getByRole("button", { name: "Stop", exact: true })).toBeDisabled();
  expect(app.requests.filter((request) => request.method === "POST")).toEqual([]);
  app.servers = app.servers.map((server) => server.id === RUNNING_ID
    ? { ...server, state: "exited", status: "Exited (0)" }
    : server);
  await page.clock.runFor(2_100);
  await expect.poll(() => app.requests.filter((request) => request.path === "/servers").length)
    .toBeGreaterThan(initialRequests);
  await expect(row.getByRole("button", { name: "Start", exact: true })).toBeEnabled();
  await expect(row.getByRole("button", { name: "Stop", exact: true })).toHaveCount(0);
  await expect(page.getByText(/Showing the last known state/)).toHaveCount(0);
});

test("denied live access does not offer or perform a blind reconnect", async ({ app, page }) => {
  await page.clock.install();
  await app.open();
  const stop = page.getByRole("article", { name: RUNNING_NAME }).getByRole("button", { name: "Stop", exact: true });
  await expect(stop).toBeEnabled();
  await expect.poll(() => app.sockets.length).toBe(1);
  const previousReads = app.requests.filter((request) => request.path === "/servers").length;
  await app.sockets[0].route.close({ code: 4403, reason: "Fixture access revoked" });
  await expect(page.getByText(/Connection access is unavailable/)).toBeVisible();
  await expect(page.getByRole("article")).toHaveCount(0);
  await expect.poll(() => app.requests.filter((request) => request.path === "/servers").length)
    .toBeGreaterThan(previousReads);
  await expect(page.getByRole("button", { name: "Retry connection", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Reload page", exact: true })).toBeVisible();
  await page.clock.runFor(25_000);
  expect(app.sockets).toHaveLength(1);
  expect(app.requests.filter((request) => request.method === "POST")).toEqual([]);
});
