import {
  apiErrorSchema,
  operationResponseSchema,
  savedScheduleSchema,
  schedulesResponseSchema,
  serverResponseSchema,
} from "@ludock/shared";
import { test, expect, ADMIN, RUNNING_ID, RUNNING_NAME } from "./fixtures";

test("lifecycle actions need confirmation, recheck the latest state, and submit once", async ({ app, page }) => {
  let releaseStop = () => {};
  const stopHeld = new Promise<void>((resolve) => { releaseStop = resolve; });
  await page.route(`**/api/v1/servers/${RUNNING_ID}/stop`, async (route) => {
    await stopHeld;
    await route.fallback();
  });
  await app.open();
  const row = page.getByRole("article", { name: RUNNING_NAME });
  const stop = row.getByRole("button", { name: "Stop", exact: true });
  const posts = () => app.requests.filter((request) => request.method === "POST");
  await stop.click();
  const dialog = page.getByRole("dialog", { name: `Stop ${RUNNING_NAME}?` });
  await expect(dialog).toBeVisible();
  expect(posts()).toEqual([]);
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  expect(posts()).toEqual([]);

  // The binding changes while the dialog is open, so confirming must not act.
  await stop.click();
  await expect.poll(() => app.sockets.length).toBe(1);
  const reads = app.requests.filter((request) => request.path === "/servers").length;
  app.servers[0].bindingStatus = "review_required";
  await app.sockets[0].route.send(JSON.stringify({ type: "container_event", action: "refresh", time: Date.now() }));
  await expect.poll(() => app.requests.filter((request) => request.path === "/servers").length).toBeGreaterThan(reads);
  await dialog.getByRole("button", { name: "Stop server", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(stop).toBeDisabled();
  expect(posts()).toEqual([]);

  app.servers[0].bindingStatus = "active";
  await app.sockets[0].route.send(JSON.stringify({ type: "container_event", action: "refresh", time: Date.now() }));
  await expect(stop).toBeEnabled();
  await stop.click();
  await dialog.getByRole("button", { name: "Stop server", exact: true }).click();
  await expect(row.getByRole("button", { name: "Working…", exact: true })).toBeDisabled();
  await row.getByRole("button", { name: `More actions for ${RUNNING_NAME}` }).click();
  await expect(row.getByRole("button", { name: /^Restart/ })).toBeDisabled();
  await page.keyboard.press("Escape");
  releaseStop();
  await expect(row.getByRole("button", { name: "Start", exact: true })).toBeVisible();
  expect(posts()).toEqual([{ method: "POST", path: `/servers/${RUNNING_ID}/stop`, query: {}, body: undefined }]);
});

test("an older detail refresh cannot restore controls after a newer one invalidated the binding", async ({ app, page }) => {
  let holdNextRead = false;
  let releaseOlder = () => {};
  const olderHeld = new Promise<void>((resolve) => { releaseOlder = resolve; });
  let olderSettled: Promise<void> = Promise.resolve();
  const activeSnapshot = { ...app.servers[0], state: "exited" };
  await page.route(`**/api/v1/servers/${RUNNING_ID}`, async (route) => {
    if (!holdNextRead) {
      await route.fallback();
      return;
    }
    holdNextRead = false;
    olderSettled = (async () => {
      await olderHeld;
      // The page may already have cancelled this request.
      await route.fulfill({ json: serverResponseSchema.parse({ server: activeSnapshot, stats: null }) }).catch(() => {});
    })();
    await olderSettled;
  });
  await app.open(`/servers/${RUNNING_ID}`);
  await expect(page.getByRole("button", { name: "Stop", exact: true })).toBeEnabled();
  await page.getByRole("tab", { name: "Activity", exact: true }).click();
  const refresh = page.getByRole("button", { name: "Refresh", exact: true });
  holdNextRead = true;
  await refresh.click();
  await expect.poll(() => holdNextRead).toBe(false);

  app.servers[0].bindingStatus = "review_required";
  await refresh.click();
  const banner = page.getByRole("alert").filter({ hasText: "This server’s binding is review required." });
  await expect(banner).toBeVisible();
  await expect(page.getByRole("button", { name: "Stop", exact: true })).toBeDisabled();

  // The older read finishes last with the binding still active and the server stopped.
  releaseOlder();
  await olderSettled;
  await page.waitForTimeout(250);
  await expect(banner).toBeVisible();
  await expect(page.getByRole("button", { name: "Start", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Stop", exact: true })).toBeDisabled();
});

test("restarts offer a backup first only with backup access", async ({ app, page }) => {
  await app.open();
  const row = page.getByRole("article", { name: RUNNING_NAME });
  await row.getByRole("button", { name: `More actions for ${RUNNING_NAME}` }).click();
  await row.getByRole("button", { name: "Restart…", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: `Restart ${RUNNING_NAME}?` });
  const backup = dialog.getByRole("checkbox", { name: "Take a backup first" });
  await expect(backup).not.toBeChecked();
  await backup.check();
  await expect(dialog).toContainText("The server stays stopped while the backup runs, then starts again. If the backup fails, it stays stopped.");
  await dialog.getByRole("button", { name: "Restart server", exact: true }).click();
  await expect.poll(() => app.requests.filter((request) => request.method === "POST")).toEqual([
    { method: "POST", path: `/servers/${RUNNING_ID}/restart`, query: {}, body: { createBackup: true } },
  ]);

  // An operator who may restart but not back up gets a plain restart and image guidance.
  app.user = { ...app.user!, role: "operator" };
  app.servers[0].permissions = ["server.view", "server.restart"];
  await app.open(`/servers/${RUNNING_ID}`);
  await page.getByRole("button", { name: "Restart", exact: true }).click();
  await expect(dialog).toBeVisible();
  await expect(backup).toHaveCount(0);
  await dialog.getByRole("button", { name: "Restart server", exact: true }).click();
  await expect.poll(() => app.requests.filter((request) => request.method === "POST").length).toBe(2);
  expect(app.requests.filter((request) => request.method === "POST")[1])
    .toEqual({ method: "POST", path: `/servers/${RUNNING_ID}/restart`, query: {}, body: undefined });
  await page.getByRole("tab", { name: "Update", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Game updates" })).toBeVisible();
  await expect(page.getByText("Pulling a newer image needs an administrator.")).toBeVisible();
  await expect(page.getByText(/To back up first/)).toHaveCount(0);
  await expect(page.getByRole("heading", { name: "Update container image" })).toHaveCount(0);
});

test("the overview shows how to connect, current resources, and the next run, without stale readings", async ({ app, page }) => {
  const now = new Date("2026-09-11T12:00:00Z");
  await page.clock.setFixedTime(now);
  let serverReadFails = false;
  await page.route(`**/api/v1/servers/${RUNNING_ID}`, async (route) => {
    if (serverReadFails) {
      await route.fulfill({ status: 500, json: apiErrorSchema.parse({ error: "Docker request failed" }) });
      return;
    }
    await route.fulfill({ json: serverResponseSchema.parse({
      server: app.servers[0], stats: { cpuPercent: 42.25, memUsageMB: 2048, memLimitMB: 8192 },
    }) });
  });
  await page.route(`**/api/v1/servers/${RUNNING_ID}/schedules`, (route) => route.fulfill({
    json: schedulesResponseSchema.parse({ schedules: [savedScheduleSchema.parse({
      id: "77777777-7777-4777-8777-777777777777", serverId: RUNNING_ID, ownerId: ADMIN.id,
      kind: "recurring", action: "restart", createBackup: false, enabled: true, time: "14:00",
      days: [0, 1, 2, 3, 4, 5, 6], timezone: "UTC", lastResult: null, lastOperation: null, lastRunAt: null,
      lastSlot: null, revision: 1, nextRunAt: now.getTime() + 2 * 3_600_000, nextRunUnavailableReason: null, completedAt: null,
    })] }),
  }));
  await app.open(`/servers/${RUNNING_ID}`);
  await expect(page.getByRole("tab", { name: "Overview", selected: true })).toBeVisible();
  const overview = page.getByRole("tabpanel", { name: "Overview" });
  await expect(overview.getByText("play.example.com:25565")).toBeVisible();
  await expect(overview.getByRole("button", { name: `Copy address for ${RUNNING_NAME}` })).toBeVisible();
  await expect(overview.getByText("42.3%")).toBeVisible();
  await expect(overview.getByText("2 GiB", { exact: true })).toBeVisible();
  await expect(overview.getByText("of 8 GiB")).toBeVisible();
  await expect(overview.getByText("in 2 hours")).toBeVisible();

  serverReadFails = true;
  await page.getByRole("tab", { name: "Activity", exact: true }).click();
  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect(page.getByText("Docker request failed")).toBeVisible();
  await page.getByRole("tab", { name: "Overview", exact: true }).click();
  await expect(overview.getByText("42.3%")).toHaveCount(0);
  await expect(overview.getByText("Not available right now")).toHaveCount(2);
});

test("a Compose update needs a fresh confirmation each time it is queued", async ({ app, page }) => {
  const updates: unknown[] = [];
  await page.route(`**/api/v1/servers/${RUNNING_ID}/updates`, async (route) => {
    updates.push(route.request().postDataJSON());
    await route.fulfill({ status: 202, json: operationResponseSchema.parse({ operation: {
      id: "c15cbd1f-dbb6-444d-8b8f-c5d728b94df3", serverId: RUNNING_ID, kind: "update",
      status: "queued", phase: "queued", createdAt: 0, updatedAt: 0, error: null, result: null,
    } }) });
  });
  await app.open(`/servers/${RUNNING_ID}?tab=update`);
  await expect(page.getByRole("heading", { name: "Game updates" })).toBeVisible();
  await expect(page.getByText(/so Restart updates the game\.$/)).toBeVisible();
  await expect(page.getByText(/To back up first, choose Take a backup first when you restart\./)).toBeVisible();
  const confirm = page.getByRole("checkbox", { name: /selected service will be recreated/ });
  const update = page.getByRole("button", { name: "Update server", exact: true });
  await expect(update).toBeDisabled();
  await confirm.check();
  await update.click();
  await expect(page.getByText("Update queued.")).toBeVisible();
  await expect(page.getByRole("tab", { name: "Activity", selected: true })).toBeVisible();
  expect(updates).toEqual([{ createBackup: true, forceRecreate: false }]);
  await page.getByRole("tab", { name: "Update", exact: true }).click();
  await expect(confirm).not.toBeChecked();
  await expect(update).toBeDisabled();
});

test("tracked shutdown progress survives list and detail reloads", async ({ app, page }) => {
  const operation = {
    id: "c15cbd1f-dbb6-444d-8b8f-c5d728b94df0", serverId: RUNNING_ID, kind: "stop",
    status: "running" as const, phase: "saving_world", createdAt: Date.now(), updatedAt: Date.now(),
    error: null, result: null,
  };
  app.operations.push(operation);
  const server = app.servers.find((item) => item.id === RUNNING_ID)!;
  server.activeOperation = operation;
  await app.open();
  let row = page.getByRole("article", { name: RUNNING_NAME });
  await expect(row.getByText("Stop: Saving world", { exact: true })).toBeVisible();
  await expect(row.getByRole("button", { name: "Stop", exact: true })).toBeDisabled();
  await page.reload();
  row = page.getByRole("article", { name: RUNNING_NAME });
  await expect(row.getByText("Stop: Saving world", { exact: true })).toBeVisible();
  await row.getByRole("link", { name: "Stop: Saving world" }).click();
  const table = page.getByRole("table", { name: "Recent operations" });
  await expect(table.getByText("Saving world", { exact: true })).toBeVisible();
  await page.reload();
  await expect(table.getByText("Saving world", { exact: true })).toBeVisible();
  operation.phase = "stopping";
  await expect(table.getByText("Waiting for server to stop", { exact: true })).toBeVisible();
  app.operations[0] = { ...operation, status: "succeeded", phase: "succeeded", result: { saveStatus: "sent" } };
  server.activeOperation = undefined;
  server.state = "exited";
  await expect(table.getByText("Save command sent before stopping.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Start", exact: true })).toBeEnabled();
  expect(app.requests.filter((request) => request.method === "POST")).toEqual([]);
});

test("an accepted update still needs fresh confirmation when its follow-up refresh fails", async ({ app, page }) => {
  let serverReadFails = false;
  const updates: unknown[] = [];
  await page.route(`**/api/v1/servers/${RUNNING_ID}/updates`, async (route) => {
    updates.push(route.request().postDataJSON());
    serverReadFails = true;
    await route.fulfill({ status: 202, json: operationResponseSchema.parse({ operation: {
      id: "c15cbd1f-dbb6-444d-8b8f-c5d728b94df4", serverId: RUNNING_ID, kind: "update",
      status: "queued", phase: "queued", createdAt: 0, updatedAt: 0, error: null, result: null,
    } }) });
  });
  await page.route(`**/api/v1/servers/${RUNNING_ID}`, async (route) => {
    if (serverReadFails) await route.fulfill({ status: 500, json: apiErrorSchema.parse({ error: "Docker request failed" }) });
    else await route.fallback();
  });
  await app.open(`/servers/${RUNNING_ID}?tab=update`);
  const confirm = page.getByRole("checkbox", { name: /selected service will be recreated/ });
  const update = page.getByRole("button", { name: "Update server", exact: true });
  await confirm.check();
  await update.click();
  await expect(page.getByText("Update queued.")).toBeVisible();
  await expect(page.getByText("Docker request failed")).toBeVisible();
  await expect(page.getByRole("tab", { name: "Activity", selected: true })).toBeVisible();

  // Once details load again, the earlier confirmation must not still be armed.
  serverReadFails = false;
  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect(page.getByText("Docker request failed")).toHaveCount(0);
  await page.getByRole("tab", { name: "Update", exact: true }).click();
  await expect(confirm).not.toBeChecked();
  await expect(update).toBeDisabled();
  await confirm.check();
  await expect(update).toBeEnabled();
  expect(updates).toEqual([{ createBackup: true, forceRecreate: false }]);
});
