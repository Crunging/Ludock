import { test, expect, signIn, createUser, serverRow } from "./fixtures.mjs";

test("sharing one server with an operator, then removing access", async ({ session }) => {
  const adminPage = await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
  await createUser(adminPage, "operator1", "operator password one", "operator");

  // A new operator sees nothing until a server is shared.
  const operator = await session();
  await signIn(operator, "operator1", "operator password one");
  await expect(operator.getByText("No servers shared")).toBeVisible();
  await expect(operator.getByRole("link", { name: "Users" })).toHaveCount(0);

  const editor = adminPage.locator("[data-grants]");
  const survival = editor.locator(".grants-server", { hasText: "Survival" });
  await survival.getByRole("button", { name: "Start and stop" }).click();
  await survival.getByLabel("Read logs").check();
  await expect(editor.locator("[data-grant-summary]")).toContainText("1 server selected.");
  await editor.getByRole("button", { name: "Save server access" }).click();
  await expect(editor.locator(".alert--success")).toContainText("Server access saved for operator1.");

  await operator.goto("/");
  await expect(operator.locator(".server-row")).toHaveCount(1);
  await expect(serverRow(operator, "Survival")).toBeVisible();
  await operator.getByRole("link", { name: "Survival", exact: true }).click();
  await expect(operator).toHaveURL(/\/servers\//);
  const survivalURL = operator.url();
  // Start and stop do not include restarting, which the Update tab needs.
  await expect(operator.getByRole("tab")).toHaveText(["Overview", "Activity"]);
  await expect(operator.locator(".detail-actions").getByRole("button", { name: "Stop" })).toBeVisible();
  await expect(operator.locator(".detail-actions").getByRole("link", { name: "Logs" })).toBeVisible();
  await expect(operator.getByRole("button", { name: "Save monitoring" })).toHaveCount(0);

  // Administrator pages and other servers stay out of reach.
  await operator.goto("/users");
  await expect(operator).toHaveURL(/\/$/);
  const hidden = await operator.request.get("/servers/00000000-0000-4000-8000-000000000000");
  expect(hidden.status()).toBe(404);

  // Removing access takes effect immediately.
  await adminPage.locator(".user-row", { hasText: "operator1" }).getByRole("button", { name: "Server access", exact: true }).click();
  await editor.locator(".grants-server", { hasText: "Survival" }).getByRole("button", { name: "Remove access" }).click();
  await editor.getByRole("button", { name: "Save server access" }).click();
  await expect(editor.locator(".alert--success")).toBeVisible();
  await operator.goto("/");
  await expect(operator.getByText("No servers shared with you yet")).toBeVisible();
  await operator.goto(survivalURL);
  await expect(operator.getByText("Server not found")).toBeVisible();

  // Disabling the account signs the operator out.
  await adminPage.locator(".user-row", { hasText: "operator1" }).getByRole("button", { name: "Disable" }).click();
  await expect(adminPage.locator(".user-row", { hasText: "operator1" })).toContainText("Disabled");
  await operator.goto("/");
  await expect(operator).toHaveURL(/\/login/);

  await adminPage.locator(".user-row", { hasText: "operator1" }).getByRole("button", { name: "Delete" }).click();
  await adminPage.getByRole("dialog", { name: "Delete operator1?" }).getByRole("button", { name: "Delete user" }).click();
  await expect(adminPage.locator("#users .alert--success")).toHaveText("Deleted operator1.");
});
