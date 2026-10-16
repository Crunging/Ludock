import { operationResponseSchema, operationsResponseSchema, updateCapabilityResponseSchema, type Operation } from "@ludock/shared";
import { test, expect, RUNNING_ID } from "./fixtures";

for (const compose of [false, true]) {
  test(`restart to update ${compose ? "alongside Compose" : "without source access"}`, async ({ app, page }, testInfo) => {
    let operations: Operation[] = [];
    let submitted: unknown;
    await page.route(`**/api/v1/servers/${RUNNING_ID}/update-capability`, (route) => route.fulfill({
      json: updateCapabilityResponseSchema.parse({ capability: {
        available: compose, updatesOnStart: true, actionLabel: "Update server", manager: compose ? "compose" : "external",
        ...(compose ? { projectName: "games", serviceName: "minecraft", image: "itzg/minecraft-server" } : { unavailableReason: "No Compose source mount" }),
      } }),
    }));
    await page.route(`**/api/v1/servers/${RUNNING_ID}/operations`, (route) => route.fulfill({
      json: operationsResponseSchema.parse({ operations }),
    }));
    await page.route(`**/api/v1/servers/${RUNNING_ID}/restart-update`, (route) => {
      submitted = route.request().postDataJSON();
      operations = [{ id: "c15cbd1f-dbb6-444d-8b8f-c5d728b94df0", serverId: RUNNING_ID,
        kind: "restart_to_update", status: "running", phase: "backing_up",
        createdAt: Date.now(), updatedAt: Date.now(), error: null, result: null }];
      return route.fulfill({ status: 202, json: operationResponseSchema.parse({ operation: operations[0] }) });
    });
    await app.open(`/servers/${RUNNING_ID}?tab=update`);
    await expect(page.getByText("This image downloads the latest game version when it starts.")).toBeVisible();
    const restart = page.getByRole("button", { name: "Restart to update", exact: true });
    await expect(restart).toBeVisible();
    await expect(restart).toBeDisabled();
    await expect(page.getByRole("button", { name: "Update server", exact: true })).toHaveCount(compose ? 1 : 0);
    const form = page.locator("form").filter({ has: restart });
    await expect(form.getByRole("checkbox", { name: "Take a backup first" })).toBeChecked();
    await form.getByRole("checkbox", { name: /I understand/ }).check();
    await page.screenshot({ path: testInfo.outputPath("update-tab.png"), fullPage: true });
    await restart.click();
    await expect(page.getByRole("tabpanel", { name: "Activity" })).toBeVisible();
    expect(submitted).toEqual({ createBackup: true });
    await page.reload();
    await expect(page.getByRole("link", { name: "restart to update", exact: true })).toBeVisible();
  });
}
