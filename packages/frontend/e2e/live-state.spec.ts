import type { Route } from "@playwright/test";
import { test, expect, makeServers, RUNNING_ID, RUNNING_NAME } from "./fixtures";

test("a disconnected list keeps an explicit stale snapshot and refreshes before enabling controls", async ({ app, page }) => {
  await page.clock.install();
  await app.open();
  const row = page.getByRole("article", { name: RUNNING_NAME });
  await expect(row.getByRole("button", { name: "Stop", exact: true })).toBeEnabled();
  await expect.poll(() => app.sockets.filter((socket) => socket.path === "/ws/v1/events").length).toBe(1);
  const initialRequests = app.requests.filter((request) => request.path === "/servers").length;
  await app.sockets[0].route.close({ code: 1011, reason: "Fixture connection interruption" });
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
});

test("denied live access does not offer or perform a blind reconnect", async ({ app, page }) => {
  await page.clock.install();
  await app.open();
  const stop = page.getByRole("article", { name: RUNNING_NAME }).getByRole("button", { name: "Stop", exact: true });
  await expect(stop).toBeEnabled();
  await expect.poll(() => app.sockets.length).toBe(1);
  const previousReads = app.requests.filter((request) => request.path === "/servers").length;
  await app.sockets[0].route.close({ code: 4403, reason: "Fixture access revoked" });
  await expect(page.getByRole("article")).toHaveCount(0);
  await expect.poll(() => app.requests.filter((request) => request.path === "/servers").length)
    .toBeGreaterThan(previousReads);
  await expect(page.getByRole("button", { name: "Retry connection", exact: true })).toHaveCount(0);
  await page.clock.runFor(25_000);
  expect(app.sockets).toHaveLength(1);
  expect(app.requests.filter((request) => request.method === "POST")).toEqual([]);
});

test("a policy close discards an in-flight server list and its late response", async ({ app, page }) => {
  await page.clock.install();
  await app.open();
  await expect(page.getByRole("article", { name: RUNNING_NAME })
    .getByRole("button", { name: "Stop", exact: true })).toBeEnabled();
  await expect.poll(() => app.sockets.length).toBe(1);
  const held: Route[] = [];
  await page.route("**/api/v1/servers", (route) => { held.push(route); });
  const aborted: string[] = [];
  page.on("requestfailed", (request) => {
    if (new URL(request.url()).pathname === "/api/v1/servers") aborted.push(request.failure()?.errorText || "");
  });
  await app.sockets[0].route.send(JSON.stringify({ type: "container_event", action: "refresh", time: 1 }));
  await expect.poll(() => held.length).toBe(1);
  await app.sockets[0].route.close({ code: 4403, reason: "Fixture access revoked" });
  // Rows disappear before any HTTP response confirms the revocation.
  await expect(page.getByRole("article")).toHaveCount(0);
  await expect.poll(() => held.length).toBe(2);
  await expect.poll(() => aborted).toHaveLength(1);
  app.servers = [];
  await held[1].fallback();
  // The status (and its reload action) appears once the revalidation finishes loading.
  await expect(page.getByRole("button", { name: "Reload page", exact: true })).toBeVisible();
  // The browser already canceled the outdated read; its body must never land.
  await held[0].fulfill({ json: { servers: makeServers() } }).catch(() => undefined);
  await expect(page.getByRole("article")).toHaveCount(0);
  await expect(page.getByRole("list", { name: "Game servers" })).toHaveCount(0);
  await expect(page.getByText(RUNNING_NAME)).toHaveCount(0);
  await expect(page.getByText(/\d+ of \d+ servers/)).toHaveCount(0);
  await expect(page.getByText("Showing the last known state", { exact: false })).toHaveCount(0);
});
