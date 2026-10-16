import { test, expect, RUNNING_ID, RUNNING_NAME } from "./fixtures";

test("lifecycle cancellation sends nothing and confirmation submits once", async ({ app, page }) => {
  await app.open();
  const row = page.getByRole("article", { name: RUNNING_NAME });
  const stop = row.getByRole("button", { name: "Stop", exact: true });
  await stop.click();
  const dialog = page.getByRole("dialog", { name: `Stop ${RUNNING_NAME}?` });
  await expect(dialog).toBeVisible();
  expect(app.requests.filter((request) => request.method === "POST")).toEqual([]);
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  expect(app.requests.filter((request) => request.method === "POST")).toEqual([]);
  await stop.click();
  await page.getByRole("dialog").getByRole("button", { name: "Stop server", exact: true }).click();
  await expect.poll(() => app.requests.filter((request) => request.method === "POST")).toEqual([
    { method: "POST", path: `/servers/${RUNNING_ID}/stop`, query: {}, body: undefined },
  ]);
  await expect(row.getByRole("button", { name: "Start", exact: true })).toBeVisible();
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
