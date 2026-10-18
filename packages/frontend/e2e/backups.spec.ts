import {
  backupPreflightResponseSchema,
  backupSchema,
  backupsResponseSchema,
  operationResponseSchema,
} from "@ludock/shared";
import { test, expect, RUNNING_ID, RUNNING_NAME } from "./fixtures";

test("backup creation rechecks readiness right before asking to confirm downtime", async ({ app, page }) => {
  app.user = { ...app.user!, role: "operator" };
  app.servers[0].permissions = ["server.view", "backups.create"];
  let ready = false;
  let checks = 0;
  let creates = 0;
  await page.route(`**/api/v1/servers/${RUNNING_ID}/backups/preflight`, (route) => {
    checks++;
    return route.fulfill({ json: backupPreflightResponseSchema.parse({
      preflight: { ready, checkedAt: Date.now(), issues: ready ? [] : [
        { code: "BACKUP_DESTINATION", message: "Mount a separate backup destination." },
        { code: "NO_BACKUP_ROOTS", message: "Choose approved mounted data roots for this server." },
      ] },
    }) });
  });
  await page.route(`**/api/v1/servers/${RUNNING_ID}/backups`, (route) => {
    creates++;
    return route.fulfill({ status: 202, json: operationResponseSchema.parse({ operation: {
      id: "c15cbd1f-dbb6-444d-8b8f-c5d728b94df0", serverId: RUNNING_ID,
      kind: "backup", status: "queued", phase: "queued", createdAt: 0, updatedAt: 0, error: null, result: null,
    } }) });
  });
  await app.open(`/servers/${RUNNING_ID}`);
  await page.getByRole("tab", { name: "Backups", exact: true }).click();
  await expect.poll(() => checks).toBe(1);
  await expect(page.getByRole("button", { name: "Create backup", exact: true })).toBeDisabled();
  expect(creates).toBe(0);
  ready = true;
  await page.getByRole("button", { name: "Check again", exact: true }).focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("button", { name: "Create backup", exact: true })).toBeEnabled();
  expect(creates).toBe(0);
  const dialogs: string[] = [];
  page.on("dialog", async (dialog) => {
    dialogs.push(dialog.message());
    expect(checks).toBe(5);
    expect(creates).toBe(0);
    await dialog.accept();
  });
  // The destination disappears after the last check: Create checks again and stops before asking.
  ready = false;
  await page.getByRole("button", { name: "Create backup", exact: true }).click();
  await expect(page.getByText("Mount a separate backup destination.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Create backup", exact: true })).toBeDisabled();
  expect(checks).toBe(3);
  expect(dialogs).toEqual([]);
  ready = true;
  await page.getByRole("button", { name: "Check again", exact: true }).click();
  await page.getByRole("button", { name: "Create backup", exact: true }).click();
  await expect(page.getByRole("tab", { name: "Activity", selected: true })).toBeVisible();
  expect(dialogs).toHaveLength(1);
  expect(dialogs[0]).toContain(RUNNING_NAME);
  expect(creates).toBe(1);
});

test("a restore needs the server's name and waits for other operations to finish", async ({ app, page }) => {
  const backup = backupSchema.parse({
    id: "60c15d4e-b46a-4e57-b52d-fc08ee134b75", serverId: RUNNING_ID, createdAt: Date.UTC(2026, 8, 10, 6),
    roots: [{ id: "data", path: "/data" }], size: 4096, checksum: "a".repeat(64), state: "complete",
  });
  const operation = {
    id: "c15cbd1f-dbb6-444d-8b8f-c5d728b94df0", serverId: RUNNING_ID, kind: "backup",
    status: "running" as const, phase: "copying", createdAt: 0, updatedAt: 0, error: null, result: null,
  };
  const restores: unknown[] = [];
  await page.route(`**/api/v1/servers/${RUNNING_ID}/backups`, (route) =>
    route.fulfill({ json: backupsResponseSchema.parse({ backups: [backup] }) }));
  await page.route(`**/api/v1/servers/${RUNNING_ID}/restores`, async (route) => {
    restores.push(route.request().postDataJSON());
    await route.fulfill({ status: 202, json: operationResponseSchema.parse({ operation: { ...operation, kind: "restore", status: "queued", phase: "queued" } }) });
  });
  const refreshActivity = async () => {
    await page.getByRole("tab", { name: "Activity", exact: true }).click();
    await page.getByRole("button", { name: "Refresh", exact: true }).click();
    await page.getByRole("tab", { name: "Backups", exact: true }).click();
  };
  await app.open(`/servers/${RUNNING_ID}?tab=backups`);
  await page.getByRole("button", { name: "Restore…", exact: true }).click();
  const submit = page.getByRole("button", { name: "Restore game data", exact: true });
  const confirmation = page.getByRole("textbox", { name: /to confirm/ });
  await confirmation.fill("friends survival world");
  await expect(submit).toBeDisabled();
  await confirmation.fill(RUNNING_NAME);
  await expect(submit).toBeEnabled();

  // Another operation starts before the restore is submitted.
  app.operations.push(operation);
  app.servers[0].activeOperation = operation;
  await refreshActivity();
  await expect(submit).toBeDisabled();
  await confirmation.press("Enter");
  app.operations[0] = { ...operation, status: "succeeded", phase: "succeeded" };
  app.servers[0].activeOperation = undefined;
  await refreshActivity();
  await expect(submit).toBeEnabled();
  expect(restores).toEqual([]);

  await submit.click();
  await expect(page.getByText("Restore queued. The server stays stopped until its data is safe.")).toBeVisible();
  await expect(page.getByRole("tab", { name: "Activity", selected: true })).toBeVisible();
  expect(restores).toEqual([{ backupId: backup.id, confirmation: RUNNING_NAME }]);
});
